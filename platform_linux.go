//go:build linux

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

func launchApp(id string) {
	spawn("gtk-launch " + shellQuote(strings.TrimSuffix(id, ".desktop")))
}

func startBackground() {
	if exec.Command("systemctl", "--user", "start", "keybar.service").Run() != nil {
		exe, _ := os.Executable()
		c := exec.Command(exe)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		c.Start()
	}
}

func printDevices() {
	for _, d := range listInputDevices() {
		fmt.Printf("%-40q vendor=%s product=%s %v\n", d.Name, d.Vendor, d.Product, d.Events)
	}
}

func autostart(on bool) error {
	return fmt.Errorf("on Linux use `make install` / `make uninstall` (systemd user service)")
}

func trayIconBytes(state string) []byte { return trayIcon(state) }

func spawn(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	c := exec.Command("sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Dir = os.Getenv("HOME")
	if err := c.Start(); err != nil {
		log.Printf("run %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---------- notifications (GNOME, over D-Bus) ----------
//
// One Keybar notification at a time, driven by a single worker so updates are
// shown in order and only the latest one counts. GNOME doesn't pop a banner
// again when an existing notification is *replaced* after its banner has gone,
// so a stale one is closed and a new one opened instead; only rapid updates
// (turning the dial) replace in place while the banner is still on screen.

type noteReq struct {
	title, body string
	d           time.Duration // 0 = keep until hidden (capped)
	hide        bool
}

var (
	noteCh   = make(chan noteReq, 64)
	noteOnce sync.Once
)

func noteSend(r noteReq) {
	noteOnce.Do(func() { go noteWorker() })
	select {
	case noteCh <- r:
	default: // worker stuck (no D-Bus?): drop rather than block the engine
	}
}

func noteWorker() {
	const (
		iface     = "org.freedesktop.Notifications"
		bannerFor = 3 * time.Second  // GNOME keeps a banner up about this long
		holdMax   = 30 * time.Second // a held picker never outlives this
	)
	var (
		conn    *dbus.Conn
		id      uint32
		shownAt time.Time
		expire  <-chan time.Time
	)
	obj := func() dbus.BusObject {
		if conn == nil {
			c, err := dbus.ConnectSessionBus()
			if err != nil {
				log.Printf("notifications: %v", err)
				return nil
			}
			conn = c
		}
		return conn.Object(iface, "/org/freedesktop/Notifications")
	}
	closeCur := func() {
		if id != 0 {
			if o := obj(); o != nil {
				o.Call(iface+".CloseNotification", 0, id)
			}
			id = 0
		}
	}
	for {
		select {
		case r := <-noteCh:
			for more := true; more; { // only the latest request matters
				select {
				case r2 := <-noteCh:
					r = r2
				default:
					more = false
				}
			}
			expire = nil
			if r.hide {
				closeCur()
				continue
			}
			replace := uint32(0)
			if id != 0 && time.Since(shownAt) < bannerFor {
				replace = id // banner still visible: update it in place
			} else {
				closeCur()
				shownAt = time.Now()
			}
			o := obj()
			if o == nil {
				continue
			}
			hints := map[string]dbus.Variant{"transient": dbus.MakeVariant(true), "urgency": dbus.MakeVariant(byte(1))}
			d := r.d
			if d == 0 {
				d = holdMax
			}
			call := o.Call(iface+".Notify", 0, "Keybar", replace, "input-keyboard", r.title, r.body, []string{}, hints, int32(d/time.Millisecond))
			if call.Err != nil {
				log.Printf("notifications: %v", call.Err)
				conn.Close()
				conn, id = nil, 0
				continue
			}
			call.Store(&id)
			expire = time.After(d)
		case <-expire:
			closeCur()
			expire = nil
		}
	}
}

// osdShow shows a short Keybar notification.
func osdShow(title, body string) {
	noteSend(noteReq{title: title, body: body, d: 2500 * time.Millisecond})
}

// singleInstance holds an exclusive lock for the life of the process.
func singleInstance() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	if os.Getenv("KEYBAR_HOME") != "" {
		dir = dataDir()
		os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(filepath.Join(dir, "keybar.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return false
	}
	lockFile = f // keep open
	return true
}

var lockFile *os.File

func openPanel(addr string) {
	if recentlyOpened() {
		return
	}
	url := "http://" + addr + "/"
	for _, b := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(b); err == nil {
			if exec.Command(p, "--app="+url, "--class=keybar", "--window-size=1180,820").Start() == nil {
				return
			}
		}
	}
	exec.Command("xdg-open", url).Start()
}

// ---------- installed applications ----------

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

// osOverlay: GNOME has no overlay for ordinary apps, so this is a notification
// that replaces itself as the selection changes.
func osOverlay(title string, lines []string, sel int, hold bool) {
	var b strings.Builder
	for i, l := range lines {
		switch {
		case sel < 0:
			b.WriteString(l + "\n")
		case i == sel:
			b.WriteString("▶  " + l + "\n")
		default:
			b.WriteString("     " + l + "\n")
		}
	}
	d := 2500 * time.Millisecond
	if hold {
		d = 0
	}
	noteSend(noteReq{title: title, body: strings.TrimSpace(b.String()), d: d})
}

func osOverlayHide() { noteSend(noteReq{hide: true}) }
