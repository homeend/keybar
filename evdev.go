//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	eventSize = 24         // struct input_event on 64-bit: timeval(16) + type + code + value
	eviocgrab = 0x40044590 // _IOW('E', 0x90, int)
)

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// ProcDevice is one block of /proc/bus/input/devices.
type ProcDevice struct {
	Name, Vendor, Product, Uniq string
	Events                      []string // /dev/input/eventN
	Keyboard                    bool     // has the "kbd" handler
}

func listInputDevices() []ProcDevice {
	f, err := os.Open("/proc/bus/input/devices")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ProcDevice
	var cur ProcDevice
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" || len(cur.Events) > 0 {
				out = append(out, cur)
			}
			cur = ProcDevice{}
		case strings.HasPrefix(line, "I:"):
			for _, f := range strings.Fields(line[2:]) {
				if v, ok := strings.CutPrefix(f, "Vendor="); ok {
					cur.Vendor = strings.ToLower(v)
				}
				if v, ok := strings.CutPrefix(f, "Product="); ok {
					cur.Product = strings.ToLower(v)
				}
			}
		case strings.HasPrefix(line, "N: Name="):
			cur.Name = strings.Trim(line[len("N: Name="):], `"`)
		case strings.HasPrefix(line, "U: Uniq="):
			cur.Uniq = line[len("U: Uniq="):]
		case strings.HasPrefix(line, "H: Handlers="):
			for _, h := range strings.Fields(line[len("H: Handlers="):]) {
				if h == "kbd" {
					cur.Keyboard = true
				}
				if strings.HasPrefix(h, "event") {
					cur.Events = append(cur.Events, "/dev/input/"+h)
				}
			}
		}
	}
	if cur.Name != "" || len(cur.Events) > 0 {
		out = append(out, cur)
	}
	return out
}

func (m DeviceMatch) matches(d ProcDevice) bool {
	if strings.HasPrefix(d.Name, virtualPrefix) {
		return false
	}
	if m.Vendor == "" && m.Product == "" && m.NameContains == "" {
		return false
	}
	if m.Vendor != "" && !strings.EqualFold(strings.TrimLeft(m.Vendor, "0"), strings.TrimLeft(d.Vendor, "0")) {
		return false
	}
	if m.Product != "" && !strings.EqualFold(strings.TrimLeft(m.Product, "0"), strings.TrimLeft(d.Product, "0")) {
		return false
	}
	if m.NameContains != "" && !strings.Contains(strings.ToLower(d.Name), strings.ToLower(m.NameContains)) {
		return false
	}
	return true
}

type openDevice struct {
	path, name string
	file       *os.File
	grabbed    bool // EVIOCGRAB: only Keybar receives its events
	ignored    bool // udev tells the desktop to ignore it (LIBINPUT_IGNORE_DEVICE)
	// A button was already down when the device appeared (pressing a button
	// wakes the Keydial up), so the desktop has seen that press. Grabbing now
	// would hide the release and leave the key stuck down and repeating; wait
	// until every button is up, then grab. Touched by the read goroutine only.
	waitGrab bool
}

// exclusive: the desktop doesn't see this device's events, so Keybar must
// send anything that should reach it (factory keys, pointer movement).
func (od *openDevice) exclusive() bool { return od.grabbed || od.ignored }

func (od *openDevice) grab() bool {
	sc, err := od.file.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	sc.Control(func(fd uintptr) {
		if err := ioctl(fd, eviocgrab, 1); err != nil {
			log.Printf("grab %s: %v", od.path, err)
		} else {
			ok = true
		}
	})
	return ok
}

// keysDown reports whether any key or button of the device is held now.
func (od *openDevice) keysDown() bool {
	var bits [96]byte // KEY_MAX+1 bits
	sc, err := od.file.SyscallConn()
	if err != nil {
		return false
	}
	var ierr error
	sc.Control(func(fd uintptr) {
		req := uintptr(2<<30 | len(bits)<<16 | 'E'<<8 | 0x18) // EVIOCGKEY(len)
		ierr = ioctl(fd, req, uintptr(unsafe.Pointer(&bits[0])))
	})
	if ierr != nil {
		return false
	}
	for _, b := range bits {
		if b != 0 {
			return true
		}
	}
	return false
}

