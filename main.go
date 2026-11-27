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
		printDevices()
	case "install", "uninstall":
		if err := autostart(cmd == "install"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: keybar [run|open|devices|install|uninstall|version]\n")
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
	cur := filepath.Join(dir, "keybar.log")
	os.Rename(cur, filepath.Join(dir, "keybar.prev.log")) // keep the last run's log for debugging
	if f, err := os.OpenFile(cur, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
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
	ident, err := loadIdentity()
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	router := NewRouter(engine, nil, bus)
	nw := NewNet(ident, loadPeers(), store.Get().PeerPort, router)
	router.net = nw
	engine.router = router
	go nw.Run()
	dm = NewDeviceManager(store, em, engine.in, func() { bus.Publish(BusEvent{Type: "status"}) })
	engine.grabbed = dm.AnyGrabbed
	dm.onClose = engine.Reset
	if !hostSupported {
		dm.onHost = router.SetHost // the host is whichever machine has the Keydial
	}
	go func() { // release held keys on shutdown too
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		<-sig
		em.ReleaseAll()
		os.Exit(0)
	}()
	go engine.Run()
	go dm.Run()

	srv := &Server{store: store, engine: engine, dm: dm, em: em, bus: bus, net: nw, router: router, addr: addr, token: newToken(), bat: -1}
	go func() { log.Fatal(http.Serve(ln, srv.Handler())) }()

	if os.Getenv("KEYBAR_NO_TRAY") != "" { // test copy: no tray icon
		select {}
	}
	systray.Run(func() { onTrayReady(addr, dm, em, router) }, func() {})
}

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
	startBackground()
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

func onTrayReady(addr string, dm *DeviceManager, em *Emitter, router *Router) {
	systray.SetTitle("Keybar")
	systray.SetTooltip("Keybar — Keydial button mapper")
	systray.SetIcon(trayIconBytes("idle"))

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
		last, pauseShown := "", true
		for {
			// Pause only means something while this machine reads the Keydial.
			if host := router.IsHost(); host != pauseShown {
				if host {
					pause.Show()
				} else {
					pause.Hide()
				}
				pauseShown = host
			}
			st, text := "idle", "Keydial: not connected"
			devs := dm.Devices()
			switch {
			case !router.IsHost():
				st, text = "on", "Keybar: receives from paired machines"
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
				systray.SetIcon(trayIconBytes(st))
				status.SetTitle(text)
				last = st + text
			}
			time.Sleep(time.Second)
		}
	}()
}
