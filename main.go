// keybar — maps the buttons and dial of a Huion Keydial (KD20 / "Keydial mini")
// to shortcuts, apps, commands, mouse events and text. Runs as a tray app with
// a local control panel at http://127.0.0.1:47830.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fyne.io/systray"
)

const version = "0.1.0"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "", "run":
		run()
	case "open":
		addr := LoadConfig(configPath()).Get().Listen
		ensureRunning(addr)
		openPanel(addr)
	case "devices":
		for _, d := range listInputDevices() {
			fmt.Printf("%-40q vendor=%s product=%s %v\n", d.Name, d.Vendor, d.Product, d.Events)
		}
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: keybar [run|open|devices|version]\n")
		os.Exit(2)
	}
}

func setupLog() {
	dir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "keybar")
	os.MkdirAll(dir, 0o755)
	if f, err := os.OpenFile(filepath.Join(dir, "keybar.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(f)
	}
}

func run() {
	store := LoadConfig(configPath())
	addr := store.Get().Listen
	if !singleInstance() {
		fmt.Fprintln(os.Stderr, "keybar is already running; use `keybar open` for the control panel")
		return
	}
	if os.Getenv("KEYBAR_LOG_STDERR") == "" {
		setupLog()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v (change \"listen\" in %s)", addr, err, store.Path())
	}
	log.Printf("keybar %s starting, config %s", version, store.Path())

	bus := NewBus()
	var dm *DeviceManager
	em := NewEmitter(func() {
		if dm != nil {
			dm.Reset() // uinput just became available: re-open devices with grab
		}
		bus.Publish(BusEvent{Type: "status"})
	})
	engine := NewEngine(store, em, bus)
	dm = NewDeviceManager(store, em, engine.in, func() { bus.Publish(BusEvent{Type: "status"}) })
	engine.grabbed = dm.AnyGrabbed
	dm.onClose = engine.Reset
	go func() { // release held keys on shutdown too
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		<-sig
		em.ReleaseAll()
		os.Exit(0)
	}()
	go engine.Run()
	go dm.Run()

	srv := &Server{store: store, engine: engine, dm: dm, em: em, bus: bus, addr: addr, token: newToken(), bat: -1}
	go func() { log.Fatal(http.Serve(ln, srv.Handler())) }()

	systray.Run(func() { onTrayReady(addr, dm, em) }, func() {})
}

// singleInstance holds an exclusive lock for the life of the process.
func singleInstance() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
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

// ensureRunning starts the background service if nothing answers on addr yet.
func ensureRunning(addr string) {
	alive := func() bool {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	if alive() {
		return
	}
	if exec.Command("systemctl", "--user", "start", "keybar.service").Run() != nil {
		exe, _ := os.Executable()
		c := exec.Command(exe)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		c.Start()
	}
	for i := 0; i < 50 && !alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
}

// recentlyOpened makes double/triple clicks on the launcher or tray open one window.
func recentlyOpened() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	stamp := filepath.Join(dir, "keybar-open.stamp")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < 3*time.Second {
		return true
	}
	os.WriteFile(stamp, nil, 0o600)
	now := time.Now()
	os.Chtimes(stamp, now, now)
	return false
}

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

func onTrayReady(addr string, dm *DeviceManager, em *Emitter) {
	systray.SetTitle("Keybar")
	systray.SetTooltip("Keybar — Keydial button mapper")
	systray.SetIcon(trayIcon("idle"))

	status := systray.AddMenuItem("Keydial: …", "")
	status.Disable()
	systray.AddSeparator()
	open := systray.AddMenuItem("Open control panel…", "")
	pause := systray.AddMenuItemCheckbox("Pause mappings", "Give the Keydial back its factory keys", false)
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit Keybar", "")

	go func() {
		for {
			select {
			case <-open.ClickedCh:
				openPanel(addr)
			case <-pause.ClickedCh:
				p := !dm.Paused()
				dm.SetPaused(p)
				if p {
					pause.Check()
				} else {
					pause.Uncheck()
				}
			case <-quit.ClickedCh:
				em.ReleaseAll()
				systray.Quit()
				os.Exit(0)
			}
		}
	}()
	go func() {
		last := ""
		for {
			st, text := "idle", "Keydial: not connected"
			devs := dm.Devices()
			switch {
			case !em.Ready():
				st, text = "warn", "Needs setup: virtual input (uinput) not accessible"
			case dm.Paused():
				st, text = "off", "Keydial: paused"
			case len(devs) > 0:
				st, text = "on", "Keydial: connected"
			default:
				st = "off"
			}
			if st+text != last {
				systray.SetIcon(trayIcon(st))
				status.SetTitle(text)
				last = st + text
			}
			time.Sleep(time.Second)
		}
	}()
}
