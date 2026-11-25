package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
)

// Router decides which machine a Keydial press goes to. The machine the device
// is plugged into (the host) reads the press and either runs it with its own
// active layout or sends the slot ID to the active paired machine, which runs
// it with *its* active layout. "machine" actions are always handled by the
// host, so the switch button works whatever the other side's layout says.

type Router struct {
	e   *Engine
	net *Net
	bus *Bus

	mu     sync.Mutex
	target string // "" = this machine
	prev   string

	picker *picker // only touched from the engine goroutine

	rmu    sync.Mutex
	remote map[string]func() // "peer/slot" → release, for presses sent to us

	// Where another machine's Keydial is pointed, as that machine last told us.
	smu  sync.Mutex
	seen *RemoteTarget
}

// RemoteTarget is shown on machines that receive from another machine's Keydial.
type RemoteTarget struct {
	HostID string `json:"host_id"`
	Host   string `json:"host"`   // machine the Keydial is plugged into
	Target string `json:"target"` // machine it controls now
	Here   bool   `json:"here"`   // that's this machine
}

func (r *Router) Seen() *RemoteTarget {
	r.smu.Lock()
	defer r.smu.Unlock()
	if r.seen == nil {
		return nil
	}
	cp := *r.seen
	return &cp
}

type picker struct {
	list  []string // machine IDs, "" = this machine
	idx   int
	moved bool
}

func NewRouter(e *Engine, n *Net, bus *Bus) *Router {
	return &Router{e: e, net: n, bus: bus, remote: map[string]func(){}}
}

func (r *Router) Target() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target
}

// route handles a press of a linked slot on the host. Engine goroutine only.
func (r *Router) route(slotID string, passDown, passUp func()) *activeTrigger {
	key, _ := r.e.store.ActiveKey(slotID)
	if key.Action.Type == "machine" {
		return r.machineAction(key.Action.Machine)
	}
	if p := r.picker; p != nil {
		switch slotID {
		case "dial_cw":
			r.move(1)
			return &activeTrigger{}
		case "dial_ccw":
			r.move(-1)
			return &activeTrigger{}
		}
	}
	if t := r.Target(); t != "" {
		if r.net.Send(t, Msg{T: "down", Slot: slotID}) == nil {
			return &activeTrigger{release: func() { r.net.Send(t, Msg{T: "up", Slot: slotID}) }}
		}
		r.SetTarget("", "connection lost") // peer gone: fall back to this machine
	}
	return r.e.runSlot(slotID, passDown, passUp)
}

func (r *Router) machines() []string {
	list := []string{""}
	if r.net == nil {
		return list
	}
	for _, p := range r.net.peers.List() {
		if r.net.Online(p.ID) {
			list = append(list, p.ID)
		}
	}
	return list
}

func (r *Router) name(id string) string {
	if id == "" {
		if r.net != nil {
			return r.net.id.Name + " (this machine)"
		}
		return "this machine"
	}
	if r.net != nil {
		if p := r.net.peers.Get(id); p != nil {
			return p.Name
		}
	}
	return id
}

func (r *Router) machineAction(what string) *activeTrigger {
	list := r.machines()
	cur := r.Target()
	idx := 0
	for i, id := range list {
		if id == cur {
			idx = i
		}
	}
	switch what {
	case "next":
		r.SetTarget(list[(idx+1)%len(list)], "")
		return &activeTrigger{}
	case "", "pick":
		r.picker = &picker{list: list, idx: idx}
		r.showPicker()
		return &activeTrigger{release: func() {
			p := r.picker
			r.picker = nil
			if p == nil {
				return
			}
			r.endPicker()
			to := p.list[p.idx]
			if !p.moved { // a quick tap goes back to the previous machine, like Alt+Tab
				to = r.prevOr(p.list)
			}
			if to == r.Target() {
				return
			}
			r.SetTarget(to, "")
		}}
	case "self":
		r.SetTarget("", "")
	default:
		r.SetTarget(what, "")
	}
	return &activeTrigger{}
}

