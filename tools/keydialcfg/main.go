// keydialcfg: read (and later write) the Keydial mini's button mapping over its
// configuration characteristic (FFE2 in service FFE0). See WINDOWS_HOST_PLAN.md.
//
//	keydialcfg <mac> read      print the current mapping and battery
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"tinygo.org/x/bluetooth"
)

var mods = []struct {
	bit  byte
	name string
}{{1, "Ctrl"}, {2, "Shift"}, {4, "Alt"}, {8, "Win"}, {16, "RCtrl"}, {32, "RShift"}, {64, "RAlt"}, {128, "RWin"}}

// usage names for the keys the app uses (USB HID keyboard page)
func keyName(u byte) string {
	switch {
	case u >= 0x04 && u <= 0x1d:
		return string(rune('A' + u - 0x04))
	case u >= 0x1e && u <= 0x26:
		return string(rune('1' + u - 0x1e))
	case u == 0x27:
		return "0"
	case u >= 0x3a && u <= 0x45:
		return fmt.Sprintf("F%d", u-0x3a+1)
	case u >= 0x68 && u <= 0x73:
		return fmt.Sprintf("F%d", u-0x68+13)
	}
	names := map[byte]string{0x28: "Enter", 0x29: "Esc", 0x2a: "Backspace", 0x2b: "Tab", 0x2c: "Space",
		0x2d: "-", 0x2e: "=", 0x2f: "[", 0x30: "]", 0x31: "\\", 0x33: ";", 0x34: "'", 0x35: "`", 0x36: ",",
		0x37: ".", 0x38: "/", 0x39: "Caps", 0x46: "PrintScreen", 0x47: "Scroll", 0x48: "Pause", 0x49: "Insert",
		0x4a: "Home", 0x4b: "PgUp", 0x4c: "Delete", 0x4d: "End", 0x4e: "PgDn", 0x4f: "Right", 0x50: "Left",
		0x51: "Down", 0x52: "Up", 0x55: "*", 0x57: "+"}
	if n, ok := names[u]; ok {
		return n
	}
	return fmt.Sprintf("0x%02x", u)
}

func describe(b []byte) string {
	switch b[0] {
	case 1:
		var parts []string
		for _, m := range mods {
			if b[1]&m.bit != 0 {
				parts = append(parts, m.name)
			}
		}
		for _, k := range b[2:6] {
			if k != 0 {
				parts = append(parts, keyName(k))
			}
		}
		return strings.Join(parts, "+")
	case 2:
		media := map[byte]string{0xb6: "Previous", 0xcd: "Play/Pause", 0xb5: "Next", 0xb7: "Stop", 0xe2: "Mute", 0xea: "Volume down", 0xe9: "Volume up"}
		if n, ok := media[b[1]]; ok {
			return "media " + n
		}
		return fmt.Sprintf("media 0x%02x", b[1])
	case 3, 4, 5, 6, 7, 8, 9:
		return "mouse " + []string{"left", "right", "middle", "forward", "back", "roll up", "roll down"}[b[0]-3]
	case 0:
		return "(nothing)"
	}
	return "?"
}

// parseMapping checks a "16 01|02 <group> <6 bytes per entry> <checksum>" block.
func parseMapping(b []byte) ([][]byte, error) {
	if len(b) < 10 || b[0] != 0x16 || (len(b)-4)%6 != 0 {
		return nil, fmt.Errorf("not a mapping block (%d bytes)", len(b))
	}
	var sum byte
	var entries [][]byte
	for i := 3; i+6 < len(b); i += 6 {
		entries = append(entries, b[i:i+6])
		for _, x := range b[i : i+6] {
			sum += x
		}
	}
	if sum != b[len(b)-1] {
		return nil, fmt.Errorf("checksum 0x%02x, block says 0x%02x", sum, b[len(b)-1])
	}
	return entries, nil
}

