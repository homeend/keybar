//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Reading the Keydial on Windows (see WINDOWS_HOST_PLAN.md). Windows owns the
// Keydial's keyboard and mouse, so Keybar can't open them; instead:
//
//   - Buttons: the Keydial is set up (keydialcfg keybar) so every button sends
//     Ctrl+Alt+Shift+F1…F12 or Ctrl+Alt+Win+F1…F7, keys no real keyboard sends.
//     A low-level keyboard hook blocks the F-key of such a combination and gives
//     the engine the same events Linux would. The modifiers reach apps; the ones
//     released after a blocked key are held back and re-sent behind a mask key,
//     so a lone Win/Alt/Shift doesn't open the Start menu, a menu bar or switch
//     the keyboard layout.
//   - Dial: a low-level mouse hook holds each wheel step until Raw Input says
//     which device sent it; the Keydial's go to the engine, others are re-sent.
//
// The hooks are installed only while the Keydial is connected and Keybar isn't
// paused, so a real keyboard's Ctrl+Alt+Shift+F1 is left alone otherwise.

var (
	procCreateWindowEx        = user32.NewProc("CreateWindowExW")
	procRegisterRawInput      = user32.NewProc("RegisterRawInputDevices")
	procGetRawInputData       = user32.NewProc("GetRawInputData")
	procGetRawInputDeviceInfo = user32.NewProc("GetRawInputDeviceInfoW")
	procGetRawInputDeviceList = user32.NewProc("GetRawInputDeviceList")
	procGetMessage            = user32.NewProc("GetMessageW")
	procSetWindowsHookEx      = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx   = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx        = user32.NewProc("CallNextHookEx")
	procPostMessage           = user32.NewProc("PostMessageW")
	procGetModuleHandle       = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	keyboardHookCallback      = syscall.NewCallback(keyboardHookProc)
	mouseHookCallback         = syscall.NewCallback(mouseHookProc)
	theDM                     *DeviceManager // the hook callbacks reach the manager through this
)

const (
	whKeyboardLL = 13
	whMouseLL    = 14
	wmInput      = 0x00FF // wmTimer is in overlay_windows.go
	wmMouseWheel = 0x020A
	wmApply      = 0x8001 // WM_APP+1: re-check whether the hooks should be on

	llkhfExtended = 0x01
	llkhfInjected = 0x10
	llkhfUp       = 0x80
	llmhfInjected = 0x01

	vkMask = 0xE8 // unassigned virtual key: "something else was pressed" for the shell

	wheelHold    = 60 * time.Millisecond // a wheel step whose device isn't known by then is re-sent
	wheelKeepRaw = 300 * time.Millisecond
	tickMs       = 15
	scanEvery    = 2 * time.Second
)

type kbdLLHook struct {
	vk, scan, flags, time uint32
	extra                 uintptr
}

type msLLHook struct {
	x, y              int32
	data, flags, time uint32
	extra             uintptr
}

type rawInputDevice struct {
	page, usage uint16
	flags       uint32
	target      uintptr
}

// Linux codes the engine expects (input-event-codes.h; keyByName is filled
// in an init func, too late for package variables).
const (
	codeCtrl  = 29  // KEY_LEFTCTRL
	codeShift = 42  // KEY_LEFTSHIFT
	codeAlt   = 56  // KEY_LEFTALT
	codeMeta  = 125 // KEY_LEFTMETA
)

// fkeyLinux maps the F1–F12 virtual keys back to Linux codes (the inverse of vkOf).
var fkeyLinux = func() map[uint32]uint16 {
	m := map[uint32]uint16{}
	for code, v := range vkOf {
		if v.code >= 0x70 && v.code <= 0x7B && !v.ext {
			m[uint32(v.code)] = code
		}
	}
	return m
}()

type wheelStep struct {
	up      bool
	delta   int16
	keydial bool
	at      time.Time
}

