package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
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

	picker    *picker // only touched from the engine goroutine
	pickerGen int

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

// picker: pressing the pick key opens it. Turning the dial while the key is
// held and then releasing switches at once; releasing without turning leaves
// it open ("sticky"): turn the dial, then press the pick key or the dial to
// switch. Any other key, or pickerTimeout, closes it without switching.
type picker struct {
	list   []string // machine IDs, "" = this machine
	idx    int
	moved  bool // turned while the pick key was held
	sticky bool // open after a tap, waiting for a confirming press
	gen    int  // identifies this picker for its timeout
}

const pickerTimeout = 10 * time.Second

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
		switch {
		case slotID == "dial_cw":
			r.move(1)
			return &activeTrigger{}
		case slotID == "dial_ccw":
			r.move(-1)
			return &activeTrigger{}
		case slotID == "dial_press" && p.sticky:
			r.confirmPicker()
			return &activeTrigger{}
		case p.sticky: // another key: close the picker, then do the key's job
			r.picker = nil
			r.endPicker()
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
		if p := r.picker; p != nil && p.sticky { // second press: switch to the selection
			r.confirmPicker()
			return &activeTrigger{}
		}
		r.pickerGen++
		r.picker = &picker{list: list, idx: idx, gen: r.pickerGen}
		r.showPicker()
		return &activeTrigger{release: func() {
			p := r.picker
			if p == nil {
				return
			}
			if p.moved { // held and turned: switch now
				r.confirmPicker()
				return
			}
			p.sticky = true // tapped: stay open until confirmed, another key, or the timeout
			r.showPicker()
			gen := p.gen
			time.AfterFunc(pickerTimeout, func() {
				select {
				case r.e.in <- InputEvent{Type: evPickerTimeout, Value: int32(gen)}:
				default:
				}
			})
		}}
	case "self":
		r.SetTarget("", "")
	default:
		r.SetTarget(what, "")
	}
	return &activeTrigger{}
}

// confirmPicker closes the picker and switches to the selected machine.
func (r *Router) confirmPicker() {
	p := r.picker
	r.picker = nil
	if p == nil {
		return
	}
	r.endPicker()
	to := p.list[p.idx]
	if to == r.Target() {
		overlayShow("Keydial stays on "+r.name(to), nil, -1, false)
		r.announce("", true) // other screens: show the (unchanged) result too
		return
	}
	r.SetTarget(to, "")
}

// PickerTimeout closes a picker left open (engine goroutine).
func (r *Router) PickerTimeout(gen int) {
	if p := r.picker; p != nil && p.sticky && p.gen == gen {
		r.picker = nil
		r.endPicker()
	}
}

func (r *Router) move(d int) {
	p := r.picker
	n := len(p.list)
	p.idx = ((p.idx+d)%n + n) % n
	if !p.sticky {
		p.moved = true
	}
	r.showPicker()
}

const (
	pickerTitle       = "Keydial → turn the dial, release to switch"
	pickerTitleSticky = "Keydial → turn the dial, press again to switch"
)

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
	title := pickerTitle
	if p.sticky {
		title = pickerTitleSticky
	}
	overlayShow(title, lines, p.idx, true)
	r.broadcast(Msg{T: "picker", On: true, IDs: ids, Names: names, Idx: p.idx, Title: title})
}

func (r *Router) endPicker() {
	overlayHide()
	r.broadcast(Msg{T: "picker", On: false})
}

// broadcast sends a message to every connected paired machine: the person may
// be looking at any of the screens.
func (r *Router) broadcast(m Msg) {
	if r.net == nil {
		return
	}
	for _, p := range r.net.peers.List() {
		if r.net.Online(p.ID) {
			r.net.Send(p.ID, m)
		}
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
	r.announce("", false)
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
		same := prev != nil && prev.HostID == peerID && prev.Target == target && prev.Here == here
		switch {
		case same && !m.On:
			// nothing changed (e.g. reconnect)
		case same && here:
			overlayShow("Keydial stays on this machine", []string{"from " + r.name(peerID)}, -1, false)
		case same:
			overlayShow("Keydial stays on "+target, nil, -1, false)
		case here:
			overlayShow("Keydial → this machine", []string{"from " + r.name(peerID)}, -1, false)
		case prev != nil && prev.Here:
			overlayShow("Keydial → "+target, []string{"no longer controlling this machine"}, -1, false)
		case prev != nil:
			overlayShow("Keydial → "+target, nil, -1, false)
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
		title := pickerTitle
		if m.Title != "" { // the host says whether it waits for a release or a press
			title = clip(m.Title, 80)
		}
		overlayShow(title, lines, m.Idx, true)
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
func (r *Router) PeerUp(peerID string) { r.announce(peerID, false) }

// announce tells connected machines (or just one) which machine the Keydial
// controls, so each can show it. Only a machine that reads a Keydial has one.
// show asks them to display it even if it didn't change ("stays on").
func (r *Router) announce(only string, show bool) {
	if !hostSupported || r.net == nil {
		return
	}
	t := r.Target()
	id, name := t, r.name(t)
	if t == "" {
		id, name = r.net.id.ID, r.net.id.Name
	}
	m := Msg{T: "target", IDs: []string{id}, Name: name, On: show}
	if only != "" {
		r.net.Send(only, m)
		return
	}
	r.broadcast(m)
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