// keybarMapping: every button sends a combination no real keyboard sends, so
// Keybar can block it on Windows without knowing which device sent it.
// Buttons 1–12: Ctrl+Alt+Shift+F1…F12; buttons 13–18: Ctrl+Alt+Win+F1…F6;
// dial press: Ctrl+Alt+Win+F7; dial turns stay mouse scroll (down, up).
func keybarMapping() []byte {
	const cas, caw = 0x01 | 0x02 | 0x04, 0x01 | 0x04 | 0x08
	f := func(n int) byte { return byte(0x3a + n - 1) } // F1 = 0x3a
	var entries [][]byte
	for i := 1; i <= 12; i++ {
		entries = append(entries, []byte{1, cas, f(i), 0, 0, 0})
	}
	for i := 1; i <= 7; i++ { // buttons 13–18 and the dial press
		entries = append(entries, []byte{1, caw, f(i), 0, 0, 0})
	}
	entries = append(entries, []byte{9, 0, 0, 0, 0, 0}, []byte{8, 0, 0, 0, 0, 0})
	return buildMapping(entries)
}

func buildMapping(entries [][]byte) []byte {
	b := []byte{0x16, 0x01, 0x00}
	var sum byte
	for _, e := range entries {
		b = append(b, e...)
		for _, x := range e {
			sum += x
		}
	}
	return append(b, sum)
}

func parseHex(s string) ([]byte, error) {
	var out []byte
	for _, f := range strings.Fields(s) {
		var v byte
		if _, err := fmt.Sscanf(f, "%02x", &v); err != nil || len(f) != 2 {
			return nil, fmt.Errorf("bad hex byte %q", f)
		}
		out = append(out, v)
	}
	return out, nil
}