type DeviceManager struct {
	store    *ConfigStore
	emitter  *Emitter
	out      chan<- InputEvent
	onChange func()
	onClose  func()     // the Keydial went away: release anything held
	onHost   func(bool) // this machine has the Keydial now (or not)

	mu      sync.Mutex
	present string // the Keydial's Raw Input device name, "" = not connected
	pause   bool
	active  atomic.Bool // hooks installed: Keydial input is read and blocked

	// hook thread only
	hwnd      uintptr
	kbHook    uintptr
	msHook    uintptr
	mods      map[uint32]bool     // hardware modifiers down, by virtual key
	blocked   map[uint32][]uint16 // F-keys blocked on down → the Linux modifiers sent with it
	maskNext  bool                // a key was blocked: modifier releases go behind a mask key
	releasing bool                // re-sending modifier releases behind that mask, in order
	held      []wheelStep         // wheel steps waiting for their device
	early     []wheelStep         // Raw Input steps that came before their hook event
	names     map[uintptr]string
	lastScan  time.Time
}

func NewDeviceManager(store *ConfigStore, em *Emitter, out chan<- InputEvent, onChange func()) *DeviceManager {
	return &DeviceManager{store: store, emitter: em, out: out, onChange: onChange,
		mods: map[uint32]bool{}, blocked: map[uint32][]uint16{}, names: map[uintptr]string{}}
}

// Run owns the hook thread: a message-only window for Raw Input, the hooks
// and a timer, all on one locked OS thread (hooks are called on the thread
// that installed them, from its message loop).
func (m *DeviceManager) Run() {
	runtime.LockOSThread()
	theDM = m
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0) // HWND_MESSAGE
	if hwnd == 0 {
		log.Printf("keydial: message window: %v", err)
		return
	}
	m.hwnd = hwnd
	devs := []rawInputDevice{{1, 2, 0x100, hwnd}} // mice, RIDEV_INPUTSINK: wheel steps by device
	if ok, _, err := procRegisterRawInput.Call(uintptr(unsafe.Pointer(&devs[0])), 1, unsafe.Sizeof(devs[0])); ok == 0 {
		log.Printf("keydial: raw input: %v", err)
	}
	procSetTimer.Call(hwnd, 1, tickMs, 0)
	m.scan()

	var msg [64]byte
	for {
		if r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0); int32(r) <= 0 {
			return
		}
		lParam := *(*uintptr)(unsafe.Pointer(&msg[24]))
		switch *(*uint32)(unsafe.Pointer(&msg[8])) {
		case wmInput:
			if s, ok := m.rawWheel(lParam); ok {
				m.wheelSourced(s)
			}
		case wmTimer:
			m.wheelTick()
			if time.Since(m.lastScan) >= scanEvery {
				m.scan()
			}
		case wmApply:
			m.apply()
		}
	}
}

// scan looks for the Keydial among Raw Input devices (hook thread).
func (m *DeviceManager) scan() {
	m.lastScan = time.Now()
	name := m.findKeydial()
	m.mu.Lock()
	was := m.present
	m.present = name
	m.mu.Unlock()
	if (was == "") != (name == "") {
		if name != "" {
			log.Printf("keydial: connected (%s)", name)
		} else {
			log.Printf("keydial: disconnected")
		}
		m.apply()
		// off the hook thread: these may talk to the network or the engine
		connected, onHost, onClose, onChange := name != "", m.onHost, m.onClose, m.onChange
		go func() {
			if onHost != nil {
				onHost(connected)
			}
			if !connected && onClose != nil {
				onClose()
			}
			if onChange != nil {
				onChange()
			}
		}()
	}
}

// isKeydialName: a Raw Input device name (any of the Keydial's collections)
// matches the configured vendor and product.
// Bluetooth: …vid&02256c_pid&8251…, USB: …vid_256c&pid_8251…
func isKeydialName(match DeviceMatch, name string) bool {
	vendor := strings.ToLower(strings.TrimLeft(match.Vendor, "0"))
	product := strings.ToLower(strings.TrimLeft(match.Product, "0"))
	if vendor == "" && product == "" {
		return false
	}
	name = strings.ToLower(name)
	return (vendor == "" || strings.Contains(name, "vid&02"+vendor) || strings.Contains(name, "vid_"+vendor)) &&
		(product == "" || strings.Contains(name, "pid&"+product) || strings.Contains(name, "pid_"+product))
}

