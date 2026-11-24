package main

import (
	"os"
	"path/filepath"
	"testing"
)

func collect(t *testing.T, evs []InputEvent) []BusEvent {
	t.Helper()
	store := LoadConfig(filepath.Join(t.TempDir(), "c.json"))
	bus := NewBus()
	ch, _ := bus.Subscribe()
	e := NewEngine(store, &Emitter{}, bus)
	for _, ev := range evs {
		e.handle(ev)
	}
	var out []BusEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func k(code uint16, v int32) InputEvent { return InputEvent{Type: evKey, Code: code, Value: v} }

func TestChords(t *testing.T) {
	ctrl, shift, z := uint16(29), uint16(42), uint16(44)
	cases := []struct {
		name string
		evs  []InputEvent
		want []string
	}{
		{"ctrl+z", []InputEvent{k(ctrl, 1), k(z, 1), k(z, 0), k(ctrl, 0)}, []string{"KEY_LEFTCTRL+KEY_Z"}},
		{"mods released first", []InputEvent{k(shift, 1), k(ctrl, 1), k(z, 1), k(ctrl, 0), k(shift, 0), k(z, 0)}, []string{"KEY_LEFTCTRL+KEY_LEFTSHIFT+KEY_Z"}},
		{"modifier only", []InputEvent{k(56, 1), k(56, 0)}, []string{"KEY_LEFTALT"}},
		{"plain key + repeat", []InputEvent{k(48, 1), k(48, 2), k(48, 0)}, []string{"KEY_B"}},
		{"wheel", []InputEvent{{Type: evRel, Code: relWheel, Value: -1}, {Type: evRel, Code: relWheelHiRes, Value: -120}}, []string{"REL_WHEEL:down"}},
		{"ctrl+wheel", []InputEvent{k(ctrl, 1), {Type: evRel, Code: relWheel, Value: 1}, k(ctrl, 0)}, []string{"KEY_LEFTCTRL+REL_WHEEL:up"}},
	}
	for _, c := range cases {
		got := collect(t, c.evs)
		var sigs []string
		for _, g := range got {
			sigs = append(sigs, g.Trigger)
		}
		if len(sigs) != len(c.want) || (len(sigs) > 0 && sigs[0] != c.want[0]) {
			t.Errorf("%s: got %v want %v", c.name, sigs, c.want)
		}
	}
}

func TestParseShortcut(t *testing.T) {
	for _, s := range []string{"ctrl+shift+t", "esc", "super", "ctrl+alt+q, ctrl+alt+q", "volumeup", "print", "kp1", "sysrq", "f13"} {
		if _, err := parseShortcut(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := parseShortcut("ctrl+nope"); err == nil {
		t.Error("expected error")
	}
	if len(typeMap) < 90 {
		t.Errorf("typeMap too small: %d", len(typeMap))
	}
}

func TestLearnCapturesOnce(t *testing.T) {
	store := LoadConfig(filepath.Join(t.TempDir(), "c.json"))
	bus := NewBus()
	ch, _ := bus.Subscribe()
	e := NewEngine(store, &Emitter{}, bus)
	e.StartLearning()
	for _, ev := range []InputEvent{k(57, 1), k(57, 0), k(57, 1), k(57, 0)} {
		e.handle(ev)
	}
	if a, b := <-ch, <-ch; a.Type != "learned" || a.Trigger != "KEY_SPACE" || b.Type != "trigger" {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestShortcutString(t *testing.T) {
	if got := shortcutString(map[uint16]bool{56: true, 29: true}, 16); got != "ctrl+alt+q" {
		t.Errorf("got %q", got)
	}
	if got := shortcutString(map[uint16]bool{125: true}, 0); got != "super" {
		t.Errorf("got %q", got)
	}
}

func TestResetReleasesHeld(t *testing.T) {
	store := LoadConfig(filepath.Join(t.TempDir(), "c.json"))
	released := false
	e := NewEngine(store, &Emitter{down: map[uint16]bool{}}, NewBus())
	e.handle(k(29, 1))
	e.handle(k(44, 1)) // Ctrl+Z held...
	e.active[44].release = func() { released = true }
	e.handle(InputEvent{Type: evReset}) // ...device drops
	if !released || len(e.held) != 0 || len(e.active) != 0 {
		t.Fatalf("reset left state: released=%v held=%v active=%v", released, e.held, e.active)
	}
}

func TestMigrateV3(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	v3 := `{"version":3,"listen":"127.0.0.1:47830","columns":4,"bindings":[
	 {"id":"k1","label":"Undo","icon":"↩","trigger":"KEY_K","group":"keys","x":0,"y":0,"w":1,"h":1,"action":{"type":"keys","keys":"ctrl+z"}},
	 {"id":"k18","label":"Voice stop","trigger":"KEY_ENTER","group":"keys","x":3,"y":3,"w":1,"h":2,"action":{"type":"keys","keys":"super+alt+d"}}]}`
	if err := os.WriteFile(p, []byte(v3), 0o644); err != nil {
		t.Fatal(err)
	}
	c := LoadConfig(p).Get()
	if c.Version != configVersion || len(c.Slots) != 2 || len(c.Layouts) != 1 || c.Bindings != nil {
		t.Fatalf("bad migration: %+v", c)
	}
	k := c.Layouts[0].Keys["k1"]
	if k.Label != "Undo" || k.Icon != "↩" || k.Action.Keys != "ctrl+z" || c.Slots[0].Trigger != "KEY_K" || c.Slots[1].H != 2 {
		t.Fatalf("lost data: %+v %+v", k, c.Slots)
	}
}

func TestLayoutSwitch(t *testing.T) {
	store := LoadConfig(filepath.Join(t.TempDir(), "c.json"))
	c := *store.Get()
	c.Slots = append([]Slot(nil), c.Slots...)
	c.Slots[3].Trigger = "KEY_B" // k1
	a := newLayout("a", "A", c.Slots)
	a.Keys["k1"] = Key{Label: "next", Action: Action{Type: "layout", Layout: "next"}}
	b := newLayout("b", "B", c.Slots)
	c.Layouts = []Layout{a, b}
	c.ActiveLayout = "a"
	store.cfg.ActiveLayout = "a"
	if err := store.Set(&c); err != nil {
		t.Fatal(err)
	}
	bus := NewBus()
	ch, _ := bus.Subscribe()
	e := NewEngine(store, &Emitter{}, bus)
	e.handle(k(48, 1))
	e.handle(k(48, 0))
	if got := store.Get().ActiveLayout; got != "b" {
		t.Fatalf("active = %s", got)
	}
	if ev1, ev2 := <-ch, <-ch; ev1.Binding != "k1" || ev2.Type != "layout" || ev2.Layout != "b" {
		t.Fatalf("events %+v %+v", ev1, ev2)
	}
	if _, err := store.SwitchLayout("nope"); err == nil {
		t.Fatal("expected error")
	}
}
