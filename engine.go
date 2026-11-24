package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The Keydial sends factory shortcuts (e.g. Ctrl+Z, B, E) and wheel events.
// The engine turns each of those into a "trigger" signature, looks up the
// binding, and runs its action. Modifiers are held back until we know which
// key completes the chord, so Ctrl+Z on the device never leaks a bare Ctrl.

type activeTrigger struct {
	release func()
}

type Engine struct {
	store    *ConfigStore
	em       *Emitter
	bus      *Bus
	in       chan InputEvent
	grabbed  func() bool
	learnUntil atomic.Int64 // unix nanos; the next press is captured instead of acted on

	held      []uint16        // modifiers physically held, in press order
	chordMods map[uint16]bool // every modifier seen in the current chord
	consumed  bool            // a non-modifier (or wheel) completed the chord
	emitted   map[uint16]bool // modifiers we forwarded for a passthrough
	active    map[uint16]*activeTrigger
}

func NewEngine(store *ConfigStore, em *Emitter, bus *Bus) *Engine {
	return &Engine{
		store: store, em: em, bus: bus,
		in:        make(chan InputEvent, 256),
		grabbed:   func() bool { return false },
		chordMods: map[uint16]bool{},
		emitted:   map[uint16]bool{},
		active:    map[uint16]*activeTrigger{},
	}
}

func (e *Engine) Run() {
	for ev := range e.in {
		e.handle(ev)
	}
}

// StartLearning captures the next press for the panel. It expires after a
// minute so a closed panel can never leave a button swallowed.
func (e *Engine) StartLearning()  { e.learnUntil.Store(time.Now().Add(time.Minute).UnixNano()) }
func (e *Engine) CancelLearning() { e.learnUntil.Store(0) }

// evReset is an internal pseudo-event: the device went away, forget everything held.
const evReset = 0xffff

func (e *Engine) Reset() { e.in <- InputEvent{Type: evReset} }

func (e *Engine) handle(ev InputEvent) {
	switch ev.Type {
	case evReset:
		for _, at := range e.active {
			if at.release != nil {
				at.release()
			}
		}
		e.active = map[uint16]*activeTrigger{}
		e.held = nil
		e.emitted = map[uint16]bool{}
		e.resetChord()
		e.em.ReleaseAll()
	case evKey:
		if ev.Value == 2 { // autorepeat: the compositor repeats our virtual keys itself
			return
		}
		down := ev.Value == 1
		switch {
		case isModifier(ev.Code):
			e.modifier(ev.Code, down)
		case down:
			e.keyDown(ev.Code)
		default:
			e.keyUp(ev.Code)
		}
	case evRel:
		switch ev.Code {
		case relWheel, relHWheel, relDial, relMisc:
			if ev.Value == 0 {
				return
			}
			dir := ":up"
			if ev.Value < 0 {
				dir = ":down"
			}
			e.consumed = true
			code, v := ev.Code, ev.Value
			sig := signature(e.heldSet(), relNames[code]+dir)
			e.tap(sig, func() {
				e.forwardMods()
				if code == relDial || code == relMisc {
					code = relWheel // no virtual dial axis; scroll is the closest thing
				}
				e.em.Rel(code, v)
			})
		case relX, relY:
			if e.grabbed() {
				e.em.Rel(ev.Code, ev.Value)
			}
		}
	}
}

func (e *Engine) heldSet() map[uint16]bool {
	m := map[uint16]bool{}
	for _, c := range e.held {
		m[c] = true
	}
	return m
}

func (e *Engine) modifier(code uint16, down bool) {
	if down {
		for _, c := range e.held {
			if c == code {
				return
			}
		}
		e.held = append(e.held, code)
		e.chordMods[code] = true
		return
	}
	for i, c := range e.held {
		if c == code {
			e.held = append(e.held[:i], e.held[i+1:]...)
			break
		}
	}
	if e.emitted[code] {
		e.em.Key(code, false)
		delete(e.emitted, code)
	}
	if len(e.held) == 0 && len(e.active) == 0 {
		if !e.consumed && len(e.chordMods) > 0 { // a modifier-only button
			mods := e.chordMods
			e.tap(signature(mods, ""), func() {
				for c := range mods {
					e.em.Key(c, true)
				}
				for c := range mods {
					e.em.Key(c, false)
				}
			})
		}
		e.resetChord()
	}
}

func (e *Engine) resetChord() {
	e.chordMods = map[uint16]bool{}
	e.consumed = false
}

func (e *Engine) keyDown(code uint16) {
	if _, dup := e.active[code]; dup {
		return
	}
	e.consumed = true
	sig := signature(e.heldSet(), keyName(code))
	e.active[code] = e.press(sig,
		func() { e.forwardMods(); e.em.Key(code, true) },
		func() { e.em.Key(code, false) })
}

