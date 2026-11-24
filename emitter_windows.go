//go:build windows

package main

import (
	"log"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Emitter sends keys and mouse events with SendInput. Keybar stores keys by
// their Linux names (KEY_LEFTMETA = the Windows key); vkOf maps them to
// Windows virtual-key codes. Note: Windows does not let a normal program type
// into windows running as administrator.

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procSendInput  = user32.NewProc("SendInput")
	procMapVirtual = user32.NewProc("MapVirtualKeyW")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfXDown      = 0x0080
	mouseeventfXUp        = 0x0100
	mouseeventfWheel      = 0x0800
	mouseeventfHWheel     = 0x1000
)

// INPUT is 40 bytes on 64-bit Windows: type, padding, then a 32-byte union.
type keyboardInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	_     uint32
	extra uint64
	_     uint64
}

type mouseInput struct {
	typ   uint32
	_     uint32
	dx    int32
	dy    int32
	data  uint32
	flags uint32
	time  uint32
	_     uint32
	extra uint64
}

var lastInputErr string

func sendInput(p unsafe.Pointer) {
	n, _, err := procSendInput.Call(1, uintptr(p), 40)
	if n == 0 { // blocked (e.g. the foreground app runs as administrator, or the screen is locked)
		if msg := err.Error(); msg != lastInputErr {
			lastInputErr = msg
			log.Printf("SendInput failed: %v", err)
		}
	} else {
		lastInputErr = ""
	}
}

type vk struct {
	code uint16
	ext  bool // extended key (right-hand modifiers, navigation block, …)
}

// vkOf maps Linux key codes (see keycodes_gen.go) to Windows virtual keys, US layout.
var vkOf = func() map[uint16]vk {
	m := map[uint16]vk{
		1: {0x1B, false}, 11: {0x30, false}, 12: {0xBD, false}, 13: {0xBB, false}, 14: {0x08, false}, 15: {0x09, false},
		26: {0xDB, false}, 27: {0xDD, false}, 28: {0x0D, false}, 29: {0xA2, false}, 39: {0xBA, false}, 40: {0xDE, false},
		41: {0xC0, false}, 42: {0xA0, false}, 43: {0xDC, false}, 51: {0xBC, false}, 52: {0xBE, false}, 53: {0xBF, false},
		54: {0xA1, false}, 55: {0x6A, false}, 56: {0xA4, false}, 57: {0x20, false}, 58: {0x14, false},
		69: {0x90, false}, 70: {0x91, false}, 71: {0x67, false}, 72: {0x68, false}, 73: {0x69, false}, 74: {0x6D, false},
		75: {0x64, false}, 76: {0x65, false}, 77: {0x66, false}, 78: {0x6B, false}, 79: {0x61, false}, 80: {0x62, false},
		81: {0x63, false}, 82: {0x60, false}, 83: {0x6E, false}, 86: {0xE2, false}, 87: {0x7A, false}, 88: {0x7B, false},
		96: {0x0D, true}, 97: {0xA3, true}, 98: {0x6F, true}, 99: {0x2C, true}, 100: {0xA5, true},
		102: {0x24, true}, 103: {0x26, true}, 104: {0x21, true}, 105: {0x25, true}, 106: {0x27, true},
		107: {0x23, true}, 108: {0x28, true}, 109: {0x22, true}, 110: {0x2D, true}, 111: {0x2E, true},
		113: {0xAD, false}, 114: {0xAE, false}, 115: {0xAF, false}, 117: {0xBB, false}, 119: {0x13, false},
		125: {0x5B, true}, 126: {0x5C, true}, 127: {0x5D, true},
		140: {0xB7, false}, 155: {0xB4, false}, 158: {0xA6, false}, 159: {0xA7, false},
		163: {0xB0, false}, 164: {0xB3, false}, 165: {0xB1, false}, 166: {0xB2, false},
		172: {0xAC, false}, 173: {0xA8, false}, 217: {0xAA, false},
	}
	for i, c := range "1234567890"[:9] { // KEY_1..KEY_9 = 2..10
		m[uint16(2+i)] = vk{uint16(c), false}
	}
	rows := []struct {
		from  uint16
		chars string
	}{{16, "QWERTYUIOP"}, {30, "ASDFGHJKL"}, {44, "ZXCVBNM"}}
	for _, r := range rows {
		for i, c := range r.chars {
			m[r.from+uint16(i)] = vk{uint16(c), false}
		}
	}
	for i := uint16(0); i < 10; i++ { // F1..F10 = 59..68
		m[59+i] = vk{0x70 + i, false}
	}
	for i := uint16(0); i < 12; i++ { // F13..F24 = 183..194
		m[183+i] = vk{0x7C + i, false}
	}
	return m
}()