// libinputIgnores reports whether udev marked the event node so that the
// desktop (libinput) ignores it: see install-udev.sh.
func libinputIgnores(path string) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false
	}
	minor := st.Rdev&0xff | (st.Rdev>>12)&0xfff00
	b, err := os.ReadFile(fmt.Sprintf("/run/udev/data/c%d:%d", 13, minor))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "\nE:LIBINPUT_IGNORE_DEVICE=1\n")
}

// DeviceManager finds the Keydial's event nodes, keeps them open across
// Bluetooth reconnects, optionally grabs them, and feeds events to the engine.
type DeviceManager struct {
	store    *ConfigStore
	emitter  *Emitter
	out      chan<- InputEvent
	onChange func()
	onClose  func()     // a device closed: release anything held
	onHost   func(bool) // the Keydial connected here (true) or left (false)

	mu     sync.Mutex
	open   map[string]*openDevice
	pause  bool
	scanMu sync.Mutex // one scan at a time (timer, /dev/input watcher, Reset)
}

func NewDeviceManager(store *ConfigStore, em *Emitter, out chan<- InputEvent, onChange func()) *DeviceManager {
	return &DeviceManager{store: store, emitter: em, out: out, onChange: onChange, open: map[string]*openDevice{}}
}

func (m *DeviceManager) Run() {
	go m.watch()
	for {
		m.scan()
		time.Sleep(2 * time.Second)
	}
}

// shouldGrab: only take exclusive ownership when we can re-emit keys ourselves,
// otherwise unmapped buttons would go dead.
func (m *DeviceManager) shouldGrab() bool {
	return m.store.Get().Grab && m.emitter.Ready() && !m.paused()
}

func (m *DeviceManager) paused() bool {
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

func (m *DeviceManager) Paused() bool { return m.paused() }

// watch rescans as soon as /dev/input changes (a node appears, or udev sets
// its permissions), so a reconnected Keydial is taken over within
// milliseconds instead of at the next 2-second scan.
func (m *DeviceManager) watch() {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		log.Printf("watch /dev/input: %v", err)
		return
	}
	if _, err := syscall.InotifyAddWatch(fd, "/dev/input", syscall.IN_CREATE|syscall.IN_ATTRIB); err != nil {
		log.Printf("watch /dev/input: %v", err)
		syscall.Close(fd)
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			log.Printf("watch /dev/input: %v", err)
			return
		}
		if n > 0 {
			m.scan()
		}
	}
}

func (m *DeviceManager) scan() {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	match := m.store.Get().Device
	grab := m.shouldGrab()
	changed, had := false, m.Connected()
	for _, d := range listInputDevices() {
		if !match.matches(d) {
			continue
		}
		for _, path := range d.Events {
			m.mu.Lock()
			_, have := m.open[path]
			m.mu.Unlock()
			if have {
				continue
			}
			f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				log.Printf("open %s (%s): %v", path, d.Name, err)
				continue
			}
			od := &openDevice{path: path, name: d.Name, file: f, ignored: libinputIgnores(path)}
			if grab {
				if !od.ignored && od.keysDown() {
					od.waitGrab = true
				} else {
					od.grabbed = od.grab()
				}
			}
			m.mu.Lock()
			m.open[path] = od
			m.mu.Unlock()
			log.Printf("opened %s %q grabbed=%v desktop-ignores=%v", path, d.Name, od.grabbed, od.ignored)
			if od.waitGrab {
				log.Printf("%s: a button is held (it woke the Keydial up); taking it over once released", path)
			}
			changed = true
			go m.read(od)
		}
	}
	if changed && !had && m.onHost != nil {
		m.onHost(true)
	}
	if changed && m.onChange != nil {
		m.onChange()
	}
}

