package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
)

// Paired machines talk over TCP, encrypted with the Noise protocol:
//   - pairing uses Noise XX (each side learns the other's key) and both screens
//     show a 6-digit code taken from the handshake; the user confirms they match,
//     which rules out anyone in the middle;
//   - after that, every connection uses Noise KK: both sides must prove they hold
//     the key the other remembered, so nothing unpaired can connect.
// Frames are a 2-byte length plus a Noise message (max 65535 bytes).

const (
	linkVersion  = 1
	pairTimeout  = 2 * time.Minute
	pingEvery    = 5 * time.Second
	readDeadline = 16 * time.Second
)

// Msg is everything sent between machines after the handshake.
type Msg struct {
	T       string   `json:"t"`            // hello, ping, confirm, down, up, release_all, active, unpair, get_layouts, layouts
	ID      int      `json:"id,omitempty"` // request ID; the reply carries it as Re
	Re      int      `json:"re,omitempty"`
	Err     string   `json:"err,omitempty"`
	Slot    string   `json:"slot,omitempty"`
	On      bool     `json:"on,omitempty"`
	OK      bool     `json:"ok,omitempty"`
	Name    string   `json:"name,omitempty"`
	OS      string   `json:"os,omitempty"`
	Layouts []Layout `json:"layouts,omitempty"`
	// "picker": the machine list the host shows while the switch key is held
	IDs   []string `json:"ids,omitempty"`
	Names []string `json:"names,omitempty"`
	Idx   int      `json:"idx,omitempty"`
	Title string   `json:"title,omitempty"`
}

// opening is sent in clear before the handshake and bound into it as the Noise
// prologue, so it cannot be altered without the handshake failing.
type opening struct {
	V    int    `json:"v"`
	Mode string `json:"mode"` // "pair" | "session"
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
}