type Emitter struct {
	mu      sync.Mutex
	down    map[uint16]bool
	onReady func()
	warned  map[uint16]bool
}

func NewEmitter(onReady func()) *Emitter {
	return &Emitter{down: map[uint16]bool{}, warned: map[uint16]bool{}, onReady: onReady}
}

func (e *Emitter) Ready() bool { return true }
func (e *Emitter) Err() string { return "" }

func (e *Emitter) Key(code uint16, down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if down {
		e.down[code] = true
	} else {
		delete(e.down, code)
	}
	e.key(code, down)
}

func (e *Emitter) key(code uint16, down bool) {
	if code >= btnLeft && code <= btnTask {
		e.button(code, down)
		return
	}
	v, ok := vkOf[code]
	if !ok {
		if !e.warned[code] {
			e.warned[code] = true
			log.Printf("no Windows key for %s", keyName(code))
		}
		return
	}
	scan, _, _ := procMapVirtual.Call(uintptr(v.code), 0)
	in := keyboardInput{typ: inputKeyboard, vk: v.code, scan: uint16(scan)}
	if v.ext {
		in.flags |= keyeventfExtended
	}
	if !down {
		in.flags |= keyeventfKeyUp
	}
	sendInput(unsafe.Pointer(&in))
}

func (e *Emitter) button(code uint16, down bool) {
	in := mouseInput{typ: inputMouse}
	pick := func(d, u uint32) uint32 {
		if down {
			return d
		}
		return u
	}
	switch code {
	case btnLeft:
		in.flags = pick(mouseeventfLeftDown, mouseeventfLeftUp)
	case btnRight:
		in.flags = pick(mouseeventfRightDown, mouseeventfRightUp)
	case btnMiddle:
		in.flags = pick(mouseeventfMiddleDown, mouseeventfMiddleUp)
	case btnSide, btnBack:
		in.flags, in.data = pick(mouseeventfXDown, mouseeventfXUp), 1
	case btnExtra, btnForward:
		in.flags, in.data = pick(mouseeventfXDown, mouseeventfXUp), 2
	default:
		return
	}
	sendInput(unsafe.Pointer(&in))
}

// ReleaseAll lifts every key and button Keybar is holding.
func (e *Emitter) ReleaseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for code := range e.down {
		e.key(code, false)
		log.Printf("released stuck %s", keyName(code))
	}
	e.down = map[uint16]bool{}
}

func (e *Emitter) Rel(code uint16, v int32) {
	if v == 0 {
		return
	}
	in := mouseInput{typ: inputMouse}
	switch code {
	case relWheel:
		in.flags, in.data = mouseeventfWheel, uint32(v*120)
	case relHWheel:
		in.flags, in.data = mouseeventfHWheel, uint32(v*120)
	case relX:
		in.flags, in.dx = mouseeventfMove, v
	case relY:
		in.flags, in.dy = mouseeventfMove, v
	default:
		return
	}
	sendInput(unsafe.Pointer(&in))
}

func (e *Emitter) Move(dx, dy int32) {
	if dx == 0 && dy == 0 {
		return
	}
	in := mouseInput{typ: inputMouse, flags: mouseeventfMove, dx: dx, dy: dy}
	sendInput(unsafe.Pointer(&in))
}
