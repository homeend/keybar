//go:build windows

package main

// block: prototype of approach A. Low-level hooks hold each hardware key / wheel
// event until Raw Input (or, for media keys, the Keydial's consumer HID
// collection) says which device sent it: the Keydial's events are swallowed,
// everything else is re-sent. An event whose source doesn't show up within
// holdFor is re-sent too, so the real keyboard can't get stuck.

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	pSetWindowsHookEx = u32.NewProc("SetWindowsHookExW")
	pCallNextHookEx   = u32.NewProc("CallNextHookEx")
	pSendInput        = u32.NewProc("SendInput")
	pPostMessage      = u32.NewProc("PostMessageW")
	pSetTimer         = u32.NewProc("SetTimer")
	pGetModuleHandle  = k32.NewProc("GetModuleHandleW")
)

const (
	marker   = 0x4B455942 // dwExtraInfo of input we re-send
	wmInput  = 0x00FF
	wmTimer  = 0x0113
	wmMedia  = 0x8001 // WM_APP+1: consumer report from the Keydial
	wmStop   = 0x8002
	holdFor  = 60 * time.Millisecond
	keepEarl = 300 * time.Millisecond
)

type kbdHookStruct struct {
	vk, scan, flags, time uint32
	extra                 uintptr
}

type msHookStruct struct {
	x, y              int32
	data, flags, time uint32
	extra             uintptr
}

type keyInput struct {
	typ       uint32
	_         uint32
	vk, scan  uint16
	flags, tm uint32
	_         uint32
	extra     uintptr
	_         [8]byte
}

type mouseInput struct {
	typ             uint32
	_               uint32
	dx, dy          int32
	data, flags, tm uint32
	_               uint32
	extra           uintptr
}

// ev is one input event, seen either by a hook or by its source.
type ev struct {
	wheel, media bool
	vk, scan     uint16
	up, ext      bool
	delta        int16
	keydial      bool
	at           time.Time
}

func (a ev) same(b ev) bool {
	if a.wheel != b.wheel || a.media != b.media {
		return false
	}
	switch {
	case a.wheel:
		return (a.delta > 0) == (b.delta > 0)
	case a.media:
		return a.up == b.up
	default:
		return a.scan == b.scan && a.up == b.up && a.ext == b.ext
	}
}

