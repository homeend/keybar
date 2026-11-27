//go:build windows

// keydialprobe: Windows spikes for reading the Keydial (see WINDOWS_HOST_PLAN.md).
//
//	keydialprobe caps     list the Keydial's HID collections and whether they open
//	keydialprobe watch    2 min: print its HID reports + Raw Input events, by device
//	keydialprobe block    2 min: swallow the Keydial's keys/wheel, pass everything else
//	keydialprobe          list Logitech receiver devices and CHANGE_HOST support
//	keydialprobe switch <dev> <host>   send a Logitech device to Easy-Switch host 1-3
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	k32            = syscall.NewLazyDLL("kernel32.dll")
	hid            = syscall.NewLazyDLL("hid.dll")
	pCreateFile    = k32.NewProc("CreateFileW")
	pGetPreparsed  = hid.NewProc("HidD_GetPreparsedData")
	pFreePreparsed = hid.NewProc("HidD_FreePreparsedData")
	pGetCaps       = hid.NewProc("HidP_GetCaps")
)

type caps struct {
	Usage, UsagePage                uint16
	InLen, OutLen, FeatLen          uint16
	Reserved                        [17]uint16
	a, b, c, d, e, f, g, h, i, j, k uint16
}

type dev struct {
	path   string
	h      syscall.Handle
	page   uint16
	outLen int
	inLen  int
}

func open(path string) (*dev, error) {
	p, _ := syscall.UTF16PtrFromString(path)
	r, _, err := pCreateFile.Call(uintptr(unsafe.Pointer(p)),
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, 0, syscall.OPEN_EXISTING, 0, 0)
	if syscall.Handle(r) == syscall.InvalidHandle {
		return nil, err
	}
	d := &dev{path: path, h: syscall.Handle(r)}
	var pp uintptr
	if ok, _, _ := pGetPreparsed.Call(r, uintptr(unsafe.Pointer(&pp))); ok != 0 {
		var c caps
		pGetCaps.Call(pp, uintptr(unsafe.Pointer(&c)))
		pFreePreparsed.Call(pp)
		d.page, d.outLen, d.inLen = c.UsagePage, int(c.OutLen), int(c.InLen)
	}
	return d, nil
}

// instance IDs of the receiver's HID++ collections, from PnP
var pnpFilter = `^HID\\VID_046D&PID_C5`

func findPaths() []string {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`Get-PnpDevice -PresentOnly | Where-Object { $_.InstanceId -match '`+pnpFilter+`' } | ForEach-Object { $_.InstanceId }`).Output()
	if err != nil {
		fmt.Println("pnp:", err)
		return nil
	}
	var paths []string
	for _, l := range strings.Fields(string(out)) {
		paths = append(paths, `\\?\`+strings.ReplaceAll(strings.ToLower(l), `\`, "#")+`#{4d1e55b2-f16f-11cf-88cb-001111000030}`)
	}
	return paths
}

var in = make(chan []byte, 64)

func reader(d *dev) {
	for {
		buf := make([]byte, d.inLen)
		var n uint32
		if err := syscall.ReadFile(d.h, buf, &n, nil); err != nil {
			return
		}
		in <- buf[:n]
	}
}

var short, long *dev

func send(idx, feat, fn byte, params ...byte) {
	b := make([]byte, long.outLen)
	b[0], b[1], b[2], b[3] = 0x11, idx, feat, fn<<4|0x0A
	copy(b[4:], params)
	var n uint32
	if err := syscall.WriteFile(long.h, b, &n, nil); err != nil {
		fmt.Println("write:", err)
	}
}

// request returns the reply params, or an error code
func request(idx, feat, fn byte, params ...byte) ([]byte, error) {
	for len(in) > 0 {
		<-in
	}
	send(idx, feat, fn, params...)
	deadline := time.After(1500 * time.Millisecond)
	for {
		select {
		case r := <-in:
			if len(r) < 4 || r[1] != idx {
				continue
			}
			if r[2] == 0x8F || r[2] == 0xFF { // HID++ 1.0 / 2.0 error
				if r[3] == feat {
					return nil, fmt.Errorf("error 0x%02x", r[5])
				}
				continue
			}
			if r[2] == feat && r[3] == fn<<4|0x0A {
				return r[4:], nil
			}
		case <-deadline:
			return nil, fmt.Errorf("timeout")
		}
	}
}

func feature(idx byte, id uint16) (byte, error) {
	r, err := request(idx, 0, 0, byte(id>>8), byte(id))
	if err != nil {
		return 0, err
	}
	if r[0] == 0 {
		return 0, fmt.Errorf("not supported")
	}
	return r[0], nil
}