func (r *Router) prevOr(list []string) string {
	r.mu.Lock()
	prev, cur := r.prev, r.target
	r.mu.Unlock()
	for _, id := range list {
		if id == prev && id != cur {
			return id
		}
	}
	for _, id := range list { // no previous: the first other machine
		if id != cur {
			return id
		}
	}
	return cur
}

func (r *Router) move(d int) {
	p := r.picker
	n := len(p.list)
	p.idx = ((p.idx+d)%n + n) % n
	p.moved = true
	r.showPicker()
}

const pickerTitle = "Keydial → turn the dial, release to switch"

// showPicker shows the machine list here and on the machine being controlled
// (that's the screen the person is looking at).
func (r *Router) showPicker() {
	p := r.picker
	names := make([]string, len(p.list))
	ids := make([]string, len(p.list))
	for i, id := range p.list {
		names[i] = r.name(id)
		ids[i] = id
		if id == "" && r.net != nil {
			ids[i] = r.net.id.ID
			names[i] = r.net.id.Name
		}
	}
	lines := append([]string(nil), names...)
	lines[0] = r.name("") // "<name> (this machine)"
	if len(lines) == 1 {
		lines = append(lines, "(no other machine connected)")
	}
	overlayShow(pickerTitle, lines, p.idx, true)
	if t := r.Target(); t != "" {
		r.net.Send(t, Msg{T: "picker", On: true, IDs: ids, Names: names, Idx: p.idx})
	}
}

func (r *Router) endPicker() {
	overlayHide()
	if t := r.Target(); t != "" {
		r.net.Send(t, Msg{T: "picker", On: false})
	}
}

// SetTarget makes a machine the one the Keydial controls.
func (r *Router) SetTarget(to, why string) error {
	if to != "" && (r.net == nil || !r.net.Online(to)) {
		return fmt.Errorf("%s is not connected", r.name(to))
	}
	r.mu.Lock()
	from := r.target
	if from == to {
		r.mu.Unlock()
		return nil
	}
	r.prev, r.target = from, to
	r.mu.Unlock()
	if from != "" && r.net != nil {
		r.net.Send(from, Msg{T: "release_all"})
	}
	r.announce("")
	msg := "Keydial → " + r.name(to)
	if why != "" {
		msg += " (" + r.name(from) + ": " + why + ")"
	}
	log.Print(msg)
	overlayShow(msg, nil, -1, false)
	r.bus.Publish(BusEvent{Type: "net"})
	return nil
}

// ---------- the receiving side ----------