func (a ev) String() string {
	switch {
	case a.wheel:
		if a.delta > 0 {
			return "wheel up"
		}
		return "wheel down"
	case a.media:
		return fmt.Sprintf("media vk=0x%02x %s", a.vk, upDown(a.up))
	}
	return fmt.Sprintf("key vk=0x%02x scan=0x%02x %s", a.vk, a.scan, upDown(a.up))
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

type blocker struct {
	start time.Time
	held  []ev // hook events waiting for their source
	early []ev // sources that came before their hook event
	stats map[string]int
	names map[uintptr]string
}

var bl = &blocker{start: time.Now(), stats: map[string]int{}, names: map[uintptr]string{}}

func (b *blocker) log(format string, a ...any) {
	fmt.Printf("%6.2fs  "+format+"\n", append([]any{time.Since(b.start).Seconds()}, a...)...)
}

// hooked: a hardware event reached a hook. true = block it (for now).
func (b *blocker) hooked(e ev) bool {
	for i, s := range b.early {
		if s.same(e) {
			b.early = append(b.early[:i], b.early[i+1:]...)
			if s.keydial {
				b.stats["Keydial, swallowed (source came first)"]++
				b.log("SWALLOW %v (source first)", e)
				return true
			}
			b.stats["other device, passed (source came first)"]++
			return false
		}
	}
	e.at = time.Now()
	b.held = append(b.held, e)
	return true
}

// sourced: Raw Input / consumer HID said where an event came from.
func (b *blocker) sourced(s ev) {
	for i, h := range b.held {
		if h.same(s) {
			b.held = append(b.held[:i], b.held[i+1:]...)
			wait := time.Since(h.at)
			if s.keydial {
				b.stats["Keydial, swallowed (hook came first)"]++
				b.log("SWALLOW %v (held %v)", h, wait.Round(time.Microsecond))
			} else {
				b.stats["other device, re-sent (hook came first)"]++
				replay(h)
			}
			return
		}
	}
	s.at = time.Now()
	b.early = append(b.early, s)
}

func (b *blocker) tick() {
	now := time.Now()
	for len(b.held) > 0 && now.Sub(b.held[0].at) > holdFor {
		h := b.held[0]
		b.held = b.held[1:]
		b.stats["no source in time, re-sent"]++
		b.log("TIMEOUT %v: no source, re-sent", h)
		replay(h)
	}
	for len(b.early) > 0 && now.Sub(b.early[0].at) > keepEarl {
		b.stats["source without hook event, dropped"]++
		b.early = b.early[1:]
	}
}

func replay(e ev) {
	if e.wheel {
		in := mouseInput{typ: 0, data: uint32(int32(e.delta)), flags: 0x0800, extra: marker}
		pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		return
	}
	in := keyInput{typ: 1, vk: e.vk, scan: e.scan, extra: marker}
	if e.ext {
		in.flags |= 1
	}
	if e.up {
		in.flags |= 2
	}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func isMedia(vk uint32) bool { return vk >= 0xAD && vk <= 0xB7 }

func keyboardProc(code, wParam, lParam uintptr) uintptr {
	if int32(code) >= 0 {
		k := (*kbdHookStruct)(unsafe.Pointer(lParam))
		if k.flags&0x10 == 0 { // not injected
			e := ev{vk: uint16(k.vk), scan: uint16(k.scan), up: k.flags&0x80 != 0, ext: k.flags&1 != 0, media: isMedia(k.vk)}
			if bl.hooked(e) {
				return 1
			}
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, code, wParam, lParam)
	return r
}

func mouseProc(code, wParam, lParam uintptr) uintptr {
	if int32(code) >= 0 && wParam == 0x020A { // WM_MOUSEWHEEL
		m := (*msHookStruct)(unsafe.Pointer(lParam))
		if m.flags&1 == 0 { // not injected
			if bl.hooked(ev{wheel: true, delta: int16(m.data >> 16)}) {
				return 1
			}
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, code, wParam, lParam)
	return r
}

// rawSource turns a WM_INPUT into a source event.
func rawSource(lParam uintptr) (ev, bool) {
	var buf [256]byte
	size := uint32(len(buf))
	if r, _, _ := pGetRawInputData.Call(lParam, 0x10000003, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 24); int32(r) <= 0 {
		return ev{}, false
	}
	typ := *(*uint32)(unsafe.Pointer(&buf[0]))
	hdev := *(*uintptr)(unsafe.Pointer(&buf[8]))
	if hdev == 0 {
		return ev{}, false // injected (e.g. our own re-sends)
	}
	name, ok := bl.names[hdev]
	if !ok {
		name = strings.ToLower(deviceName(hdev))
		bl.names[hdev] = name
	}
	keydial := strings.Contains(name, "vid&02256c")
	d := buf[24:]
	switch typ {
	case 1:
		vk := *(*uint16)(unsafe.Pointer(&d[6]))
		if isMedia(uint32(vk)) {
			return ev{}, false // media keys: attributed via the consumer collection
		}
		flags := *(*uint16)(unsafe.Pointer(&d[2]))
		return ev{vk: vk, scan: *(*uint16)(unsafe.Pointer(&d[0])), up: flags&1 != 0, ext: flags&2 != 0, keydial: keydial}, true
	case 0:
		if *(*uint16)(unsafe.Pointer(&d[4]))&0x0400 == 0 {
			return ev{}, false
		}
		return ev{wheel: true, delta: *(*int16)(unsafe.Pointer(&d[6])), keydial: keydial}, true
	}
	return ev{}, false
}

// readConsumer forwards the Keydial's consumer reports (media keys) to the hook thread.
func readConsumer(hwnd uintptr) {
	pnpFilter = `^HID\\.*VID&02256C_PID&8251`
	for _, p := range findPaths() {
		d, err := open(p)
		if err != nil || d.page != 0x000C {
			continue
		}
		go func() {
			for {
				buf := make([]byte, d.inLen)
				var n uint32
				if syscall.ReadFile(d.h, buf, &n, nil) != nil || n < 3 {
					return
				}
				up := uintptr(0)
				if buf[1] == 0 && buf[2] == 0 {
					up = 1
				}
				pPostMessage.Call(hwnd, wmMedia, up, 0)
			}
		}()
		return
	}
	fmt.Println("Keydial consumer collection not found: media keys won't be attributed")
}

func blockMain() {
	runtime.LockOSThread()
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0)
	if hwnd == 0 {
		fmt.Println("window:", err)
		return
	}
	devs := []rawDevice{{1, 6, 0x100, hwnd}, {1, 2, 0x100, hwnd}}
	if ok, _, err := pRegisterRawInput.Call(uintptr(unsafe.Pointer(&devs[0])), uintptr(len(devs)), unsafe.Sizeof(devs[0])); ok == 0 {
		fmt.Println("register raw input:", err)
		return
	}
	readConsumer(hwnd)
	mod, _, _ := pGetModuleHandle.Call(0)
	if h, _, err := pSetWindowsHookEx.Call(13, syscall.NewCallback(keyboardProc), mod, 0); h == 0 {
		fmt.Println("keyboard hook:", err)
		return
	}
	if h, _, err := pSetWindowsHookEx.Call(14, syscall.NewCallback(mouseProc), mod, 0); h == 0 {
		fmt.Println("mouse hook:", err)
		return
	}
	pSetTimer.Call(hwnd, 1, 15, 0)
	go func() { time.Sleep(120 * time.Second); pPostMessage.Call(hwnd, wmStop, 0, 0) }()
	fmt.Println("READY: Keydial keys and dial are blocked for 2 minutes")

	var msg [64]byte
	for {
		if r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0); int32(r) <= 0 {
			break
		}
		wParam := *(*uintptr)(unsafe.Pointer(&msg[16]))
		lParam := *(*uintptr)(unsafe.Pointer(&msg[24]))
		switch *(*uint32)(unsafe.Pointer(&msg[8])) {
		case wmInput:
			if s, ok := rawSource(lParam); ok {
				bl.sourced(s)
			}
		case wmMedia:
			bl.sourced(ev{media: true, up: wParam == 1, keydial: true})
		case wmTimer:
			bl.tick()
		case wmStop:
			var keys []string
			for k := range bl.stats {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Println("\nsummary:")
			for _, k := range keys {
				fmt.Printf("  %4d  %s\n", bl.stats[k], k)
			}
			os.Exit(0)
		}
	}
}