func name(idx byte) string {
	f, err := feature(idx, 0x0005)
	if err != nil {
		return "?"
	}
	r, err := request(idx, f, 0)
	if err != nil {
		return "?"
	}
	n := int(r[0])
	var s []byte
	for len(s) < n {
		c, err := request(idx, f, 1, byte(len(s)))
		if err != nil {
			break
		}
		for _, ch := range c {
			if ch == 0 || len(s) >= n {
				break
			}
			s = append(s, ch)
		}
	}
	return string(s)
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "block" {
		blockMain()
		return
	}
	if len(os.Args) < 2 || os.Args[1] == "switch" {
		go func() { time.Sleep(30 * time.Second); fmt.Println("overall timeout"); os.Exit(2) }()
	}
	if len(os.Args) == 2 && os.Args[1] == "watch" { // print raw reports from the readable Keydial collections
		pnpFilter = `^HID\\.*VID&02256C_PID&8251`
		start := time.Now()
		for _, p := range findPaths() {
			d, err := open(p)
			if err != nil {
				continue
			}
			tag := fmt.Sprintf("page 0x%04x", d.page)
			fmt.Println("watching", tag, "in", d.inLen, "bytes")
			go func(d *dev, tag string) {
				for {
					buf := make([]byte, d.inLen)
					var n uint32
					if err := syscall.ReadFile(d.h, buf, &n, nil); err != nil {
						fmt.Println(tag, "read:", err)
						return
					}
					fmt.Printf("%6.2fs  %s  % x\n", time.Since(start).Seconds(), tag, buf[:n])
				}
			}(d, tag)
		}
		go watchRaw(start)
		fmt.Println("READY")
		time.Sleep(120 * time.Second)
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "caps" { // list HID collections of the Keydial
		pnpFilter = `^HID\\.*VID&02256C_PID&8251`
		for _, p := range findPaths() {
			d, err := open(p)
			if err != nil {
				// keyboards/mice are opened exclusively by Windows: retry without access to read caps
				pp, _ := syscall.UTF16PtrFromString(p)
				r, _, e2 := pCreateFile.Call(uintptr(unsafe.Pointer(pp)), 0,
					syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, 0, syscall.OPEN_EXISTING, 0, 0)
				if syscall.Handle(r) == syscall.InvalidHandle {
					fmt.Println(p, "open:", err, e2)
					continue
				}
				d = &dev{path: p, h: syscall.Handle(r)}
				var ppd uintptr
				if ok, _, _ := pGetPreparsed.Call(r, uintptr(unsafe.Pointer(&ppd))); ok != 0 {
					var c caps
					pGetCaps.Call(ppd, uintptr(unsafe.Pointer(&c)))
					pFreePreparsed.Call(ppd)
					fmt.Printf("%s\n   page=0x%04x usage=0x%04x in=%d out=%d feat=%d  (read/write: %v)\n", p, c.UsagePage, c.Usage, c.InLen, c.OutLen, c.FeatLen, err)
				}
				continue
			}
			var ppd uintptr
			pGetPreparsed.Call(uintptr(d.h), uintptr(unsafe.Pointer(&ppd)))
			var c caps
			pGetCaps.Call(ppd, uintptr(unsafe.Pointer(&c)))
			fmt.Printf("%s\n   page=0x%04x usage=0x%04x in=%d out=%d feat=%d  (read/write: OK)\n", p, c.UsagePage, c.Usage, c.InLen, c.OutLen, c.FeatLen)
		}
		return
	}
	for _, p := range findPaths() {
		d, err := open(p)
		if err != nil {
			continue
		}
		fmt.Printf("%s  page=0x%04x in=%d out=%d\n", p, d.page, d.inLen, d.outLen)
		if d.page == 0xFF00 && d.outLen == 7 {
			short = d
		}
		if d.page == 0xFF00 && d.outLen == 20 {
			long = d
		}
	}
	if long == nil {
		fmt.Println("no HID++ long collection found")
		os.Exit(1)
	}
	// synchronous handles serialize I/O: a pending read would block writes,
	// so read through second handles
	if r, err := open(long.path); err == nil {
		go reader(r)
	}
	if short != nil {
		if r, err := open(short.path); err == nil {
			go reader(r)
		}
	}

	if len(os.Args) == 4 && os.Args[1] == "switch" {
		idx, _ := strconv.Atoi(os.Args[2])
		host, _ := strconv.Atoi(os.Args[3])
		f, err := feature(byte(idx), 0x1814)
		if err != nil {
			fmt.Println("CHANGE_HOST:", err)
			os.Exit(1)
		}
		send(byte(idx), f, 1, byte(host-1))
		time.Sleep(200 * time.Millisecond)
		fmt.Println("sent")
		return
	}

	for idx := byte(1); idx <= 6; idx++ {
		r, err := request(idx, 0, 1) // ping: protocol version
		if err != nil {
			continue
		}
		fmt.Printf("\ndevice %d: HID++ %d.%d  %q\n", idx, r[0], r[1], name(idx))
		f, err := feature(idx, 0x1814)
		if err != nil {
			fmt.Println("  CHANGE_HOST:", err)
			continue
		}
		h, err := request(idx, f, 0)
		if err != nil {
			fmt.Println("  getHostInfo:", err)
			continue
		}
		fmt.Printf("  CHANGE_HOST: %d hosts, current = %d\n", h[0], h[1]+1)
	}
}
