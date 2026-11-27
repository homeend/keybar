//go:build windows

package main

import "testing"

// The hook turns a Keydial combination into the events Linux would send, and
// leaves everything else alone. The combination's modifiers are released in
// Windows at once (behind a mask key), and their real releases are swallowed.
func TestKeydialCombination(t *testing.T) {
	if fkeyLinux[0x70] != 59 || fkeyLinux[0x79] != 68 || fkeyLinux[0x7A] != 87 || fkeyLinux[0x7B] != 88 || len(fkeyLinux) != 12 {
		t.Fatalf("F-key map: %v", fkeyLinux)
	}
	var sent []keyboardInput
	defer func(f func([]keyboardInput)) { sendKeys = f }(sendKeys)
	sendKeys = func(in []keyboardInput) { sent = append(sent, in...) }
	out := make(chan InputEvent, 16)
	m := NewDeviceManager(nil, nil, out, nil)
	key := func(vk uint32, up bool, injected bool) bool {
		k := &kbdLLHook{vk: vk}
		if up {
			k.flags |= llkhfUp
		}
		if injected {
			k.flags |= llkhfInjected
		}
		return m.keyboard(k)
	}
	drain := func() []InputEvent {
		var evs []InputEvent
		for len(out) > 0 {
			evs = append(evs, <-out)
		}
		return evs
	}

	// Ctrl+Shift+Alt+F1 from the Keydial: modifiers pass, F1 is blocked.
	for _, vk := range []uint32{0xA2, 0xA0, 0xA4} {
		if key(vk, false, false) {
			t.Fatalf("modifier %#x blocked", vk)
		}
	}
	if !key(0x70, false, false) {
		t.Fatal("F1 of Ctrl+Shift+Alt+F1 not blocked")
	}
	want := []InputEvent{{evKey, codeCtrl, 1}, {evKey, codeShift, 1}, {evKey, codeAlt, 1}, {evKey, 59, 1}}
	if got := drain(); len(got) != len(want) || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("down events %v, want %v", got, want)
	}
	if len(sent) != 5 || sent[0].vk != vkMask || sent[2].flags&keyeventfKeyUp == 0 {
		t.Fatalf("lifted modifiers: %+v", sent)
	}
	if !key(0xA2, false, false) {
		t.Fatal("a repeated Ctrl down of the Keydial must be swallowed")
	}
	for _, vk := range []uint32{0xA2, 0xA0, 0xA4} { // the Keydial releases its modifiers first
		if !key(vk, true, false) {
			t.Fatalf("real release of %#x not swallowed", vk)
		}
	}
	if key(0xA2, false, false) || key(0xA2, true, false) {
		t.Fatal("Ctrl after the combination must pass again")
	}
	if !key(0x70, true, false) {
		t.Fatal("F1 release not blocked")
	}
	want = []InputEvent{{evKey, 59, 0}, {evKey, codeAlt, 0}, {evKey, codeShift, 0}, {evKey, codeCtrl, 0}}
	if got := drain(); len(got) != len(want) || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("up events %v, want %v", got, want)
	}

	// Ctrl+Alt+Win+F7: the second set.
	m.mods = map[uint32]bool{0xA2: true, 0xA4: true, 0x5B: true}
	if !key(0x76, false, false) {
		t.Fatal("F7 of Ctrl+Alt+Win+F7 not blocked")
	}
	if got := drain(); len(got) != 4 || got[2].Code != codeMeta || got[3].Code != 65 {
		t.Fatalf("Ctrl+Alt+Win+F7 events %v", got)
	}
	m.blocked = map[uint32][]uint16{}

	// Not Keydial combinations: plain F5, Ctrl+Alt+F5, all four modifiers, a letter, injected input.
	cases := []struct {
		mods map[uint32]bool
		vk   uint32
		inj  bool
	}{
		{map[uint32]bool{}, 0x74, false},
		{map[uint32]bool{0xA2: true, 0xA4: true}, 0x74, false},
		{map[uint32]bool{0xA2: true, 0xA4: true, 0xA0: true, 0x5B: true}, 0x74, false},
		{map[uint32]bool{0xA2: true, 0xA4: true, 0xA0: true}, 0x41, false},
		{map[uint32]bool{0xA2: true, 0xA4: true, 0xA0: true}, 0x74, true},
	}
	for i, c := range cases {
		m.mods = c.mods
		if key(c.vk, false, c.inj) {
			t.Errorf("case %d: %#x blocked", i, c.vk)
		}
	}
	if got := drain(); len(got) != 0 {
		t.Errorf("events for non-Keydial keys: %v", got)
	}
}

func TestBluetoothAddress(t *testing.T) {
	for name, want := range map[string]string{
		`\\?\hid#{00001812-0000-1000-8000-00805f9b34fb}_dev_vid&02256c_pid&8251_rev&0001_202503116dde#b&8686a42&0&0000#{4d1e55b2-f16f-11cf-88cb-001111000030}`: "202503116dde",
		`\\?\HID#VID_256C&PID_8251&MI_01#7&1234#{4d1e55b2-f16f-11cf-88cb-001111000030}`:                                                                        "",
		"": "",
	} {
		if got := bluetoothAddress(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	t.Logf("battery of 20:25:03:11:6D:DE on this machine: %d", bleBattery("202503116dde"))
}

func TestIsKeydialName(t *testing.T) {
	match := DeviceMatch{Vendor: "256c", Product: "8251"}
	for name, want := range map[string]bool{
		`\\?\hid#{00001812-0000-1000-8000-00805f9b34fb}_dev_vid&02256c_pid&8251_rev&0001_202503116dde&col03#b&3425750a&0&0002#{4d1e55b2-f16f-11cf-88cb-001111000030}`: true,
		`\\?\HID#VID_256C&PID_8251&MI_01#7&1234#{4d1e55b2-f16f-11cf-88cb-001111000030}`:                                                                               true,
		`\\?\hid#vid_046d&pid_c52b&mi_01&col01#a&25b98e35&0&0000#{378de44c-56ef-11d1-bc8c-00a0c91405dd}`:                                                              false,
	} {
		if got := isKeydialName(match, name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if isKeydialName(DeviceMatch{}, `vid&02256c_pid&8251`) {
		t.Error("an empty match must not match")
	}
}
