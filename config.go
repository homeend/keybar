package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type DeviceMatch struct {
	Vendor       string `json:"vendor"`
	Product      string `json:"product"`
	NameContains string `json:"name_contains"`
}

// Action is what a button does. Type is one of:
// keys, app, command, mouse, text, layout, machine, passthrough, none.
type Action struct {
	Type           string `json:"type"`
	Keys           string `json:"keys,omitempty"`            // "ctrl+shift+t" or "ctrl+c, ctrl+v"
	App            string `json:"app,omitempty"`             // desktop file id, e.g. "firefox_firefox.desktop"
	AppName        string `json:"app_name,omitempty"`        // display only
	Command        string `json:"command,omitempty"`         // sh -c, on press
	ReleaseCommand string `json:"release_command,omitempty"` // sh -c, on release (push-to-talk)
	Mouse          string `json:"mouse,omitempty"`           // left,right,middle,back,forward,scroll_up,scroll_down,scroll_left,scroll_right,move
	Amount         int    `json:"amount,omitempty"`          // scroll steps, or dx for move
	DY             int    `json:"dy,omitempty"`
	Text           string `json:"text,omitempty"`
	Layout         string `json:"layout,omitempty"`  // for "layout": a layout ID, or "next" / "prev"
	Machine        string `json:"machine,omitempty"` // for "machine": "pick", "next", "self" or a paired machine's ID
}

// Slot is one physical control on the Keydial: where it is drawn and which
// factory signal ("trigger") the device sends for it. Slots belong to the
// machine the device is plugged into; layouts refer to them by ID.
type Slot struct {
	ID      string `json:"id"`
	Group   string `json:"group"`   // "dial" | "keys"
	Trigger string `json:"trigger"` // what the device sends, e.g. "KEY_LEFTCTRL+KEY_Z"
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

// Key is what one slot does in one layout.
type Key struct {
	Label  string `json:"label"`
	Icon   string `json:"icon,omitempty"` // emoji shown on the key
	Action Action `json:"action"`
	Note   string `json:"note,omitempty"` // ⚠ copying from another machine couldn't keep this; cleared when edited
	Info   string `json:"info,omitempty"` // ℹ copying converted this for this OS
}

// Layout is a named set of key assignments; one is active at a time.
type Layout struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Keys map[string]Key `json:"keys"` // slot ID → key
}

// legacyBinding is the v1–v3 format (slot and action in one), read for migration only.
type legacyBinding struct {
	Slot
	Label  string `json:"label"`
	Icon   string `json:"icon,omitempty"`
	Action Action `json:"action"`
}

type Config struct {
	Version             int         `json:"version"`
	Listen              string      `json:"listen"`
	Device              DeviceMatch `json:"device"`
	Grab                bool        `json:"grab"`                 // take exclusive control of the device
	UnmappedPassthrough bool        `json:"unmapped_passthrough"` // unlinked buttons keep their factory keys
	Columns             int         `json:"columns"`
	PeerPort            int         `json:"peer_port"` // TCP port for paired machines (all interfaces)
	Slots               []Slot      `json:"slots"`
	Layouts             []Layout    `json:"layouts"`
	ActiveLayout        string      `json:"active_layout"`

	Bindings []legacyBinding `json:"bindings,omitempty"` // pre-v4 only
}

const configVersion = 4

func defaultSlots() []Slot {
	slots := []Slot{{ID: "dial_ccw", Group: "dial"}, {ID: "dial_press", Group: "dial"}, {ID: "dial_cw", Group: "dial"}}
	for i := 0; i < 18; i++ {
		slots = append(slots, Slot{ID: fmt.Sprintf("k%d", i+1), Group: "keys"})
	}
	applyKD20Layout(slots)
	return slots
}

func defaultLabel(slotID string) string {
	switch slotID {
	case "dial_ccw":
		return "Dial ↺"
	case "dial_press":
		return "Dial press"
	case "dial_cw":
		return "Dial ↻"
	}
	return strings.ToUpper(slotID)
}

func newLayout(id, name string, slots []Slot) Layout {
	l := Layout{ID: id, Name: name, Keys: map[string]Key{}}
	for _, s := range slots {
		l.Keys[s.ID] = Key{Label: defaultLabel(s.ID), Action: Action{Type: "none"}}
	}
	return l
}

func defaultConfig() *Config {
	c := &Config{
		Version: configVersion, Listen: "127.0.0.1:47830",
		Device:  DeviceMatch{Vendor: "256c", Product: "8251"},
		Grab:    true,
		Columns: 4,
		Slots:   defaultSlots(),
	}
	c.Layouts = []Layout{newLayout("default", "Default", c.Slots)}
	c.ActiveLayout = "default"
	return c
}