func (m *DeviceManager) findKeydial() string {
	match := m.store.Get().Device
	var n uint32
	const size = 16 // RAWINPUTDEVICELIST: handle + type
	procGetRawInputDeviceList.Call(0, uintptr(unsafe.Pointer(&n)), size)
	if n == 0 {
		return ""
	}
	list := make([]byte, int(n)*size)
	got, _, _ := procGetRawInputDeviceList.Call(uintptr(unsafe.Pointer(&list[0])), uintptr(unsafe.Pointer(&n)), size)
	if int32(got) <= 0 {
		return ""
	}
	for i := 0; i < int(got); i++ {
		h := *(*uintptr)(unsafe.Pointer(&list[i*size]))
		if name := strings.ToLower(rawDeviceName(h)); isKeydialName(match, name) {
			return name
		}
	}
	return ""
}

func rawDeviceName(h uintptr) string {
	var n uint32
	procGetRawInputDeviceInfo.Call(h, 0x20000007, 0, uintptr(unsafe.Pointer(&n))) // RIDI_DEVICENAME
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetRawInputDeviceInfo.Call(h, 0x20000007, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	return syscall.UTF16ToString(buf)
}

// apply installs or removes the hooks to match "connected and not paused" (hook thread).
func (m *DeviceManager) apply() {
	m.mu.Lock()
	want := m.present != "" && !m.pause
	m.mu.Unlock()
	if want == (m.kbHook != 0) {
		return
	}
	if want {
		mod, _, _ := procGetModuleHandle.Call(0)
		m.kbHook, _, _ = procSetWindowsHookEx.Call(whKeyboardLL, keyboardHookCallback, mod, 0)
		m.msHook, _, _ = procSetWindowsHookEx.Call(whMouseLL, mouseHookCallback, mod, 0)
		if m.kbHook == 0 || m.msHook == 0 {
			log.Printf("keydial: could not install the input hooks")
		}
	} else {
		for _, h := range []*uintptr{&m.kbHook, &m.msHook} {
			if *h != 0 {
				procUnhookWindowsHookEx.Call(*h)
				*h = 0
			}
		}
		for _, s := range m.held { // never keep a real wheel step
			m.replayWheel(s)
		}
		m.held, m.early, m.maskNext = nil, nil, false
		m.blocked = map[uint32][]uint16{}
	}
	m.active.Store(m.kbHook != 0)
	log.Printf("keydial: reading %v", m.kbHook != 0)
	if m.onChange != nil {
		go m.onChange()
	}
}

func (m *DeviceManager) send(ev InputEvent) {
	select {
	case m.out <- ev:
	default:
		log.Printf("keydial: engine busy, dropped %+v", ev)
	}
}

func isModifierVK(vk uint32) bool {
	switch vk {
	case 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C:
		return true
	}
	return false
}

func (m *DeviceManager) anyMods() bool {
	for _, down := range m.mods {
		if down {
			return true
		}
	}
	return false
}

// keydialMods: the Linux modifiers of a Keydial combination held now, or nil.
func (m *DeviceManager) keydialMods() []uint16 {
	ctrl := m.mods[0xA2] || m.mods[0xA3]
	alt := m.mods[0xA4] || m.mods[0xA5]
	shift := m.mods[0xA0] || m.mods[0xA1]
	win := m.mods[0x5B] || m.mods[0x5C]
	switch {
	case ctrl && alt && shift && !win:
		return []uint16{codeCtrl, codeShift, codeAlt}
	case ctrl && alt && win && !shift:
		return []uint16{codeCtrl, codeAlt, codeMeta}
	}
	return nil
}

// keyboard runs in the hook: true = swallow the event.
func (m *DeviceManager) keyboard(k *kbdLLHook) bool {
	if k.flags&llkhfInjected != 0 {
		return false // ours (or another program's) synthetic input
	}
	up := k.flags&llkhfUp != 0
	if isModifierVK(k.vk) {
		m.mods[k.vk] = !up
		if !up {
			m.releasing = false
			return false
		}
		swallow := false
		switch {
		case m.maskNext:
			// After a blocked key: a mask key first, then this release, so the
			// shell doesn't take it as a lone modifier tap. Injected input goes
			// to the end of the input queue, so the release is re-sent with it.
			m.maskNext, m.releasing, swallow = false, true, true
			resend := []keyboardInput{
				{typ: inputKeyboard, vk: vkMask, extra: keybarMark},
				{typ: inputKeyboard, vk: vkMask, flags: keyeventfKeyUp, extra: keybarMark},
				m.release(k),
			}
			procSendInput.Call(uintptr(len(resend)), uintptr(unsafe.Pointer(&resend[0])), unsafe.Sizeof(resend[0]))
		case m.releasing:
			swallow = true // the next releases follow behind the mask, in order
			in := m.release(k)
			procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
		}
		if !m.anyMods() {
			m.releasing = false
		}
		return swallow
	}
	code, isF := fkeyLinux[k.vk]
	if !up {
		m.releasing = false
		if !isF {
			return false
		}
		mods := m.keydialMods()
		if mods == nil {
			return false
		}
		if _, again := m.blocked[k.vk]; !again {
			for _, c := range mods {
				m.send(InputEvent{Type: evKey, Code: c, Value: 1})
			}
			m.send(InputEvent{Type: evKey, Code: code, Value: 1})
		}
		m.blocked[k.vk] = mods
		m.maskNext = true
		return true
	}
	mods, ok := m.blocked[k.vk]
	if !ok {
		return false
	}
	delete(m.blocked, k.vk)
	m.send(InputEvent{Type: evKey, Code: code, Value: 0})
	for i := len(mods) - 1; i >= 0; i-- {
		m.send(InputEvent{Type: evKey, Code: mods[i], Value: 0})
	}
	return true
}

// release builds the injected copy of a hardware modifier release.
func (m *DeviceManager) release(k *kbdLLHook) keyboardInput {
	in := keyboardInput{typ: inputKeyboard, vk: uint16(k.vk), scan: uint16(k.scan), flags: keyeventfKeyUp, extra: keybarMark}
	if k.flags&llkhfExtended != 0 {
		in.flags |= keyeventfExtended
	}
	return in
}

func keyboardHookProc(code, wParam, lParam uintptr) uintptr {
	if int32(code) >= 0 && theDM != nil && theDM.keyboard((*kbdLLHook)(unsafe.Pointer(lParam))) {
		return 1
	}
	r, _, _ := procCallNextHookEx.Call(0, code, wParam, lParam)
	return r
}

func mouseHookProc(code, wParam, lParam uintptr) uintptr {
	if int32(code) >= 0 && wParam == wmMouseWheel && theDM != nil {
		h := (*msLLHook)(unsafe.Pointer(lParam))
		if h.flags&llmhfInjected == 0 && theDM.wheelHooked(int16(h.data>>16)) {
			return 1
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, code, wParam, lParam)
	return r
}

// wheelHooked: a hardware wheel step reached the hook. true = hold it back.
func (m *DeviceManager) wheelHooked(delta int16) bool {
	up := delta > 0
	for i, s := range m.early {
		if s.up == up {
			m.early = append(m.early[:i], m.early[i+1:]...)
			if s.keydial {
				m.wheelToEngine(delta)
				return true
			}
			return false
		}
	}
	m.held = append(m.held, wheelStep{up: up, delta: delta, at: time.Now()})
	return true
}

// wheelSourced: Raw Input said where a wheel step came from.
func (m *DeviceManager) wheelSourced(s wheelStep) {
	for i, h := range m.held {
		if h.up == s.up {
			m.held = append(m.held[:i], m.held[i+1:]...)
			if s.keydial {
				m.wheelToEngine(h.delta)
			} else {
				m.replayWheel(h)
			}
			return
		}
	}
	if m.kbHook != 0 {
		s.at = time.Now()
		m.early = append(m.early, s)
	}
}

func (m *DeviceManager) wheelToEngine(delta int16) {
	steps := int32(delta) / 120
	if steps == 0 {
		steps = 1
		if delta < 0 {
			steps = -1
		}
	}
	m.send(InputEvent{Type: evRel, Code: relWheel, Value: steps})
}

func (m *DeviceManager) replayWheel(s wheelStep) {
	in := mouseInput{typ: inputMouse, flags: mouseeventfWheel, data: uint32(int32(s.delta)), extra: keybarMark}
	sendInput(unsafe.Pointer(&in))
}

func (m *DeviceManager) wheelTick() {
	now := time.Now()
	for len(m.held) > 0 && now.Sub(m.held[0].at) > wheelHold {
		m.replayWheel(m.held[0])
		m.held = m.held[1:]
	}
	for len(m.early) > 0 && now.Sub(m.early[0].at) > wheelKeepRaw {
		m.early = m.early[1:]
	}
}

// rawWheel turns a WM_INPUT into a wheel step with its device (hook thread).
func (m *DeviceManager) rawWheel(lParam uintptr) (wheelStep, bool) {
	var buf [128]byte
	size := uint32(len(buf))
	if r, _, _ := procGetRawInputData.Call(lParam, 0x10000003, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 24); int32(r) <= 0 {
		return wheelStep{}, false
	}
	hdev := *(*uintptr)(unsafe.Pointer(&buf[8]))
	if *(*uint32)(unsafe.Pointer(&buf[0])) != 0 || hdev == 0 { // not a mouse, or injected
		return wheelStep{}, false
	}
	d := buf[24:]
	if *(*uint16)(unsafe.Pointer(&d[4]))&0x0400 == 0 { // RI_MOUSE_WHEEL
		return wheelStep{}, false
	}
	name, ok := m.names[hdev]
	if !ok {
		name = rawDeviceName(hdev)
		m.names[hdev] = name
	}
	keydial := isKeydialName(m.store.Get().Device, name)
	return wheelStep{up: *(*int16)(unsafe.Pointer(&d[6])) > 0, keydial: keydial}, true
}

// keybarMark tags input Keybar injects itself (dwExtraInfo).
const keybarMark = 0x4B455942

func (m *DeviceManager) Reset() {
	if m.hwnd != 0 {
		procPostMessage.Call(m.hwnd, wmApply, 0, 0)
	}
}

func (m *DeviceManager) Devices() []DeviceInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.present == "" {
		return []DeviceInfo{}
	}
	return []DeviceInfo{{Path: m.present, Name: "Keydial", Grabbed: m.active.Load()}}
}

func (m *DeviceManager) AnyGrabbed() bool { return m.active.Load() }

func (m *DeviceManager) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pause
}

func (m *DeviceManager) SetPaused(p bool) {
	m.mu.Lock()
	m.pause = p
	m.mu.Unlock()
	m.Reset()
}

// Connected: the Keydial is connected to this machine now.
func (m *DeviceManager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.present != ""
}

func batteryPercent(DeviceMatch) int { return -1 }

// captureShortcut: the panel records shortcuts in the browser on Windows.
func captureShortcut(ctx context.Context, skip DeviceMatch) string { return "" }

func printDevices() {
	m := NewDeviceManager(LoadConfig(configPath()), nil, nil, nil)
	if name := m.findKeydial(); name != "" {
		fmt.Println("Keydial:", name)
		return
	}
	fmt.Println("No Keydial connected (looking for", m.store.Get().Device.Vendor+":"+m.store.Get().Device.Product+").")
}
