package main

import "testing"

// captured from the Huion app (RxBle log): the reply to 16 02 00 after a write
const captured = "16 02 00 01 0B 3E 00 00 00 01 00 0A 00 00 00 01 00 0F 00 00 00 01 00 4C 00 00 00 01 00 0C 00 00 00 01 00 07 00 00 00 01 00 05 00 00 00 01 00 08 00 00 00 01 00 16 00 00 00 01 00 1D 00 00 00 01 00 06 00 00 00 01 00 19 00 00 00 01 01 00 00 00 00 01 04 00 00 00 00 01 02 00 00 00 00 01 07 45 00 00 00 01 00 2C 00 00 00 01 07 44 00 00 00 02 CD 00 00 00 00 09 00 00 00 00 00 08 00 00 00 00 00 DC"

func TestKeybarMapping(t *testing.T) {
	entries, err := parseMapping(keybarMapping())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 21 {
		t.Fatalf("%d entries, want 21 (18 buttons, dial press, 2 dial turns)", len(entries))
	}
	want := map[int]string{0: "Ctrl+Shift+Alt+F1", 11: "Ctrl+Shift+Alt+F12", 12: "Ctrl+Alt+Win+F1",
		17: "Ctrl+Alt+Win+F6", 18: "Ctrl+Alt+Win+F7", 19: "mouse roll down", 20: "mouse roll up"}
	for i, w := range want {
		if got := describe(entries[i]); got != w {
			t.Errorf("entry %d = %q, want %q", i+1, got, w)
		}
	}
}

func TestParseCapturedMapping(t *testing.T) {
	b, err := parseHex(captured)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseMapping(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 21 {
		t.Fatalf("%d entries, want 21", len(entries))
	}
	want := map[int]string{0: "Ctrl+Shift+Win+F5", 3: "Delete", 15: "Ctrl+Shift+Alt+F12", 16: "Space",
		18: "media Play/Pause", 19: "mouse roll down", 20: "mouse roll up"}
	for i, w := range want {
		if got := describe(entries[i]); got != w {
			t.Errorf("entry %d = %q, want %q", i+1, got, w)
		}
	}
	// building the captured entries again gives the same block (as a write)
	rebuilt := buildMapping(entries)
	b[1] = 0x01
	if string(rebuilt) != string(b) {
		t.Fatalf("rebuilt % x\nwant    % x", rebuilt, b)
	}
	b[10] ^= 1 // corrupt one byte: checksum must catch it
	if _, err := parseMapping(b); err == nil {
		t.Error("corrupted block accepted")
	}
}
