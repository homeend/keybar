package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// ---------- event bus (Server-Sent Events) ----------

type BusEvent struct {
	Type    string `json:"type"` // trigger | learned | status
	Trigger string `json:"trigger,omitempty"`
	Binding string `json:"binding,omitempty"`
}

type Bus struct {
	mu   sync.Mutex
	subs map[chan BusEvent]bool
}

func NewBus() *Bus { return &Bus{subs: map[chan BusEvent]bool{}} }

func (b *Bus) Publish(ev BusEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow client: drop
		}
	}
}

func (b *Bus) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *Bus) Subscribe() (chan BusEvent, func()) {
	ch := make(chan BusEvent, 64)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// ---------- HTTP ----------

type Server struct {
	store  *ConfigStore
	engine *Engine
	dm     *DeviceManager
	em     *Emitter
	bus    *Bus
	addr   string
	token  string

	batMu   sync.Mutex
	bat     int
	batTime time.Time
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type Status struct {
	Version    string       `json:"version"`
	Devices    []DeviceInfo `json:"devices"`
	Uinput     bool         `json:"uinput"`
	UinputErr  string       `json:"uinput_error,omitempty"`
	Grabbed    bool         `json:"grabbed"`
	Paused     bool         `json:"paused"`
	Battery    int          `json:"battery"`
	ConfigPath string       `json:"config_path"`
	Panels     int          `json:"panels"` // connected control-panel pages
}

func (s *Server) status() Status {
	return Status{
		Version: version, Devices: s.dm.Devices(), Uinput: s.em.Ready(), UinputErr: s.em.Err(),
		Grabbed: s.dm.AnyGrabbed(), Paused: s.dm.Paused(), Battery: s.battery(), ConfigPath: s.store.Path(), Panels: s.bus.Count(),
	}
}

func (s *Server) battery() int {
	s.batMu.Lock()
	defer s.batMu.Unlock()
	if time.Since(s.batTime) > time.Minute {
		s.bat = batteryPercent(s.store.Get().Device)
		s.batTime = time.Now()
	}
	return s.bat
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(strings.Replace(string(b), "__TOKEN__", s.token, 1)))
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.store.Get()) })
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
			httpErr(w, http.StatusBadRequest, err)
			return
		}
		old := s.store.Get()
		if err := s.store.Set(&c); err != nil {
			httpErr(w, http.StatusConflict, err)
			return
		}
		if old.Grab != c.Grab || old.Device != c.Device {
			s.dm.Reset()
		}
		writeJSON(w, s.store.Get())
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.status()) })
	mux.HandleFunc("POST /api/learn", func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Start bool }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&v)
		if v.Start {
			s.engine.StartLearning()
		} else {
			s.engine.CancelLearning()
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/test", func(w http.ResponseWriter, r *http.Request) {
		var a Action
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&a); err != nil {
			httpErr(w, http.StatusBadRequest, err)
			return
		}
		go func() {
			time.Sleep(600 * time.Millisecond) // let the user see where focus is
			if rel := runAction(s.em, a); rel != nil {
				time.Sleep(80 * time.Millisecond)
				rel()
			}
		}()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Paused bool }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&v)
		s.dm.SetPaused(v.Paused)
		writeJSON(w, map[string]bool{"ok": true})
	})
	var capMu sync.Mutex
	mux.HandleFunc("POST /api/capture", func(w http.ResponseWriter, r *http.Request) {
		if !capMu.TryLock() {
			httpErr(w, http.StatusConflict, fmt.Errorf("already recording"))
			return
		}
		defer capMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		writeJSON(w, map[string]string{"keys": captureShortcut(ctx, s.store.Get().Device)})
	})
	mux.HandleFunc("GET /api/apps", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, listApps()) })
	mux.HandleFunc("GET /api/icon", func(w http.ResponseWriter, r *http.Request) {
		p := findIcon(r.URL.Query().Get("name"))
		if p == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "max-age=86400")
		http.ServeFile(w, r, p)
	})
	mux.HandleFunc("GET /api/events", s.events)
	return s.guard(mux)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, cancel := s.bus.Subscribe()
	defer cancel()
	fmt.Fprint(w, "data: {\"type\":\"hello\"}\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// guard blocks DNS rebinding (Host check) and cross-site requests (token on every API call),
// since this server can run commands.
func (s *Server) guard(h http.Handler) http.Handler {
	_, port, _ := strings.Cut(s.addr, ":")
	okHosts := map[string]bool{s.addr: true, "localhost:" + port: true, "127.0.0.1:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !okHosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/icon" {
			tok := r.Header.Get("X-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token") // EventSource cannot set headers
			}
			if tok != s.token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// ---------- installed applications ----------

type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

func appDirs() []string {
	home := os.Getenv("HOME")
	dirs := []string{
		filepath.Join(home, ".local/share/applications"),
		filepath.Join(home, ".local/share/flatpak/exports/share/applications"),
		"/var/lib/flatpak/exports/share/applications",
		"/var/lib/snapd/desktop/applications",
		"/usr/local/share/applications",
		"/usr/share/applications",
	}
	return dirs
}

func listApps() []App {
	seen := map[string]bool{}
	apps := []App{}
	for _, dir := range appDirs() {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".desktop") || seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true // earlier dirs override later ones
			a, ok := parseDesktop(filepath.Join(dir, e.Name()))
			if ok {
				a.ID = e.Name()
				apps = append(apps, a)
			}
		}
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps
}

func parseDesktop(path string) (App, bool) {
	f, err := os.Open(path)
	if err != nil {
		return App{}, false
	}
	defer f.Close()
	var a App
	inEntry, isApp, hidden := false, false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "Name":
			a.Name = v
		case "Icon":
			a.Icon = v
		case "Type":
			isApp = v == "Application"
		case "NoDisplay", "Hidden":
			if v == "true" {
				hidden = true
			}
		}
	}
	return a, isApp && !hidden && a.Name != ""
}

