package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// Discovery: every copy of Keybar announces itself on a LAN multicast group
// and lists the others it hears. Beacons carry no secrets; trust comes only
// from pairing.

var beaconGroup = &net.UDPAddr{IP: net.IPv4(239, 255, 47, 83), Port: 47832}

type beacon struct {
	App  string `json:"app"` // "keybar"
	V    int    `json:"v"`
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
	Port int    `json:"port"`
}

// Seen is a machine heard on the network.
type Seen struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	OS       string    `json:"os"`
	Addr     string    `json:"addr"` // host:port of its peer listener
	LastSeen time.Time `json:"-"`
}

type Discovery struct {
	self     func() beacon
	onChange func()

	mu   sync.Mutex
	seen map[string]*Seen
}

func (d *Discovery) List() []Seen {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Seen{}
	for _, s := range d.seen {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (d *Discovery) Get(id string) *Seen {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.seen[id]; s != nil {
		cp := *s
		return &cp
	}
	return nil
}

func (d *Discovery) Run() {
	lc := net.ListenConfig{Control: reuseAddr}
	var pc net.PacketConn
	for {
		var err error
		if pc, err = lc.ListenPacket(context.Background(), "udp4", ":47832"); err == nil {
			break
		}
		log.Printf("discovery: %v (retrying)", err)
		time.Sleep(10 * time.Second)
	}
	p := ipv4.NewPacketConn(pc)
	p.SetMulticastLoopback(true) // lets a second copy on this computer hear us (test mode)
	p.SetMulticastTTL(1)
	joined := map[string]bool{}
	join := func() []net.Interface {
		var out []net.Interface
		ifs, _ := net.Interfaces()
		for _, ifc := range ifs {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
				continue
			}
			if !joined[ifc.Name] && p.JoinGroup(&ifc, beaconGroup) == nil {
				joined[ifc.Name] = true
			}
			out = append(out, ifc)
		}
		return out
	}
	go func() {
		for {
			ifs := join() // interfaces come and go (Wi-Fi, VPN)
			b, _ := json.Marshal(d.self())
			for _, ifc := range ifs {
				if p.SetMulticastInterface(&ifc) == nil {
					pc.WriteTo(b, beaconGroup)
				}
			}
			d.expire()
			time.Sleep(2 * time.Second)
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			log.Printf("discovery read: %v", err)
			time.Sleep(time.Second)
			continue
		}
		var b beacon
		if json.Unmarshal(buf[:n], &b) != nil || b.App != "keybar" || b.ID == "" || b.Port <= 0 {
			continue
		}
		if b.ID == d.self().ID {
			continue
		}
		ua, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		d.saw(b, net.JoinHostPort(ua.IP.String(), itoa(b.Port)))
	}
}

func (d *Discovery) saw(b beacon, addr string) {
	d.mu.Lock()
	s := d.seen[b.ID]
	changed := s == nil || s.Name != b.Name || s.Addr != addr
	if s == nil {
		s = &Seen{ID: b.ID}
		d.seen[b.ID] = s
	}
	s.Name, s.OS, s.Addr, s.LastSeen = clip(b.Name, 64), clip(b.OS, 16), addr, time.Now()
	d.mu.Unlock()
	if changed && d.onChange != nil {
		d.onChange()
	}
}

func (d *Discovery) expire() {
	d.mu.Lock()
	changed := false
	for id, s := range d.seen {
		if time.Since(s.LastSeen) > 9*time.Second {
			delete(d.seen, id)
			changed = true
		}
	}
	d.mu.Unlock()
	if changed && d.onChange != nil {
		d.onChange()
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }
