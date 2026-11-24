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
			to := p.list[p.idx]
			if !p.moved { // a quick tap goes back to the previous machine, like Alt+Tab
				to = r.prevOr(p.list)
			}
			if to == r.Target() {
				hidePicker()
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

func (r *Router) showPicker() {
	p := r.picker
	var b strings.Builder
	for i, id := range p.list {
		if i == p.idx {
			b.WriteString("▶  " + r.name(id) + "\n")
		} else {
			b.WriteString("     " + r.name(id) + "\n")
		}
	}
	if len(p.list) == 1 {
		b.WriteString("\nNo other machine connected.")
	}
	showOSD("Keydial → turn the dial, release to switch", b.String())
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
		r.net.Send(from, Msg{T: "active", On: false})
	}
	if to != "" {
		r.net.Send(to, Msg{T: "active", On: true})
	}
	msg := "Keydial → " + r.name(to)
	if why != "" {
		msg += " (" + r.name(from) + ": " + why + ")"
	}
	log.Print(msg)
	showOSD(msg, "")
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
	case "active":
		if m.On {
			showOSD("Keydial from "+r.name(peerID)+" now controls this machine", "")
		}
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
}

func (r *Router) Changed() { r.bus.Publish(BusEvent{Type: "net"}) }

// ---------- on-screen messages ----------

// showOSD shows (or replaces) Keybar's single on-screen notification.
func showOSD(title, body string) {
	if quietDesktop {
		return
	}
	go osdShow(title, body)
}

func hidePicker() { showOSD("Keydial: no change", "") }