func findIcon(name string) string {
	if name == "" || strings.Contains(name, "..") {
		return ""
	}
	okExt := func(p string) bool {
		ext := strings.ToLower(filepath.Ext(p))
		return ext == ".png" || ext == ".svg"
	}
	if filepath.IsAbs(name) {
		home := os.Getenv("HOME")
		allowed := false
		for _, pre := range []string{"/usr/share/", "/snap/", "/var/lib/snapd/", "/var/lib/flatpak/", home + "/.local/share/"} {
			allowed = allowed || strings.HasPrefix(name, pre)
		}
		if allowed && okExt(name) {
			if _, err := os.Stat(name); err == nil {
				return name
			}
		}
		return ""
	}
	home := os.Getenv("HOME")
	bases := []string{
		filepath.Join(home, ".local/share/icons/hicolor"),
		filepath.Join(home, ".local/share/flatpak/exports/share/icons/hicolor"),
		"/var/lib/flatpak/exports/share/icons/hicolor",
		"/usr/share/icons/hicolor",
	}
	sizes := []string{"scalable", "256x256", "128x128", "96x96", "64x64", "48x48", "512x512", "32x32"}
	for _, b := range bases {
		for _, sz := range sizes {
			for _, ext := range []string{".svg", ".png"} {
				p := filepath.Join(b, sz, "apps", name+ext)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	for _, ext := range []string{".svg", ".png"} {
		p := filepath.Join("/usr/share/pixmaps", name+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Themed fallback (e.g. Yaru) for system apps.
	for _, theme := range []string{"Yaru", "Adwaita"} {
		matches, _ := filepath.Glob(filepath.Join("/usr/share/icons", theme, "*", "apps", name+".*"))
		for _, m := range matches {
			if okExt(m) {
				return m
			}
		}
	}
	return ""
}