func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 65535 {
		return errors.New("frame too large")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	_, err := w.Write(append(hdr[:], b...))
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

type secConn struct {
	c          net.Conn
	send, recv *noise.CipherState
	wmu        sync.Mutex
}

// Messages larger than one frame (e.g. a set of layouts) are split: every
// encrypted chunk starts with 1 if more chunks follow, 0 for the last one.
const (
	chunkSize = 60000
	maxMsg    = 4 << 20
)

func (s *secConn) Send(m Msg) error {
	b, _ := json.Marshal(m)
	if len(b) > maxMsg {
		return errors.New("message too large")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	for {
		n := min(len(b), chunkSize)
		flag := byte(0)
		if n < len(b) {
			flag = 1
		}
		ct, err := s.send.Encrypt(nil, nil, append([]byte{flag}, b[:n]...))
		if err != nil {
			return err
		}
		if err := writeFrame(s.c, ct); err != nil {
			return err
		}
		b = b[n:]
		if flag == 0 {
			return nil
		}
	}
}

func (s *secConn) Recv() (Msg, error) {
	var whole []byte
	for {
		s.c.SetReadDeadline(time.Now().Add(readDeadline))
		f, err := readFrame(s.c)
		if err != nil {
			return Msg{}, err
		}
		pt, err := s.recv.Decrypt(nil, nil, f)
		if err != nil {
			return Msg{}, err
		}
		if len(pt) == 0 || len(whole)+len(pt) > maxMsg {
			return Msg{}, errors.New("bad message")
		}
		whole = append(whole, pt[1:]...)
		if pt[0] == 0 {
			break
		}
	}
	var m Msg
	return m, json.Unmarshal(whole, &m)
}

// handshake runs a Noise handshake of n messages and returns the secured connection.
func handshake(c net.Conn, hs *noise.HandshakeState, initiator bool, n int) (*secConn, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	var cs1, cs2 *noise.CipherState
	for i := 0; i < n; i++ {
		var err error
		if (i%2 == 0) == initiator {
			var out []byte
			if out, cs1, cs2, err = hs.WriteMessage(nil, nil); err == nil {
				err = writeFrame(c, out)
			}
		} else {
			var in []byte
			if in, err = readFrame(c); err == nil {
				_, cs1, cs2, err = hs.ReadMessage(nil, in)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if cs1 == nil {
		return nil, errors.New("handshake incomplete")
	}
	if initiator {
		return &secConn{c: c, send: cs1, recv: cs2}, nil
	}
	return &secConn{c: c, send: cs2, recv: cs1}, nil
}

// pairCode turns the handshake hash into the 6-digit code both screens show.
func pairCode(h []byte) string {
	return fmt.Sprintf("%03d %03d", binary.BigEndian.Uint32(h[:4])%1000, binary.BigEndian.Uint32(h[4:8])%1000)
}

// ---------- the manager ----------

// Pairing is a pending pairing shown in both panels.
type Pairing struct {
	PeerID   string `json:"peer"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Code     string `json:"code"`
	Outgoing bool   `json:"outgoing"` // we asked
	Mine     string `json:"mine"`     // "", "yes", "no"
	Theirs   string `json:"theirs"`

	conn   *secConn
	public []byte
	addr   string
	answer chan bool
}

type peerConn struct {
	sc   *secConn
	name string
	os   string
}

type LinkEvents interface {
	PeerMsg(peerID string, m Msg) *Msg // optional reply to a request
	PeerDown(peerID string)
	PeerUp(peerID string) // a session just opened
	Changed()             // anything shown in the Machines view changed
}

type Net struct {
	id    *Identity
	peers *PeerStore
	disc  *Discovery
	port  int
	ev    LinkEvents

	mu      sync.Mutex
	conns   map[string]*peerConn
	pairing *Pairing // at most one at a time

	reqMu   sync.Mutex
	nextReq int
	pending map[int]chan Msg
}

func NewNet(id *Identity, peers *PeerStore, port int, ev LinkEvents) *Net {
	n := &Net{id: id, peers: peers, port: port, ev: ev, conns: map[string]*peerConn{}, pending: map[int]chan Msg{}}
	n.disc = &Discovery{
		self: func() beacon {
			return beacon{App: "keybar", V: linkVersion, ID: id.ID, Name: id.Name, OS: hostOS(), Port: port}
		},
		onChange: ev.Changed,
		seen:     map[string]*Seen{},
	}
	return n
}

func (n *Net) Run() {
	go n.disc.Run()
	go n.dialLoop()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", n.port))
	if err != nil {
		log.Printf("network: cannot listen on port %d: %v — other machines can't connect to this one", n.port, err)
		return
	}
	log.Printf("network: %s (%s) listening on :%d", n.id.Name, n.id.ID, n.port)
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		go n.accept(c)
	}
}

func (n *Net) accept(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	pro, err := readFrame(c)
	var op opening
	if err != nil || len(pro) > 1024 || json.Unmarshal(pro, &op) != nil || op.V != linkVersion || op.ID == "" || op.ID == n.id.ID {
		c.Close()
		return
	}
	op.Name, op.OS = clip(op.Name, 64), clip(op.OS, 16)
	switch op.Mode {
	case "session":
		p := n.peers.Get(op.ID)
		if p == nil {
			c.Close() // not paired: say nothing more
			return
		}
		hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noiseSuite, Pattern: noise.HandshakeKK, Initiator: false,
			Prologue: pro, StaticKeypair: n.id.Keypair(), PeerStatic: p.Public, Random: rand.Reader})
		if err != nil {
			c.Close()
			return
		}
		sc, err := handshake(c, hs, false, 2)
		if err != nil {
			log.Printf("network: session from %s refused: %v", op.Name, err)
			c.Close()
			return
		}
		n.serve(op.ID, sc, c.RemoteAddr().String())
	case "pair":
		n.mu.Lock()
		busy := n.pairing != nil
		n.mu.Unlock()
		if busy {
			c.Close()
			return
		}
		hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noiseSuite, Pattern: noise.HandshakeXX, Initiator: false,
			Prologue: pro, StaticKeypair: n.id.Keypair(), Random: rand.Reader})
		if err != nil {
			c.Close()
			return
		}
		sc, err := handshake(c, hs, false, 3)
		if err != nil {
			c.Close()
			return
		}
		n.runPairing(&Pairing{PeerID: op.ID, Name: op.Name, OS: op.OS, Code: pairCode(hs.ChannelBinding()),
			conn: sc, public: hs.PeerStatic(), addr: c.RemoteAddr().String()})
	default:
		c.Close()
	}
}

func (n *Net) dial(addr, mode string) (net.Conn, []byte, error) {
	d := net.Dialer{Timeout: 4 * time.Second}
	c, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	pro, _ := json.Marshal(opening{V: linkVersion, Mode: mode, ID: n.id.ID, Name: n.id.Name, OS: hostOS()})
	if err := writeFrame(c, pro); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, pro, nil
}

// Pair starts pairing with a machine seen on the network.
func (n *Net) Pair(peerID string) error {
	s := n.disc.Get(peerID)
	if s == nil {
		return errors.New("that machine is no longer visible on the network")
	}
	n.mu.Lock()
	busy := n.pairing != nil
	n.mu.Unlock()
	if busy {
		return errors.New("another pairing is in progress")
	}
	c, pro, err := n.dial(s.Addr, "pair")
	if err != nil {
		return fmt.Errorf("cannot reach %s: %v", s.Name, err)
	}
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noiseSuite, Pattern: noise.HandshakeXX, Initiator: true,
		Prologue: pro, StaticKeypair: n.id.Keypair(), Random: rand.Reader})
	if err != nil {
		c.Close()
		return err
	}
	sc, err := handshake(c, hs, true, 3)
	if err != nil {
		c.Close()
		return fmt.Errorf("%s refused pairing (it may be busy with another one)", s.Name)
	}
	go n.runPairing(&Pairing{PeerID: peerID, Name: s.Name, OS: s.OS, Code: pairCode(hs.ChannelBinding()), Outgoing: true,
		conn: sc, public: hs.PeerStatic(), addr: s.Addr})
	return nil
}

// runPairing waits until both people confirmed the code (or one rejected, or time ran out).
func (n *Net) runPairing(p *Pairing) {
	p.answer = make(chan bool, 1)
	n.mu.Lock()
	if n.pairing != nil {
		n.mu.Unlock()
		p.conn.c.Close()
		return
	}
	n.pairing = p
	n.mu.Unlock()
	n.ev.Changed()
	if !p.Outgoing {
		notify(fmt.Sprintf("Pairing request from %s — code %s. Open Keybar to confirm.", p.Name, p.Code))
	}
	theirs := make(chan bool, 1)
	go func() {
		for {
			m, err := p.conn.Recv()
			if err != nil {
				close(theirs)
				return
			}
			if m.T == "confirm" {
				theirs <- m.OK
				return
			}
		}
	}()
	ok := func() bool {
		timeout := time.After(pairTimeout)
		mine, got := false, false
		for !(mine && got) {
			select {
			case yes := <-p.answer:
				n.set(func() { p.Mine = yesNo(yes) })
				p.conn.Send(Msg{T: "confirm", OK: yes})
				if !yes {
					return false
				}
				mine = true
			case yes, alive := <-theirs:
				if !alive || !yes {
					n.set(func() { p.Theirs = "no" })
					return false
				}
				n.set(func() { p.Theirs = "yes" })
				got, theirs = true, nil
			case <-timeout:
				return false
			}
		}
		return true
	}()
	time.Sleep(300 * time.Millisecond) // let our confirm reach them before closing
	p.conn.c.Close()
	if ok {
		n.peers.Put(Peer{ID: p.PeerID, Name: p.Name, OS: p.OS, Public: p.public, Addr: n.peerAddr(p)})
		log.Printf("network: paired with %s (%s)", p.Name, p.PeerID)
		notify("Paired with " + p.Name)
	} else {
		log.Printf("network: pairing with %s cancelled", p.Name)
	}
	n.mu.Lock()
	n.pairing = nil
	n.mu.Unlock()
	n.ev.Changed()
}

// peerAddr is where the peer's listener is: the beacon's address, or (incoming
// pairing without a beacon yet) the connection's IP — the port is filled later.
func (n *Net) peerAddr(p *Pairing) string {
	if s := n.disc.Get(p.PeerID); s != nil {
		return s.Addr
	}
	if p.Outgoing {
		return p.addr
	}
	return ""
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (n *Net) set(fn func()) {
	n.mu.Lock()
	fn()
	n.mu.Unlock()
	n.ev.Changed()
}

// Answer is the local person's decision on the pending pairing.
func (n *Net) Answer(peerID string, ok bool) error {
	n.mu.Lock()
	p := n.pairing
	n.mu.Unlock()
	if p == nil || p.PeerID != peerID {
		return errors.New("no such pairing")
	}
	select {
	case p.answer <- ok:
	default:
	}
	return nil
}

// Unpair forgets a machine on both sides (the other side only if it's connected now).
func (n *Net) Unpair(peerID string) {
	n.mu.Lock()
	pc := n.conns[peerID]
	n.mu.Unlock()
	if pc != nil {
		pc.sc.Send(Msg{T: "unpair"})
		time.Sleep(100 * time.Millisecond)
	}
	n.peers.Delete(peerID)
	if pc != nil {
		pc.sc.c.Close()
	}
	n.ev.Changed()
}

// dialLoop keeps a session open to every paired machine. To avoid two
// connections per pair, only the side with the smaller ID dials.
func (n *Net) dialLoop() {
	for {
		for _, p := range n.peers.List() {
			if n.id.ID > p.ID || n.Online(p.ID) {
				continue
			}
			addr := p.Addr
			if s := n.disc.Get(p.ID); s != nil {
				addr = s.Addr
			}
			if addr == "" {
				continue
			}
			go n.connect(p, addr)
		}
		time.Sleep(3 * time.Second)
	}
}

var connecting sync.Map

func (n *Net) connect(p Peer, addr string) {
	if _, busy := connecting.LoadOrStore(p.ID, true); busy {
		return
	}
	defer connecting.Delete(p.ID)
	c, pro, err := n.dial(addr, "session")
	if err != nil {
		return
	}
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noiseSuite, Pattern: noise.HandshakeKK, Initiator: true,
		Prologue: pro, StaticKeypair: n.id.Keypair(), PeerStatic: p.Public, Random: rand.Reader})
	if err != nil {
		c.Close()
		return
	}
	sc, err := handshake(c, hs, true, 2)
	if err != nil {
		log.Printf("network: %s refused the session (unpaired there?): %v", p.Name, err)
		c.Close()
		return
	}
	n.peers.Update(p.ID, func(pp *Peer) { pp.Addr = addr })
	go n.serve(p.ID, sc, addr)
}

func (n *Net) serve(peerID string, sc *secConn, addr string) {
	pc := &peerConn{sc: sc}
	n.mu.Lock()
	old := n.conns[peerID]
	n.conns[peerID] = pc
	n.mu.Unlock()
	if old != nil {
		old.sc.c.Close()
	}
	sc.Send(Msg{T: "hello", Name: n.id.Name, OS: hostOS()})
	log.Printf("network: connected to %s", addr)
	n.ev.PeerUp(peerID)
	n.ev.Changed()

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if sc.Send(Msg{T: "ping"}) != nil {
					sc.c.Close()
					return
				}
			}
		}
	}()
	for {
		m, err := sc.Recv()
		if err != nil {
			break
		}
		switch m.T {
		case "ping":
		case "hello":
			n.mu.Lock()
			pc.name, pc.os = clip(m.Name, 64), clip(m.OS, 16)
			n.mu.Unlock()
			n.peers.Update(peerID, func(p *Peer) { p.Name, p.OS = clip(m.Name, 64), clip(m.OS, 16) })
			n.ev.Changed()
		case "unpair":
			n.peers.Delete(peerID)
			notify("Keybar: unpaired by the other machine")
			sc.c.Close()
		default:
			if m.Re != 0 {
				n.reqMu.Lock()
				ch := n.pending[m.Re]
				n.reqMu.Unlock()
				if ch != nil {
					ch <- m
				}
				continue
			}
			if reply := n.ev.PeerMsg(peerID, m); reply != nil && m.ID != 0 {
				reply.Re = m.ID
				sc.Send(*reply)
			}
		}
	}
	close(stop)
	sc.c.Close()
	n.mu.Lock()
	gone := n.conns[peerID] == pc
	if gone {
		delete(n.conns, peerID)
	}
	n.mu.Unlock()
	if gone {
		log.Printf("network: disconnected from %s", addr)
		n.ev.PeerDown(peerID)
		n.ev.Changed()
	}
}

func (n *Net) Online(peerID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conns[peerID] != nil
}

// Send delivers a message to a connected peer.
func (n *Net) Send(peerID string, m Msg) error {
	n.mu.Lock()
	pc := n.conns[peerID]
	n.mu.Unlock()
	if pc == nil {
		return errors.New("not connected")
	}
	err := pc.sc.Send(m)
	if err != nil {
		pc.sc.c.Close()
	}
	return err
}

// Request sends a message and waits for the reply.
func (n *Net) Request(peerID string, m Msg, timeout time.Duration) (Msg, error) {
	ch := make(chan Msg, 1)
	n.reqMu.Lock()
	n.nextReq++
	m.ID = n.nextReq
	n.pending[m.ID] = ch
	n.reqMu.Unlock()
	defer func() { n.reqMu.Lock(); delete(n.pending, m.ID); n.reqMu.Unlock() }()
	if err := n.Send(peerID, m); err != nil {
		return Msg{}, err
	}
	select {
	case r := <-ch:
		if r.Err != "" {
			return r, errors.New(r.Err)
		}
		return r, nil
	case <-time.After(timeout):
		return Msg{}, errors.New("the other machine did not answer")
	}
}

// SetName renames this machine (shown to the others).
func (n *Net) SetName(name string) error {
	n.mu.Lock()
	n.id.Name = name
	n.mu.Unlock()
	if err := n.id.save(); err != nil {
		return err
	}
	n.mu.Lock()
	conns := make([]*peerConn, 0, len(n.conns))
	for _, pc := range n.conns {
		conns = append(conns, pc)
	}
	n.mu.Unlock()
	for _, pc := range conns {
		pc.sc.Send(Msg{T: "hello", Name: name, OS: hostOS()})
	}
	return nil
}

// NetState is what the panel's Machines view shows.
type NetState struct {
	Me      MachineInfo   `json:"me"`
	Target  string        `json:"target"` // "" = this machine
	Paired  []MachineInfo `json:"paired"`
	Nearby  []MachineInfo `json:"nearby"` // seen but not paired
	Pairing *Pairing      `json:"pairing,omitempty"`
	Remote  *RemoteTarget `json:"remote,omitempty"` // another machine's Keydial, as it last told us
}

type MachineInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Addr   string `json:"addr,omitempty"`
	Online bool   `json:"online"`
}

func (n *Net) State(target string) NetState {
	st := NetState{Me: MachineInfo{ID: n.id.ID, Name: n.id.Name, OS: hostOS(), Online: true}, Target: target,
		Paired: []MachineInfo{}, Nearby: []MachineInfo{}}
	paired := map[string]bool{}
	for _, p := range n.peers.List() {
		paired[p.ID] = true
		st.Paired = append(st.Paired, MachineInfo{ID: p.ID, Name: p.Name, OS: p.OS, Addr: p.Addr, Online: n.Online(p.ID)})
	}
	for _, s := range n.disc.List() {
		if !paired[s.ID] {
			st.Nearby = append(st.Nearby, MachineInfo{ID: s.ID, Name: s.Name, OS: s.OS, Addr: s.Addr, Online: true})
		}
	}
	n.mu.Lock()
	if n.pairing != nil {
		cp := *n.pairing
		st.Pairing = &cp
	}
	n.mu.Unlock()
	return st
}
