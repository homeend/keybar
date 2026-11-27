package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
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
	Layout  string `json:"layout,omitempty"`
	From    string `json:"from,omitempty"` // machine that sent the press
	To      string `json:"to,omitempty"`   // machine the press was sent to
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
	net    *Net
	router *Router
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
	Host       bool         `json:"host"`   // this machine reads the Keydial now
}

func (s *Server) status() Status {
	return Status{
		Version: version, Devices: s.dm.Devices(), Uinput: s.em.Ready(), UinputErr: s.em.Err(),
		Grabbed: s.dm.AnyGrabbed(), Paused: s.dm.Paused(), Battery: s.battery(), ConfigPath: s.store.Path(), Panels: s.bus.Count(),
		Host: s.router.IsHost(),
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
		page := strings.Replace(string(b), "__TOKEN__", s.token, 1)
		page = strings.Replace(page, "__OS__", hostOS(), 1)
		page = strings.Replace(page, "__HOST__", fmt.Sprint(s.router.IsHost()), 1)
		w.Write([]byte(page))
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
		if !s.router.IsHost() {
			// Button links belong to the machine holding the Keydial; this
			// panel's copy may be older than the last update from there.
			links := old.Links()
			for i := range c.Slots {
				c.Slots[i].Trigger = links[c.Slots[i].ID]
			}
		}
		if err := s.store.Set(&c); err != nil {
			httpErr(w, http.StatusConflict, err)
			return
		}
		s.router.LinksChanged()
		if old.Grab != c.Grab || old.Device != c.Device {
			s.dm.Reset()
		}
		writeJSON(w, s.store.Get())
	})
	mux.HandleFunc("POST /api/layout", func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Layout string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&v)
		l, err := s.store.SwitchLayout(v.Layout)
		if err != nil {
			httpErr(w, http.StatusBadRequest, err)
			return
		}
		s.bus.Publish(BusEvent{Type: "layout", Layout: l.ID})
		writeJSON(w, s.store.Get())
	})
	mux.HandleFunc("GET /api/net", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.netState()) })
	type idReq struct {
		ID   string `json:"id"`
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	netPost := func(path string, fn func(v idReq) error) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var v idReq
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v); err != nil {
				httpErr(w, http.StatusBadRequest, err)
				return
			}
			if err := fn(v); err != nil {
				httpErr(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, s.netState())
		})
	}
	netPost("/api/net/pair", func(v idReq) error { return s.net.Pair(v.ID) })
	netPost("/api/net/answer", func(v idReq) error { return s.net.Answer(v.ID, v.OK) })
	netPost("/api/net/unpair", func(v idReq) error { s.net.Unpair(v.ID); return nil })
	netPost("/api/net/target", func(v idReq) error { return s.router.SetTarget(v.ID, "") })
	netPost("/api/net/name", func(v idReq) error {
		n := strings.TrimSpace(v.Name)
		if n == "" || len(n) > 64 {
			return fmt.Errorf("name must be 1–64 characters")
		}
		err := s.net.SetName(n)
		s.bus.Publish(BusEvent{Type: "net"})
		return err
	})
	mux.HandleFunc("GET /api/net/layouts", func(w http.ResponseWriter, r *http.Request) {
		m, err := s.net.Request(r.URL.Query().Get("id"), Msg{T: "get_layouts"}, 10*time.Second)
		if err != nil {
			httpErr(w, http.StatusBadGateway, err)
			return
		}
		type item struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Assigned int    `json:"assigned"`
		}
		out := []item{}
		for _, l := range m.Layouts {
			out = append(out, item{l.ID, l.Name, assignedKeys(l)})
		}
		writeJSON(w, map[string]any{"os": m.OS, "layouts": out})
	})
	mux.HandleFunc("POST /api/net/import", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			ID      string   `json:"id"`
			Layouts []string `json:"layouts"`
			Into    string   `json:"into"` // replace the keys of this layout instead of adding new ones
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&v); err != nil {
			httpErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := s.net.Request(v.ID, Msg{T: "get_layouts"}, 10*time.Second)
		if err != nil {
			httpErr(w, http.StatusBadGateway, err)
			return
		}
		want := map[string]bool{}
		for _, id := range v.Layouts {
			want[id] = true
		}
		var pick []Layout
		for _, l := range m.Layouts {
			if want[l.ID] {
				pick = append(pick, l)
			}
		}
		if len(pick) == 0 {
			httpErr(w, http.StatusBadRequest, fmt.Errorf("choose at least one layout"))
			return
		}
		from := v.ID
		if p := s.net.peers.Get(v.ID); p != nil {
			from = p.Name
		}
		cur := s.store.Get()
		imported, rep := importLayouts(pick, m.OS, from, cur.Layouts, listApps())
		c := *cur
		c.Layouts = append([]Layout(nil), cur.Layouts...)
		if v.Into != "" {
			dst := c.layout(v.Into)
			if dst == nil {
				httpErr(w, http.StatusBadRequest, fmt.Errorf("no layout %q here", v.Into))
				return
			}
			if len(imported) != 1 {
				httpErr(w, http.StatusBadRequest, fmt.Errorf("choose one layout to copy into %q", dst.Name))
				return
			}
			src := imported[0]
			for slot, k := range src.Keys { // "switch to the copied layout" now means this one
				if k.Action.Type == "layout" && k.Action.Layout == src.ID {
					k.Action.Layout = dst.ID
					src.Keys[slot] = k
				}
			}
			dst.Keys = src.Keys
			imported = []Layout{*dst}
		} else {
			c.Layouts = append(c.Layouts, imported...)
		}
		if err := s.store.Set(&c); err != nil {
			httpErr(w, http.StatusConflict, err)
			return
		}
		// A machine whose active layout does nothing yet (fresh install) starts
		// using the copy right away; otherwise the person picks when to switch.
		activated := false
		if l := cur.layout(cur.ActiveLayout); l == nil || assignedKeys(*l) == 0 {
			if _, err := s.store.SwitchLayout(imported[0].ID); err == nil {
				activated = true
				s.bus.Publish(BusEvent{Type: "layout", Layout: imported[0].ID})
			}
		}
		writeJSON(w, map[string]any{"config": s.store.Get(), "report": rep, "first": imported[0].ID, "activated": activated})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.status()) })
	mux.HandleFunc("POST /api/learn", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Start bool
			Slot  string
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&v)
		if !s.router.IsHost() {
			// The Keydial is on another machine: it listens for the press.
			if err := s.router.RemoteLearn(v.Slot, v.Start); err != nil && v.Start {
				httpErr(w, http.StatusConflict, err)
				return
			}
		} else if v.Start {
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

// App is an installed application the panel can offer.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// assignedKeys counts the keys in a layout that do something.
func assignedKeys(l Layout) int {
	n := 0
	for _, k := range l.Keys {
		if k.Action.Type != "none" && k.Action.Type != "" {
			n++
		}
	}
	return n
}

func (s *Server) netState() NetState {
	st := s.net.State(s.router.Target())
	st.Remote = s.router.Seen()
	return st
}
