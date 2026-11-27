package main

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// Huion's configuration protocol (see WINDOWS_HOST_PLAN.md, from the Keydial
// app): commands go to characteristic FFE2 in service FFE0 and the replies come
// back as notifications on it. tools/keydialcfg reads and prints the mapping.

// keybarMapping: every button sends a combination no real keyboard sends, so
// Keybar can block it on Windows without knowing which device sent it.
// Buttons 1–12: Ctrl+Alt+Shift+F1…F12; buttons 13–18: Ctrl+Alt+Win+F1…F6;
// dial press: Ctrl+Alt+Win+F7; dial turns stay mouse scroll (down, up).
// An entry is [type, modifiers, key1..key4]: type 1 = keys (USB modifier bits
// Ctrl 1, Shift 2, Alt 4, Win 8; USB HID usages, F1 = 0x3a), 8/9 = scroll.
func keybarMapping() []byte {
	const cas, caw = 0x01 | 0x02 | 0x04, 0x01 | 0x04 | 0x08
	f := func(n int) byte { return byte(0x3a + n - 1) }
	var entries [][]byte
	for i := 1; i <= 12; i++ {
		entries = append(entries, []byte{1, cas, f(i), 0, 0, 0})
	}
	for i := 1; i <= 7; i++ { // buttons 13–18 and the dial press
		entries = append(entries, []byte{1, caw, f(i), 0, 0, 0})
	}
	entries = append(entries, []byte{9, 0, 0, 0, 0, 0}, []byte{8, 0, 0, 0, 0, 0})
	b := []byte{0x16, 0x01, 0x00} // write mapping, group 0
	var sum byte
	for _, e := range entries {
		b = append(b, e...)
		for _, x := range e {
			sum += x
		}
	}
	return append(b, sum)
}

var (
	bleMu      sync.Mutex
	bleEnabled bool
)

// setupKeydial writes Keybar's mapping to the Keydial at address
// ("20:25:03:11:6D:DE"), which must be connected to this machine.
func setupKeydial(address string) error {
	bleMu.Lock()
	defer bleMu.Unlock()
	mac, err := bluetooth.ParseMAC(address)
	if err != nil {
		return err
	}
	adapter := bluetooth.DefaultAdapter
	if !bleEnabled {
		if err := adapter.Enable(); err != nil {
			return fmt.Errorf("Bluetooth: %w", err)
		}
		bleEnabled = true
	}
	type result struct {
		ack []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		ack, err := writeMapping(adapter, mac, keybarMapping())
		done <- result{ack, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		log.Printf("keydial: mapping written (% x)", r.ack)
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("the Keydial didn't answer (is it awake?)")
	}
}

func writeMapping(adapter *bluetooth.Adapter, mac bluetooth.MAC, block []byte) ([]byte, error) {
	dev, err := adapter.Connect(bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac}}, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if runtime.GOOS == "windows" {
		// Closes only this program's GATT session. On Linux it would drop the
		// Keydial's connection, so it stays open there.
		defer dev.Disconnect()
	}
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{bluetooth.New16BitUUID(0xFFE0)})
	if err != nil || len(svcs) == 0 {
		return nil, fmt.Errorf("the Keydial's configuration service wasn't found (%v)", err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{bluetooth.New16BitUUID(0xFFE2)})
	if err != nil || len(chars) == 0 {
		return nil, fmt.Errorf("the Keydial's configuration characteristic wasn't found (%v)", err)
	}
	c := chars[0]
	replies := make(chan []byte, 16)
	if err := c.EnableNotifications(func(buf []byte) {
		select {
		case replies <- append([]byte(nil), buf...):
		default:
		}
	}); err != nil {
		return nil, fmt.Errorf("notifications: %w", err)
	}
	// Windows can't write with response to FFE2; without response works everywhere.
	if _, err := c.WriteWithoutResponse(block); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-replies:
			if len(r) >= 4 && r[0] == 0x16 && r[1] == 0x01 { // 16 01 00 01 = OK
				if r[3] != 0x01 {
					return r, fmt.Errorf("the Keydial refused the mapping (% x)", r)
				}
				return r, nil
			}
		case <-deadline:
			return nil, errors.New("the Keydial didn't confirm the mapping")
		}
	}
}

// formatMAC turns "202503116dde" into "20:25:03:11:6D:DE".
func formatMAC(hex string) string {
	if len(hex) != 12 {
		return ""
	}
	out := make([]byte, 0, 17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[i:i+2]...)
	}
	return strings.ToUpper(string(out))
}
