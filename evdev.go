//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
	grabbed    bool
}

// DeviceManager finds the Keydial's event nodes, keeps them open across
// Bluetooth reconnects, optionally grabs them, and feeds events to the engine.
type DeviceManager struct {
	store    *ConfigStore
	emitter  *Emitter
	out      chan<- InputEvent
	onChange func()
	onClose  func() // a device closed: release anything held

	mu    sync.Mutex
	open  map[string]*openDevice
	pause bool
}

func NewDeviceManager(store *ConfigStore, em *Emitter, out chan<- InputEvent, onChange func()) *DeviceManager {
	return &DeviceManager{store: store, emitter: em, out: out, onChange: onChange, open: map[string]*openDevice{}}
}

func (m *DeviceManager) Run() {
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

func (m *DeviceManager) scan() {
	match := m.store.Get().Device
	grab := m.shouldGrab()
	changed := false
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
			od := &openDevice{path: path, name: d.Name, file: f}
			if grab {
				if sc, err := f.SyscallConn(); err == nil {
					sc.Control(func(fd uintptr) {
						if err := ioctl(fd, eviocgrab, 1); err != nil {
							log.Printf("grab %s: %v", path, err)
						} else {
							od.grabbed = true
						}
					})
				}
			}
			m.mu.Lock()
			m.open[path] = od
			m.mu.Unlock()
			log.Printf("opened %s %q grabbed=%v", path, d.Name, od.grabbed)
			changed = true
			go m.read(od)
		}
	}
	if changed && m.onChange != nil {
		m.onChange()
	}
}

func (m *DeviceManager) read(od *openDevice) {
	buf := make([]byte, eventSize*64)
	for {
		n, err := od.file.Read(buf)
		if err != nil {
			break
		}
		for off := 0; off+eventSize <= n; off += eventSize {
			b := buf[off : off+eventSize]
			ev := InputEvent{
				Type:  binary.LittleEndian.Uint16(b[16:]),
				Code:  binary.LittleEndian.Uint16(b[18:]),
				Value: int32(binary.LittleEndian.Uint32(b[20:])),
			}
			if ev.Type == evKey || ev.Type == evRel {
				m.out <- ev
			}
		}
	}
	od.file.Close()
	m.mu.Lock()
	if m.open[od.path] == od {
		delete(m.open, od.path)
	}
	m.mu.Unlock()
	log.Printf("closed %s", od.path)
	if m.onClose != nil {
		m.onClose()
	}
	if m.onChange != nil {
		m.onChange()
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
		out = append(out, DeviceInfo{Path: od.path, Name: od.name, Grabbed: od.grabbed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (m *DeviceManager) AnyGrabbed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, od := range m.open {
		if od.grabbed {
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
