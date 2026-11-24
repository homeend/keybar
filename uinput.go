package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
	"unsafe"
)

const (
	uiSetEvBit  = 0x40045564
	uiSetKeyBit = 0x40045565
	uiSetRelBit = 0x40045566
	uiDevSetup  = 0x405c5503
	uiDevCreate = 0x5501

	busVirtual    = 0x06
	virtualPrefix = "Keybar virtual"
)

type uinputSetup struct {
	Bustype, Vendor, Product, Version uint16
	Name                              [80]byte
	FFEffectsMax                      uint32
}

type uinputDev struct{ f *os.File }

func newUinput(name string, product uint16, keys []uint16, rels []uint16) (*uinputDev, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	fd := f.Fd()
	fail := func(what string, err error) (*uinputDev, error) {
		f.Close()
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(keys) > 0 {
		if err := ioctl(fd, uiSetEvBit, evKey); err != nil {
			return fail("UI_SET_EVBIT key", err)
		}
		for _, k := range keys {
			if err := ioctl(fd, uiSetKeyBit, uintptr(k)); err != nil {
				return fail("UI_SET_KEYBIT", err)
			}
		}
	}
	if len(rels) > 0 {
		if err := ioctl(fd, uiSetEvBit, evRel); err != nil {
			return fail("UI_SET_EVBIT rel", err)
		}
		for _, r := range rels {
			if err := ioctl(fd, uiSetRelBit, uintptr(r)); err != nil {
				return fail("UI_SET_RELBIT", err)
			}
		}
	}
	setup := uinputSetup{Bustype: busVirtual, Vendor: 0x1209, Product: product, Version: 1}
	copy(setup.Name[:], name)
	if err := ioctl(fd, uiDevSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
		return fail("UI_DEV_SETUP", err)
	}
	if err := ioctl(fd, uiDevCreate, 0); err != nil {
		return fail("UI_DEV_CREATE", err)
	}
	return &uinputDev{f: f}, nil
}

func (u *uinputDev) emit(typ, code uint16, value int32) {
	var b [eventSize]byte
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	if _, err := u.f.Write(b[:]); err != nil {
		log.Printf("uinput write: %v", err)
	}
}

func (u *uinputDev) sync() { u.emit(evSyn, synReport, 0) }

// Emitter owns a virtual keyboard and a virtual mouse. Two devices keep udev /
// libinput from classifying one combined device as something odd (joystick, tablet).
type Emitter struct {
	mu      sync.Mutex
	kbd     *uinputDev
	mouse   *uinputDev
	err     error
	onReady func()
	down    map[uint16]bool // keys/buttons currently held on the virtual devices
}

func NewEmitter(onReady func()) *Emitter {
	e := &Emitter{onReady: onReady, down: map[uint16]bool{}}
	e.tryCreate()
	go func() {
		for range time.Tick(5 * time.Second) {
			if !e.Ready() && e.tryCreate() && e.onReady != nil {
				e.onReady()
			}
		}
	}()
	return e
}

func (e *Emitter) tryCreate() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kbd != nil {
		return true
	}
	var keys []uint16
	for c := uint16(1); c < 0x100; c++ {
		keys = append(keys, c)
	}
	for c := uint16(0x160); c < 0x2c0; c++ { // KEY_OK.. (skips BTN_* and trigger-happy ranges)
		keys = append(keys, c)
	}
	kbd, err := newUinput(virtualPrefix+" keyboard", 0x4b01, keys, nil)
	if err != nil {
		if e.err == nil || e.err.Error() != err.Error() {
			log.Printf("uinput unavailable: %v (run: sudo ./install-udev.sh)", err)
		}
		e.err = err
		return false
	}
	var btns []uint16
	for c := uint16(btnLeft); c <= btnTask; c++ {
		btns = append(btns, c)
	}
	mouse, err := newUinput(virtualPrefix+" mouse", 0x4b02, btns,
		[]uint16{relX, relY, relWheel, relHWheel, relWheelHiRes, relHWheelHiRes})
	if err != nil {
		kbd.f.Close()
		e.err = err
		return false
	}
	e.kbd, e.mouse, e.err = kbd, mouse, nil
	log.Printf("uinput devices created")
	time.Sleep(150 * time.Millisecond) // let the compositor pick them up
	return true
}

func (e *Emitter) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kbd != nil
}

func (e *Emitter) Err() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *Emitter) Key(code uint16, down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kbd == nil {
		return
	}
	d := e.kbd
	if code >= btnLeft && code <= btnTask {
		d = e.mouse
	}
	v := int32(0)
	if down {
		v = 1
		e.down[code] = true
	} else {
		delete(e.down, code)
	}
	d.emit(evKey, code, v)
	d.sync()
}

// ReleaseAll lifts every key and button Keybar is holding, so nothing can stay
// stuck (device dropped mid-press, restart, quit).
func (e *Emitter) ReleaseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kbd == nil {
		return
	}
	for code := range e.down {
		d := e.kbd
		if code >= btnLeft && code <= btnTask {
			d = e.mouse
		}
		d.emit(evKey, code, 0)
		d.sync()
		log.Printf("released stuck %s", keyName(code))
	}
	e.down = map[uint16]bool{}
}

func (e *Emitter) Rel(code uint16, v int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mouse == nil || v == 0 {
		return
	}
	switch code {
	case relWheel:
		e.mouse.emit(evRel, relWheel, v)
		e.mouse.emit(evRel, relWheelHiRes, v*120)
	case relHWheel:
		e.mouse.emit(evRel, relHWheel, v)
		e.mouse.emit(evRel, relHWheelHiRes, v*120)
	case relX, relY:
		e.mouse.emit(evRel, code, v)
	default:
		return
	}
	e.mouse.sync()
}

func (e *Emitter) Move(dx, dy int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mouse == nil {
		return
	}
	if dx != 0 {
		e.mouse.emit(evRel, relX, dx)
	}
	if dy != 0 {
		e.mouse.emit(evRel, relY, dy)
	}
	e.mouse.sync()
}
