package main

import (
	"crypto/rand"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

type recEvents struct {
	mu   sync.Mutex
	msgs []Msg
	down []string
}

func (r *recEvents) PeerMsg(id string, m Msg) *Msg {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	if m.T == "echo" {
		return &Msg{T: "echo", Name: m.Name}
	}
	return nil
}
func (r *recEvents) PeerDown(id string) { r.mu.Lock(); r.down = append(r.down, id); r.mu.Unlock() }
func (r *recEvents) Changed()           {}
func (r *recEvents) PeerUp(id string)   {}

func testNet(t *testing.T, id string, handler ...LinkEvents) (*Net, *recEvents, string) {
	kp, _ := noise.DH25519.GenerateKeypair(rand.Reader)
	ev := &recEvents{}
	var h LinkEvents = ev
	if len(handler) > 0 {
		h = handler[0]
	}
	n := NewNet(&Identity{ID: id, Name: "m-" + id, Private: kp.Private, Public: kp.Public},
		&PeerStore{path: filepath.Join(t.TempDir(), "peers.json"), peers: map[string]*Peer{}}, 0, h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go n.accept(c)
		}
	}()
	return n, ev, ln.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPairAndSession(t *testing.T) {
	a, _, aAddr := testNet(t, "aaaa")
	b, bEv, bAddr := testNet(t, "bbbb")
	a.disc.saw(beacon{ID: "bbbb", Name: "m-bbbb", OS: "linux", Port: 1}, bAddr)
	b.disc.saw(beacon{ID: "aaaa", Name: "m-aaaa", OS: "linux", Port: 1}, aAddr)

	if err := a.Pair("bbbb"); err != nil {
		t.Fatal(err)
	}
	var pa, pb *Pairing
	waitFor(t, "pairing on both sides", func() bool {
		pa, pb = a.State("").Pairing, b.State("").Pairing
		return pa != nil && pb != nil
	})
	if pa.Code != pb.Code || len(pa.Code) != 7 {
		t.Fatalf("codes differ: %q vs %q", pa.Code, pb.Code)
	}
	a.Answer("bbbb", true)
	b.Answer("aaaa", true)
	waitFor(t, "both paired", func() bool { return a.peers.Get("bbbb") != nil && b.peers.Get("aaaa") != nil })

	// a has the smaller ID, so it dials.
	go a.connect(*a.peers.Get("bbbb"), bAddr)
	waitFor(t, "session", func() bool { return a.Online("bbbb") && b.Online("aaaa") })
	if err := a.Send("bbbb", Msg{T: "down", Slot: "k1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "message delivered", func() bool {
		bEv.mu.Lock()
		defer bEv.mu.Unlock()
		return len(bEv.msgs) == 1 && bEv.msgs[0].Slot == "k1"
	})

	// An unpaired machine that knows b's public key still cannot open a session.
	c, _, _ := testNet(t, "cccc")
	c.peers.Put(Peer{ID: "bbbb", Public: b.id.Public})
	c.connect(*c.peers.Get("bbbb"), bAddr)
	if c.Online("bbbb") {
		t.Fatal("unpaired machine got a session")
	}
	// Nor can it pretend to be a by using a's ID with its own key.
	c.id.ID = "aaaa"
	c.connect(*c.peers.Get("bbbb"), bAddr)
	if c.Online("bbbb") {
		t.Fatal("impersonator got a session")
	}

	a.Unpair("bbbb")
	waitFor(t, "unpaired on both sides", func() bool { return a.peers.Get("bbbb") == nil && b.peers.Get("aaaa") == nil })
	waitFor(t, "b saw the disconnect", func() bool { bEv.mu.Lock(); defer bEv.mu.Unlock(); return len(bEv.down) == 1 })
}

func TestPairRejected(t *testing.T) {
	a, _, aAddr := testNet(t, "aaaa")
	b, _, bAddr := testNet(t, "bbbb")
	a.disc.saw(beacon{ID: "bbbb", Name: "B", Port: 1}, bAddr)
	b.disc.saw(beacon{ID: "aaaa", Name: "A", Port: 1}, aAddr)
	if err := a.Pair("bbbb"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pending", func() bool { return b.State("").Pairing != nil })
	a.Answer("bbbb", true)
	b.Answer("aaaa", false)
	waitFor(t, "both done", func() bool { return a.State("").Pairing == nil && b.State("").Pairing == nil })
	if a.peers.Get("bbbb") != nil || b.peers.Get("aaaa") != nil {
		t.Fatal("rejected pairing was stored")
	}
}

func TestRouting(t *testing.T) {
	dryRun = true
	defer func() { dryRun = false }()
	mk := func(id string, setup func(c *Config)) (*Engine, *Router, *Net, chan BusEvent, string) {
		store := LoadConfig(filepath.Join(t.TempDir(), "c.json"))
		c := *store.Get()
		c.Slots = append([]Slot(nil), c.Slots...)
		c.Layouts = []Layout{newLayout("default", "Default", c.Slots)}
		setup(&c)
		if err := store.Set(&c); err != nil {
			t.Fatal(err)
		}
		bus := NewBus()
		ch, _ := bus.Subscribe()
		e := NewEngine(store, &Emitter{}, bus)
		r := NewRouter(e, nil, bus)
		n, _, addr := testNet(t, id, r)
		r.net, e.router = n, r
		return e, r, n, ch, addr
	}
	slot := func(c *Config, id string) *Slot {
		for i := range c.Slots {
			if c.Slots[i].ID == id {
				return &c.Slots[i]
			}
		}
		return nil
	}
	ea, ra, na, _, _ := mk("aaaa", func(c *Config) {
		slot(c, "k1").Trigger = "KEY_B"
		slot(c, "k2").Trigger = "KEY_G"
		c.Layouts[0].Keys["k2"] = Key{Label: "switch", Action: Action{Type: "machine", Machine: "next"}}
	})
	_, rb, nb, chB, bAddr := mk("bbbb", func(c *Config) {
		c.Layouts[0].Keys["k1"] = Key{Label: "Copy", Action: Action{Type: "keys", Keys: "ctrl+c"}}
	})
	na.peers.Put(Peer{ID: "bbbb", Name: "B", Public: nb.id.Public})
	nb.peers.Put(Peer{ID: "aaaa", Name: "A", Public: na.id.Public})
	go na.connect(*na.peers.Get("bbbb"), bAddr)
	waitFor(t, "session", func() bool { return na.Online("bbbb") && nb.Online("aaaa") })

	ea.handle(k(34, 1)) // G: "next machine"
	ea.handle(k(34, 0))
	if ra.Target() != "bbbb" {
		t.Fatalf("target = %q", ra.Target())
	}
	waitFor(t, "B told it's controlled", func() bool { s := rb.Seen(); return s != nil && s.Here && s.HostID == "aaaa" })
	ea.handle(k(48, 1)) // B: k1, should run on B with B's layout
	ea.handle(k(48, 0))
	var got BusEvent
	waitFor(t, "press on B", func() bool {
		select {
		case ev := <-chB:
			if ev.Type == "trigger" {
				got = ev
				return true
			}
		default:
		}
		return false
	})
	if got.Binding != "k1" || got.From != "m-aaaa" {
		t.Fatalf("B got %+v", got)
	}

	nb.mu.Lock()
	nb.conns["aaaa"].sc.c.Close() // B goes away
	nb.mu.Unlock()
	waitFor(t, "fallback to host", func() bool { return ra.Target() == "" })
}

func TestBigMessageAndRequest(t *testing.T) {
	a, _, _ := testNet(t, "aaaa")
	b, _, bAddr := testNet(t, "bbbb")
	a.peers.Put(Peer{ID: "bbbb", Public: b.id.Public})
	b.peers.Put(Peer{ID: "aaaa", Public: a.id.Public})
	go a.connect(*a.peers.Get("bbbb"), bAddr)
	waitFor(t, "session", func() bool { return a.Online("bbbb") && b.Online("aaaa") })
	big := strings.Repeat("x", 200000) // several frames
	r, err := a.Request("bbbb", Msg{T: "echo", Name: big}, 5*time.Second)
	if err != nil || r.Name != big {
		t.Fatalf("echo failed: %v (len %d)", err, len(r.Name))
	}
}

func TestImportLayouts(t *testing.T) {
	src := []Layout{{ID: "x", Name: "Work", Keys: map[string]Key{
		"k1": {Label: "Desk ←", Action: Action{Type: "keys", Keys: "super+pageup"}},
		"k2": {Label: "Copy", Action: Action{Type: "keys", Keys: "ctrl+c"}},
		"k3": {Label: "Browser", Action: Action{Type: "app", App: "msedge.lnk", AppName: "Firefox"}},
		"k4": {Label: "Notes", Action: Action{Type: "app", App: "x.lnk", AppName: "Notepad++"}},
		"k5": {Label: "Script", Action: Action{Type: "command", Command: "start calc"}},
		"k6": {Label: "Next", Action: Action{Type: "layout", Layout: "x"}},
	}}}
	apps := []App{{ID: "firefox_firefox.desktop", Name: "Firefox"}}
	out, rep := importLayouts(src, "windows", "PC", []Layout{{Name: "Work (PC)"}}, apps)
	l := out[0]
	if l.Name != "Work (PC) 2" || l.ID == "x" {
		t.Fatalf("name/id: %q %q", l.Name, l.ID)
	}
	if got := l.Keys["k1"].Action.Keys; got != "super+pageup" {
		t.Errorf("k1 = %q (windows ctrl+super+left was not given, so it should stay)", got)
	}
	if got := l.Keys["k3"].Action; got.App != "firefox_firefox.desktop" {
		t.Errorf("app not matched: %+v", got)
	}
	if k := l.Keys["k4"]; k.Action.Type != "none" || k.Note == "" {
		t.Errorf("unknown app not flagged: %+v", k)
	}
	if k := l.Keys["k5"]; k.Action.Type != "none" || !strings.Contains(k.Note, "start calc") {
		t.Errorf("command not flagged: %+v", k)
	}
	if got := l.Keys["k6"].Action.Layout; got != l.ID {
		t.Errorf("layout link not remapped: %q", got)
	}
	if rep.Attention != 2 || rep.Layouts != 1 {
		t.Errorf("report %+v", rep)
	}
	if c, ok := convertShortcut("ctrl+super+left", "windows", "linux"); !ok || c != "super+pageup" {
		t.Errorf("convert = %q %v", c, ok)
	}
}

func init() { quietDesktop = true } // never pop notifications during tests