func (r *Router) PeerMsg(peerID string, m Msg) *Msg {
	switch m.T {
	case "get_layouts":
		c := r.e.store.Get()
		return &Msg{T: "layouts", OS: hostOS(), Layouts: c.Layouts}
	case "down":
		key := peerID + "/" + m.Slot
		r.bus.Publish(BusEvent{Type: "trigger", Binding: m.Slot, From: r.name(peerID), Layout: r.e.store.Get().ActiveLayout})
		at := r.e.runSlot(m.Slot, func() {}, func() {}) // factory keys only exist on the host
		r.rmu.Lock()
		old := r.remote[key]
		r.remote[key] = nil
		if at.release != nil {
			r.remote[key] = at.release
		}
		r.rmu.Unlock()
		if old != nil {
			old()
		}
	case "up":
		r.rmu.Lock()
		rel := r.remote[peerID+"/"+m.Slot]
		delete(r.remote, peerID+"/"+m.Slot)
		r.rmu.Unlock()
		if rel != nil {
			rel()
		}
	case "release_all":
		r.releaseFrom(peerID)
	case "target":
		if len(m.IDs) != 1 || r.net == nil {
			break
		}
		here := m.IDs[0] == r.net.id.ID
		log.Printf("from %s: Keydial target is now %q (this machine: %v)", r.name(peerID), m.Name, here)
		target := clip(m.Name, 64)
		if here {
			target = r.net.id.Name
		}
		r.smu.Lock()
		prev := r.seen
		r.seen = &RemoteTarget{HostID: peerID, Host: r.name(peerID), Target: target, Here: here}
		r.smu.Unlock()
		r.bus.Publish(BusEvent{Type: "net"})
		if prev != nil && prev.HostID == peerID && prev.Target == target && prev.Here == here {
			break // nothing changed (e.g. reconnect)
		}
		switch {
		case here:
			overlayShow("Keydial → this machine", []string{"from " + r.name(peerID)}, -1, false)
		case prev != nil && prev.Here:
			overlayShow("Keydial → "+target, []string{"no longer controlling this machine"}, -1, false)
		case prev != nil:
			overlayShow("Keydial → "+target, []string{"Keydial on " + r.name(peerID)}, -1, false)
		}
	case "picker":
		log.Printf("from %s: picker on=%v idx=%d %v", r.name(peerID), m.On, m.Idx, m.Names)
		if !m.On || len(m.Names) == 0 || len(m.Names) > 32 {
			overlayHide()
			break
		}
		lines := make([]string, len(m.Names))
		for i, n := range m.Names {
			lines[i] = clip(n, 64)
			if i < len(m.IDs) && r.net != nil && m.IDs[i] == r.net.id.ID {
				lines[i] += " (this machine)"
			}
		}
		overlayShow(pickerTitle, lines, m.Idx, true)
	}
	return nil
}

func (r *Router) releaseFrom(peerID string) {
	r.rmu.Lock()
	var rels []func()
	for k, rel := range r.remote {
		if strings.HasPrefix(k, peerID+"/") {
			delete(r.remote, k)
			if rel != nil {
				rels = append(rels, rel)
			}
		}
	}
	r.rmu.Unlock()
	for _, rel := range rels {
		rel()
	}
}

// PeerDown: never leave keys held, and take the Keydial back if it was over there.
func (r *Router) PeerDown(peerID string) {
	r.releaseFrom(peerID)
	if r.Target() == peerID {
		r.SetTarget("", "disconnected")
	}
	r.smu.Lock()
	s := r.seen
	if s != nil && s.HostID == peerID {
		r.seen = nil
	}
	r.smu.Unlock()
	if s != nil && s.HostID == peerID && s.Here {
		overlayShow("Keydial disconnected", []string{s.Host + " is no longer connected; this machine isn't controlled any more"}, -1, false)
	}
}

// PeerUp tells a machine that just connected where the Keydial is pointed.
func (r *Router) PeerUp(peerID string) { r.announce(peerID) }

// announce tells connected machines (or just one) which machine the Keydial
// controls, so each can show it. Only a machine that reads a Keydial has one.
func (r *Router) announce(only string) {
	if !hostSupported || r.net == nil {
		return
	}
	t := r.Target()
	id, name := t, r.name(t)
	if t == "" {
		id, name = r.net.id.ID, r.net.id.Name
	}
	m := Msg{T: "target", IDs: []string{id}, Name: name}
	if only != "" {
		r.net.Send(only, m)
		return
	}
	for _, p := range r.net.peers.List() {
		if r.net.Online(p.ID) {
			r.net.Send(p.ID, m)
		}
	}
}

func (r *Router) Changed() { r.bus.Publish(BusEvent{Type: "net"}) }

// ---------- on-screen messages ----------

// showOSD shows (or replaces) Keybar's single on-screen notification.
func showOSD(title, body string) {
	if quietDesktop {
		return
	}
	osdShow(title, body)
}

// overlayShow shows a title and optional list (sel = highlighted row, -1 none).
// hold keeps it up until overlayHide; otherwise it disappears by itself.
func overlayShow(title string, lines []string, sel int, hold bool) {
	if quietDesktop {
		return
	}
	osOverlay(title, lines, sel, hold)
}

func overlayHide() {
	if quietDesktop {
		return
	}
	osOverlayHide()
}