// kd20Layout is the physical key grid of the Keydial mini: three full rows of
// four, then three keys plus a tall key on the right (rows 4–5), and a wide
// key under the first two columns next to a single key.
var kd20Layout = map[string][4]int{ // id → x, y, w, h
	"k1": {0, 0, 1, 1}, "k2": {1, 0, 1, 1}, "k3": {2, 0, 1, 1}, "k4": {3, 0, 1, 1},
	"k5": {0, 1, 1, 1}, "k6": {1, 1, 1, 1}, "k7": {2, 1, 1, 1}, "k8": {3, 1, 1, 1},
	"k9": {0, 2, 1, 1}, "k10": {1, 2, 1, 1}, "k11": {2, 2, 1, 1}, "k12": {3, 2, 1, 1},
	"k13": {0, 3, 1, 1}, "k14": {1, 3, 1, 1}, "k15": {2, 3, 1, 1}, "k16": {2, 4, 1, 1},
	"k17": {0, 4, 2, 1}, "k18": {3, 3, 1, 2},
}

func applyKD20Layout(slots []Slot) {
	for i := range slots {
		if p, ok := kd20Layout[slots[i].ID]; ok {
			s := &slots[i]
			s.X, s.Y, s.W, s.H = p[0], p[1], p[2], p[3]
		}
	}
}

// migrate upgrades older config formats in place.
func migrate(c *Config) {
	if c.Version < 2 { // v1 drew a 4×4 grid plus two wide keys
		for i := range c.Bindings {
			if p, ok := kd20Layout[c.Bindings[i].ID]; ok {
				b := &c.Bindings[i]
				b.X, b.Y, b.W, b.H = p[0], p[1], p[2], p[3]
			}
		}
		c.Columns = 4
	}
	if c.Version < 3 { // v2 let unassigned buttons type their factory keys
		c.UnmappedPassthrough = false
		for i := range c.Bindings {
			if c.Bindings[i].Action.Type == "passthrough" {
				c.Bindings[i].Action = Action{Type: "none"}
			}
		}
	}
	if c.Version < 4 && len(c.Bindings) > 0 { // v3 had one set of bindings; v4 splits slots from layouts
		l := Layout{ID: "default", Name: "Default", Keys: map[string]Key{}}
		c.Slots = nil
		for _, b := range c.Bindings {
			c.Slots = append(c.Slots, b.Slot)
			l.Keys[b.ID] = Key{Label: b.Label, Icon: b.Icon, Action: b.Action}
		}
		c.Layouts = []Layout{l}
		c.ActiveLayout = l.ID
		c.Bindings = nil
	}
	if c.Version < configVersion {
		c.Version = configVersion
	}
}

func configPath() string { return filepath.Join(dataDir(), "config.json") }

type ConfigStore struct {
	mu        sync.RWMutex
	wmu       sync.Mutex // serialises read-modify-write of the file
	path      string
	cfg       *Config
	byTrigger map[string]*Slot
}

func LoadConfig(path string) *ConfigStore {
	s := &ConfigStore{path: path}
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var c Config
		if jerr := json.Unmarshal(data, &c); jerr != nil {
			bad := path + ".broken-" + time.Now().Format("20060102-150405")
			os.Rename(path, bad)
			log.Printf("config unreadable (%v); moved to %s, using defaults", jerr, bad)
		} else {
			cfg = &c
		}
	case !os.IsNotExist(err):
		log.Printf("read config: %v; using defaults", err)
	}
	if err := normalize(cfg); err != nil {
		log.Printf("config: %v; using defaults", err)
		cfg = defaultConfig()
	}
	s.install(cfg)
	if os.IsNotExist(err) {
		if werr := s.write(cfg); werr != nil {
			log.Printf("write config: %v", werr)
		}
	}
	return s
}

func normalize(c *Config) error {
	migrate(c)
	if c.Listen == "" {
		c.Listen = "127.0.0.1:47830"
	}
	if c.Columns <= 0 {
		c.Columns = 4
	}
	if c.PeerPort <= 0 {
		c.PeerPort = 47831
	}
	if len(c.Slots) == 0 {
		c.Slots = defaultSlots()
	}
	seen := map[string]string{}
	ids := map[string]bool{}
	for i := range c.Slots {
		s := &c.Slots[i]
		s.Trigger = strings.TrimSpace(s.Trigger)
		if s.ID == "" || ids[s.ID] {
			return fmt.Errorf("slot %d has a missing or duplicate id", i)
		}
		ids[s.ID] = true
		if s.W <= 0 {
			s.W = 1
		}
		if s.H <= 0 {
			s.H = 1
		}
		if s.Trigger != "" {
			if other, dup := seen[s.Trigger]; dup {
				return fmt.Errorf("%s and %s use the same device button", other, s.ID)
			}
			seen[s.Trigger] = s.ID
		}
	}
	if len(c.Layouts) == 0 {
		c.Layouts = []Layout{newLayout("default", "Default", c.Slots)}
	}
	layoutIDs := map[string]bool{}
	for i := range c.Layouts {
		l := &c.Layouts[i]
		l.Name = strings.TrimSpace(l.Name)
		if l.ID == "" || layoutIDs[l.ID] {
			return fmt.Errorf("layout %q has a missing or duplicate id", l.Name)
		}
		layoutIDs[l.ID] = true
		if l.Name == "" {
			return fmt.Errorf("layout %s needs a name", l.ID)
		}
		if l.Keys == nil {
			l.Keys = map[string]Key{}
		}
		for _, s := range c.Slots { // every slot has an entry, so the panel never sees gaps
			if _, ok := l.Keys[s.ID]; !ok {
				l.Keys[s.ID] = Key{Label: defaultLabel(s.ID), Action: Action{Type: "none"}}
			}
		}
		for id, k := range l.Keys {
			if !ids[id] {
				delete(l.Keys, id)
				continue
			}
			if k.Action.Type == "" {
				k.Action.Type = "none"
				l.Keys[id] = k
			}
			if k.Action.Type == "keys" && strings.TrimSpace(k.Action.Keys) != "" {
				if _, err := parseShortcut(k.Action.Keys); err != nil {
					return fmt.Errorf("%s / %s: %v", l.Name, k.Label, err)
				}
			}
		}
	}
	if !layoutIDs[c.ActiveLayout] {
		c.ActiveLayout = c.Layouts[0].ID
	}
	return nil
}

