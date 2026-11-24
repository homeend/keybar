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
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// hostSupported: this OS can read the Keydial itself (not only receive from another machine).
const hostSupported = true

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

var osd struct {
	mu sync.Mutex
	id string
}

// showOSD shows (or replaces) a single Keybar notification.
func osdShow(title, body string) {
	func() {
		osd.mu.Lock()
		defer osd.mu.Unlock()
		args := []string{"-a", "Keybar", "-p", "-t", "2500", "-h", "int:transient:1"}
		if osd.id != "" {
			args = append(args, "-r", osd.id)
		}
		args = append(args, title, body)
		out, err := exec.Command("notify-send", args...).Output()
		if err != nil {
			return
		}
		if id := strings.TrimSpace(string(out)); id != "" {
			if _, err := strconv.Atoi(id); err == nil {
				osd.id = id
			}
		}
	}()
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
	go osdShow(title, strings.TrimSpace(b.String()))
}

func osOverlayHide() {} // the notification expires by itself