// Connected: at least one of the Keydial's event nodes is open.
func (m *DeviceManager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.open) > 0
}

func (m *DeviceManager) read(od *openDevice) {
	buf := make([]byte, eventSize*64)
	for {
		n, err := od.file.Read(buf)
		if err != nil {
			break
		}
		paused := m.paused()
		for off := 0; off+eventSize <= n; off += eventSize {
			b := buf[off : off+eventSize]
			ev := InputEvent{
				Type:  binary.LittleEndian.Uint16(b[16:]),
				Code:  binary.LittleEndian.Uint16(b[18:]),
				Value: int32(binary.LittleEndian.Uint32(b[20:])),
			}
			if ev.Type != evKey && ev.Type != evRel {
				continue
			}
			switch {
			case paused:
				// Mappings are off. If the desktop ignores the device, hand it
				// the factory keys ourselves; otherwise it already has them.
				if od.ignored {
					m.forward(ev)
				}
			case od.waitGrab:
				// The desktop gets these until the held button is released.
			default:
				m.out <- ev
			}
		}
		if od.waitGrab && !od.keysDown() {
			od.waitGrab = false
			ok := od.grab()
			m.mu.Lock()
			od.grabbed = ok
			m.mu.Unlock()
			log.Printf("%s: buttons released, grabbed=%v", od.path, ok)
			if m.onChange != nil {
				m.onChange()
			}
		}
	}
	od.file.Close()
	m.mu.Lock()
	if m.open[od.path] == od {
		delete(m.open, od.path)
	}
	gone := len(m.open) == 0
	m.mu.Unlock()
	log.Printf("closed %s", od.path)
	if gone && m.onHost != nil {
		m.onHost(false)
	}
	if m.onClose != nil {
		m.onClose()
	}
	if m.onChange != nil {
		m.onChange()
	}
}

// forward passes a raw event on unchanged (used while paused).
func (m *DeviceManager) forward(ev InputEvent) {
	switch ev.Type {
	case evKey:
		if ev.Value != 2 { // the desktop repeats held keys itself
			m.emitter.Key(ev.Code, ev.Value == 1)
		}
	case evRel:
		m.emitter.Rel(ev.Code, ev.Value)
	}
}

// Reset closes all devices; the next scan reopens them with current settings.
func (m *DeviceManager) Reset() {
	m.mu.Lock()
	for _, od := range m.open {
		od.file.Close() // read loop exits and removes it
	}
	m.mu.Unlock()
	go func() { time.Sleep(200 * time.Millisecond); m.scan() }()
}

func (m *DeviceManager) Devices() []DeviceInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []DeviceInfo{}
	for _, od := range m.open {
		out = append(out, DeviceInfo{Path: od.path, Name: od.name, Grabbed: od.exclusive()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (m *DeviceManager) AnyGrabbed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, od := range m.open {
		if od.exclusive() {
			return true
		}
	}
	return false
}

// batteryPercent asks BlueZ for the device's battery level (BLE battery service).
func batteryPercent(match DeviceMatch) int {
	for _, d := range listInputDevices() {
		if !match.matches(d) || d.Uniq == "" {
			continue
		}
		out, err := exec.Command("bluetoothctl", "info", strings.ToUpper(d.Uniq)).Output()
		if err != nil {
			return -1
		}
		for _, line := range strings.Split(string(out), "\n") {
			// "Battery Percentage: 0x32 (50)"
			if _, rest, ok := strings.Cut(line, "Battery Percentage:"); ok {
				if a := strings.Index(rest, "("); a >= 0 {
					if b := strings.Index(rest[a:], ")"); b > 0 {
						if n, err := strconv.Atoi(rest[a+1 : a+b]); err == nil {
							return n
						}
					}
				}
			}
		}
		return -1
	}
	return -1
}