func main() {
	var block []byte
	switch {
	case len(os.Args) == 3 && os.Args[2] == "read":
	case len(os.Args) == 4 && os.Args[2] == "write":
		b, err := parseHex(os.Args[3])
		must("mapping", err)
		_, err = parseMapping(b)
		must("mapping", err)
		if b[1] != 0x01 {
			must("mapping", fmt.Errorf("a write block starts with 16 01"))
		}
		block = b
	case len(os.Args) == 3 && os.Args[2] == "keybar":
		block = keybarMapping()
	default:
		fmt.Println("usage: keydialcfg <mac> read | keybar | write \"16 01 00 … <checksum>\"")
		os.Exit(2)
	}
	go func() { time.Sleep(45 * time.Second); fmt.Println("timeout"); os.Exit(1) }()

	mac, err := bluetooth.ParseMAC(os.Args[1])
	must("mac", err)
	adapter := bluetooth.DefaultAdapter
	must("enable adapter", adapter.Enable())
	fmt.Println("connecting to", os.Args[1], "...")
	dev, err := adapter.Connect(bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac}}, bluetooth.ConnectionParams{})
	must("connect", err)
	defer dev.Disconnect()

	svcs, err := dev.DiscoverServices([]bluetooth.UUID{bluetooth.New16BitUUID(0xFFE0)})
	must("discover FFE0", err)
	if len(svcs) == 0 {
		fmt.Println("service FFE0 not found")
		os.Exit(1)
	}
	all, err := svcs[0].DiscoverCharacteristics(nil)
	must("discover characteristics", err)
	var c bluetooth.DeviceCharacteristic
	found := false
	for _, ch := range all {
		fmt.Printf("characteristic %s properties 0x%02x\n", ch.UUID(), ch.Properties())
		if ch.UUID() == bluetooth.New16BitUUID(0xFFE2) {
			c, found = ch, true
			continue
		}
		name := ch.UUID().String()[4:8]
		if err := ch.EnableNotifications(func(buf []byte) {
			fmt.Printf("  NOTIFY %s: % x\n", name, buf)
		}); err != nil {
			fmt.Println("  notifications on", name+":", err)
		}
	}
	if !found {
		fmt.Println("characteristic FFE2 not found")
		os.Exit(1)
	}
	if mtu, err := c.GetMTU(); err == nil {
		fmt.Println("MTU", mtu)
	}

	replies := make(chan []byte, 16)
	must("notifications", c.EnableNotifications(func(buf []byte) {
		replies <- append([]byte(nil), buf...)
	}))

	// ask sends cmd and waits for the reply starting with want (other
	// notifications, e.g. repeated battery reports, are printed and skipped)
	ask := func(cmd []byte, want ...byte) []byte {
		// Android's default write type is with response
		if _, err := c.Write(cmd); err != nil {
			fmt.Println("  write with response:", err, "- retrying without")
			if _, err := c.WriteWithoutResponse(cmd); err != nil {
				must("write", err)
			}
		}
		deadline := time.After(4 * time.Second)
		for {
			select {
			case r := <-replies:
				if len(r) >= len(want) && string(r[:len(want)]) == string(want) {
					return r
				}
				fmt.Printf("  (other notification: % x)\n", r)
			case <-deadline:
				return nil
			}
		}
	}

	if r := ask([]byte{0xCD, 0xD1, 0, 0, 0, 0, 0, 0}, 0x06, 0xD1); len(r) >= 4 {
		pct := r[3]
		if pct == 0 {
			pct = r[2]
		}
		fmt.Printf("battery: %d%%   (% x)\n", pct, r)
	} else {
		fmt.Printf("battery: no reply (% x)\n", r)
	}

	time.Sleep(300 * time.Millisecond)
	for len(replies) > 0 {
		fmt.Printf("  (other notification: % x)\n", <-replies)
	}
	// the app asks for both firmware versions before it reads the mapping
	for _, cmd := range [][]byte{{0xCD, 0xC9, 0, 0, 0, 0, 0, 0xED}, {0xCD, 0xFE, 0, 0, 0, 0, 0, 0xED}} {
		v := ask(cmd, 0x06, cmd[1]) // any reply: 06 C9 … / 06 FE …?
		if v == nil {
			v = ask(cmd) // any notification at all
		}
		fmt.Printf("version % x: % x  %q\n", cmd[:2], v, printable(v))
	}
	if block != nil {
		// the app writes, waits for 16 01 <group> 01 (OK), then reads back
		ack := ask(block, 0x16, 0x01)
		fmt.Printf("write reply: % x\n", ack)
		if len(ack) < 4 || ack[3] != 0x01 {
			fmt.Println("write NOT confirmed")
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
	var r []byte
	for _, cmd := range [][]byte{
		{0x16, 0x02, 0x00},
		{0x16, 0x02, 0x00, 0, 0, 0, 0, 0},
		{0x16, 0x02, 0x01},
	} {
		fmt.Printf("read mapping with % x ...\n", cmd)
		if r = ask(cmd, 0x16, 0x02); r != nil {
			break
		}
	}
	if r == nil {
		// maybe the reply is left in a characteristic's value, or comes late
		for _, ch := range all {
			buf := make([]byte, 512)
			n, err := ch.Read(buf)
			fmt.Printf("read %s: % x (err %v)\n", ch.UUID().String()[4:8], buf[:n], err)
		}
		fmt.Println("waiting 8 s for a late reply ...")
		select {
		case late := <-replies:
			fmt.Printf("late notification: % x\n", late)
			r = late
		case <-time.After(8 * time.Second):
		}
	}
	fmt.Printf("mapping reply (%d bytes): % x\n", len(r), r)
	if len(r) < 10 || r[0] != 0x16 || r[1] != 0x02 {
		os.Exit(1)
	}
	entries, err := parseMapping(r)
	must("reply", err)
	for i, e := range entries {
		fmt.Printf("  entry %2d: % x   %s\n", i+1, e, describe(e))
	}
}

func printable(b []byte) string {
	var s []byte
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			s = append(s, c)
		}
	}
	return string(s)
}

func must(what string, err error) {
	if err != nil {
		fmt.Println(what+":", err)
		os.Exit(1)
	}
}
