package main

import (
	"fmt"
	"testing"
)

// The block tools/keydialcfg wrote and the Keydial confirmed (2026-11-27).
func TestKeybarMapping(t *testing.T) {
	b := keybarMapping()
	if len(b) != 3+21*6+1 || b[0] != 0x16 || b[1] != 0x01 || b[2] != 0x00 {
		t.Fatalf("block header/length: % x", b)
	}
	var sum byte
	for _, x := range b[3 : len(b)-1] {
		sum += x
	}
	if sum != b[len(b)-1] {
		t.Fatalf("checksum 0x%02x, block has 0x%02x", sum, b[len(b)-1])
	}
	entry := func(n int) string { return fmt.Sprintf("% x", b[3+6*(n-1):3+6*n]) }
	for n, want := range map[int]string{
		1:  "01 07 3a 00 00 00", // Ctrl+Shift+Alt+F1
		12: "01 07 45 00 00 00", // …F12
		13: "01 0d 3a 00 00 00", // Ctrl+Alt+Win+F1
		19: "01 0d 40 00 00 00", // dial press: Ctrl+Alt+Win+F7
		20: "09 00 00 00 00 00", // scroll
		21: "08 00 00 00 00 00",
	} {
		if got := entry(n); got != want {
			t.Errorf("entry %d = %s, want %s", n, got, want)
		}
	}
	if got := formatMAC("202503116dde"); got != "20:25:03:11:6D:DE" {
		t.Errorf("formatMAC = %q", got)
	}
}