func (e *Engine) keyUp(code uint16) {
	if at, ok := e.active[code]; ok {
		if at.release != nil {
			at.release()
		}
		delete(e.active, code)
	}
	if len(e.held) == 0 && len(e.active) == 0 {
		e.resetChord()
	}
}

func (e *Engine) forwardMods() {
	for _, c := range e.held {
		if !e.emitted[c] {
			e.em.Key(c, true)
			e.emitted[c] = true
		}
	}
}

func (e *Engine) tap(sig string, passDown func()) {
	if at := e.press(sig, passDown, nil); at.release != nil {
		at.release()
	}
}

// press resolves a trigger and starts its action; the returned release runs on button-up.
func (e *Engine) press(sig string, passDown, passUp func()) *activeTrigger {
	if until := e.learnUntil.Load(); until > time.Now().UnixNano() && e.learnUntil.CompareAndSwap(until, 0) {
		e.bus.Publish(BusEvent{Type: "learned", Trigger: sig})
		return &activeTrigger{}
	}
	cfg := e.store.Get()
	b := e.store.Find(sig)
	ev := BusEvent{Type: "trigger", Trigger: sig}
	var act Action
	if b != nil {
		ev.Binding = b.ID
		act = b.Action
	} else if cfg.UnmappedPassthrough {
		act = Action{Type: "passthrough"}
	} else {
		act = Action{Type: "none"}
	}
	e.bus.Publish(ev)
	log.Printf("trigger %s -> binding=%q action=%+v grabbed=%v", sig, ev.Binding, act, e.grabbed())

	if act.Type == "passthrough" {
		if !e.grabbed() {
			return &activeTrigger{} // the original event already reached the desktop
		}
		passDown()
		return &activeTrigger{release: passUp}
	}
	return &activeTrigger{release: runAction(e.em, act)}
}

// runAction starts an action and returns what to do on release (may be nil).
func runAction(em *Emitter, a Action) func() {
	switch a.Type {
	case "keys":
		seq, err := parseShortcut(a.Keys)
		if err != nil {
			log.Printf("shortcut %q: %v", a.Keys, err)
			return nil
		}
		if len(seq) == 1 { // a single combo is held for as long as the button is
			combo := seq[0]
			for _, k := range combo {
				em.Key(k, true)
			}
			return func() {
				for i := len(combo) - 1; i >= 0; i-- {
					em.Key(combo[i], false)
				}
			}
		}
		for _, combo := range seq {
			for _, k := range combo {
				em.Key(k, true)
			}
			for i := len(combo) - 1; i >= 0; i-- {
				em.Key(combo[i], false)
			}
			time.Sleep(15 * time.Millisecond)
		}
	case "app":
		if a.App != "" {
			spawn("gtk-launch " + shellQuote(strings.TrimSuffix(a.App, ".desktop")))
		}
	case "command":
		spawn(a.Command)
		if a.ReleaseCommand != "" {
			rc := a.ReleaseCommand
			return func() { spawn(rc) }
		}
	case "mouse":
		return runMouse(em, a)
	case "text":
		typeText(em, a.Text)
	}
	return nil
}

func runMouse(em *Emitter, a Action) func() {
	n := int32(a.Amount)
	if n == 0 {
		n = 1
	}
	buttons := map[string]uint16{"left": btnLeft, "right": btnRight, "middle": btnMiddle, "back": btnSide, "forward": btnExtra}
	if b, ok := buttons[a.Mouse]; ok {
		em.Key(b, true)
		return func() { em.Key(b, false) }
	}
	switch a.Mouse {
	case "scroll_up":
		em.Rel(relWheel, n)
	case "scroll_down":
		em.Rel(relWheel, -n)
	case "scroll_left":
		em.Rel(relHWheel, -n)
	case "scroll_right":
		em.Rel(relHWheel, n)
	case "move":
		em.Move(int32(a.Amount), int32(a.DY))
	}
	return nil
}

func typeText(em *Emitter, s string) {
	shift := keyByNameMust("KEY_LEFTSHIFT")
	for _, r := range s {
		k, ok := typeMap[r]
		if !ok {
			log.Printf("text: cannot type %q (US layout ASCII only)", r)
			continue
		}
		if k.shift {
			em.Key(shift, true)
		}
		em.Key(k.code, true)
		em.Key(k.code, false)
		if k.shift {
			em.Key(shift, false)
		}
		time.Sleep(4 * time.Millisecond)
	}
}

func spawn(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	c := exec.Command("sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Dir = os.Getenv("HOME")
	if err := c.Start(); err != nil {
		log.Printf("run %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
