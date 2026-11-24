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
// keys, app, command, mouse, text, passthrough, none.
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
}

type Binding struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Icon    string `json:"icon,omitempty"` // emoji shown on the key
	Trigger string `json:"trigger"`        // what the device sends, e.g. "KEY_LEFTCTRL+KEY_Z"
	Action  Action `json:"action"`
	Group   string `json:"group"` // "dial" | "keys"
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

type Config struct {
	Version             int         `json:"version"`
	Listen              string      `json:"listen"`
	Device              DeviceMatch `json:"device"`
	Grab                bool        `json:"grab"`                 // take exclusive control of the device
	UnmappedPassthrough bool        `json:"unmapped_passthrough"` // unassigned buttons keep their factory keys
	Columns             int         `json:"columns"`
	Bindings            []Binding   `json:"bindings"`
}

func defaultConfig() *Config {
	c := &Config{
		Version: 3, Listen: "127.0.0.1:47830",
		Device:              DeviceMatch{Vendor: "256c", Product: "8251"},
		Grab:                true,
		UnmappedPassthrough: false,
		Columns:             4,
	}
	none := Action{Type: "none"} // until mapped, a button is silent
	c.Bindings = append(c.Bindings,
		Binding{ID: "dial_ccw", Label: "Dial ↺", Group: "dial", Action: none},
		Binding{ID: "dial_press", Label: "Dial press", Group: "dial", Action: none},
		Binding{ID: "dial_cw", Label: "Dial ↻", Group: "dial", Action: none},
	)
	for i := 0; i < 18; i++ {
		c.Bindings = append(c.Bindings, Binding{
			ID: fmt.Sprintf("k%d", i+1), Label: fmt.Sprintf("K%d", i+1), Group: "keys", Action: none,
		})
	}
	applyKD20Layout(c)
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

func applyKD20Layout(c *Config) {
	for i := range c.Bindings {
		if p, ok := kd20Layout[c.Bindings[i].ID]; ok {
			b := &c.Bindings[i]
			b.X, b.Y, b.W, b.H = p[0], p[1], p[2], p[3]
		}
	}
	c.Columns = 4
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "keybar", "config.json")
}

type ConfigStore struct {
	mu        sync.RWMutex
	path      string
	cfg       *Config
	byTrigger map[string]*Binding
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
	if c.Listen == "" {
		c.Listen = "127.0.0.1:47830"
	}
	if c.Columns <= 0 {
		c.Columns = 4
	}
	if c.Version < 2 { // v1 drew a 4×4 grid plus two wide keys
		applyKD20Layout(c)
		c.Version = 2
	}
	if c.Version < 3 { // v2 let unassigned buttons type their factory keys
		c.UnmappedPassthrough = false
		for i := range c.Bindings {
			if c.Bindings[i].Action.Type == "passthrough" {
				c.Bindings[i].Action = Action{Type: "none"}
			}
		}
		c.Version = 3
	}
	seen := map[string]string{}
	ids := map[string]bool{}
	for i := range c.Bindings {
		b := &c.Bindings[i]
		b.Trigger = strings.TrimSpace(b.Trigger)
		if b.ID == "" || ids[b.ID] {
			return fmt.Errorf("binding %d has a missing or duplicate id", i)
		}
		ids[b.ID] = true
		if b.Action.Type == "" {
			b.Action.Type = "none"
		}
		if b.W <= 0 {
			b.W = 1
		}
		if b.H <= 0 {
			b.H = 1
		}
		if b.Trigger != "" {
			if other, dup := seen[b.Trigger]; dup {
				return fmt.Errorf("%q and %q use the same device button", other, b.Label)
			}
			seen[b.Trigger] = b.Label
		}
		if b.Action.Type == "keys" && strings.TrimSpace(b.Action.Keys) != "" {
			if _, err := parseShortcut(b.Action.Keys); err != nil {
				return fmt.Errorf("%s: %v", b.Label, err)
			}
		}
	}
	return nil
}

func (s *ConfigStore) install(c *Config) {
	idx := map[string]*Binding{}
	for i := range c.Bindings {
		if t := c.Bindings[i].Trigger; t != "" {
			idx[t] = &c.Bindings[i]
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

func (s *ConfigStore) Find(trigger string) *Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byTrigger[trigger]
}

func (s *ConfigStore) Set(c *Config) error {
	if err := normalize(c); err != nil {
		return err
	}
	if err := s.write(c); err != nil {
		return err
	}
	s.install(c)
	return nil
}

func (s *ConfigStore) Path() string { return s.path }