func (s *ConfigStore) install(c *Config) {
	idx := map[string]*Slot{}
	for i := range c.Slots {
		if t := c.Slots[i].Trigger; t != "" {
			idx[t] = &c.Slots[i]
		}
	}
	s.mu.Lock()
	s.cfg, s.byTrigger = c, idx
	s.mu.Unlock()
}

func (s *ConfigStore) write(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Get returns the current config. Treat it as read-only; Set replaces it.
func (s *ConfigStore) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SlotByTrigger returns the slot linked to a device signal, or nil.
func (s *ConfigStore) SlotByTrigger(trigger string) *Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byTrigger[trigger]
}

// ActiveKey returns what a slot does in the active layout.
func (s *ConfigStore) ActiveKey(slotID string) (Key, *Layout) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := s.cfg.layout(s.cfg.ActiveLayout)
	if l == nil {
		return Key{Action: Action{Type: "none"}}, nil
	}
	return l.Keys[slotID], l
}

func (c *Config) layout(id string) *Layout {
	for i := range c.Layouts {
		if c.Layouts[i].ID == id {
			return &c.Layouts[i]
		}
	}
	return nil
}

// Set saves a config from the panel. The active layout is runtime state that a
// Keydial button may have changed since the panel loaded, so it is kept unless
// the panel's copy no longer has that layout.
func (s *ConfigStore) Set(c *Config) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if cur := s.Get(); cur != nil && c.layout(cur.ActiveLayout) != nil {
		c.ActiveLayout = cur.ActiveLayout
	}
	if err := normalize(c); err != nil {
		return err
	}
	if err := s.write(c); err != nil {
		return err
	}
	s.install(c)
	return nil
}

// SwitchLayout activates a layout by ID, or "next"/"prev" in list order, and
// returns the layout now active.
func (s *ConfigStore) SwitchLayout(to string) (Layout, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	cur := *s.cfg // shallow copy: only ActiveLayout changes
	i := 0
	for j, l := range cur.Layouts {
		if l.ID == cur.ActiveLayout {
			i = j
		}
	}
	n := len(cur.Layouts)
	switch to {
	case "next", "":
		i = (i + 1) % n
	case "prev":
		i = (i + n - 1) % n
	default:
		if cur.layout(to) == nil {
			return Layout{}, fmt.Errorf("no layout %q", to)
		}
		for j, l := range cur.Layouts {
			if l.ID == to {
				i = j
			}
		}
	}
	cur.ActiveLayout = cur.Layouts[i].ID
	if err := s.write(&cur); err != nil {
		return Layout{}, err
	}
	s.install(&cur)
	return cur.Layouts[i], nil
}

func (s *ConfigStore) Path() string { return s.path }

// Links returns which device signal each slot is linked to ("" = not linked).
func (c *Config) Links() map[string]string {
	m := make(map[string]string, len(c.Slots))
	for _, s := range c.Slots {
		m[s.ID] = s.Trigger
	}
	return m
}

// Link links a slot to a device signal, unlinking any other slot that had it.
func (s *ConfigStore) Link(slotID, trigger string) error {
	return s.edit(func(c *Config) bool {
		found := false
		for i := range c.Slots {
			switch {
			case c.Slots[i].ID == slotID:
				c.Slots[i].Trigger, found = trigger, true
			case trigger != "" && c.Slots[i].Trigger == trigger:
				c.Slots[i].Trigger = ""
			}
		}
		return found
	})
}

// SetLinks replaces all links with the ones the machine holding the Keydial
// reports. It returns whether anything changed.
func (s *ConfigStore) SetLinks(links map[string]string) (bool, error) {
	changed := false
	err := s.edit(func(c *Config) bool {
		for i := range c.Slots {
			if t := links[c.Slots[i].ID]; t != c.Slots[i].Trigger {
				c.Slots[i].Trigger, changed = t, true
			}
		}
		return changed
	})
	return changed, err
}

// edit applies fn to a copy of the config and saves it if fn reports a change.
func (s *ConfigStore) edit(fn func(*Config) bool) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	cur := *s.Get()
	cur.Slots = append([]Slot(nil), cur.Slots...)
	if !fn(&cur) {
		return nil
	}
	if err := s.write(&cur); err != nil {
		return err
	}
	s.install(&cur)
	return nil
}
