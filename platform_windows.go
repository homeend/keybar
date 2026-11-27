//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// hostSupported: false = this machine is the host only while the Keydial is
// connected to it (the DeviceManager reports that); otherwise it receives
// presses from the machine that has it.
const hostSupported = false

const createNoWindow = 0x08000000

func hidden(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return c
}

// spawn runs a command line with cmd.exe, without a console window.
func spawn(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	c := exec.Command("cmd.exe")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow, CmdLine: `cmd.exe /C ` + cmd}
	c.Dir = os.Getenv("USERPROFILE")
	if err := c.Start(); err != nil {
		log.Printf("run %q: %v", cmd, err)
		return
	}
	go c.Wait()
}

// launchApp opens a Start-menu shortcut (the app ID is its full path).
func launchApp(id string) {
	if !strings.EqualFold(filepath.Ext(id), ".lnk") || !underStartMenu(id) {
		log.Printf("app %q: not a Start menu shortcut", id)
		return
	}
	if err := shellOpen(id); err != nil {
		log.Printf("open %q: %v", id, err)
	}
}

func shellOpen(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// ---------- notifications ----------

var osd sync.Mutex

// osdShow shows a Windows toast; toasts with the same tag replace each other.
// Text is passed through environment variables, never into the script.
func osdShow(title, body string) { go toast(title, body) }

func toast(title, body string) {
	osd.Lock()
	defer osd.Unlock()
	const script = `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$t = $xml.GetElementsByTagName('text')
$t.Item(0).AppendChild($xml.CreateTextNode($env:KEYBAR_T)) > $null
$t.Item(1).AppendChild($xml.CreateTextNode($env:KEYBAR_B)) > $null
$n = [Windows.UI.Notifications.ToastNotification]::new($xml)
$n.Tag = 'keybar'; $n.Group = 'keybar'
$n.ExpirationTime = [DateTimeOffset]::Now.AddSeconds(5)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show($n)
`
	c := hidden(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script))
	c.Env = append(os.Environ(), "KEYBAR_T="+title, "KEYBAR_B="+body)
	if out, err := c.CombinedOutput(); err != nil {
		log.Printf("notification: %v %s", err, bytes.TrimSpace(out))
	}
}

// ---------- process & panel ----------

var instanceMutex windows.Handle

// singleInstance holds a per-user named mutex for the life of the process.
func singleInstance() bool {
	sum := sha256.Sum256([]byte(strings.ToLower(dataDir())))
	name, _ := windows.UTF16PtrFromString(`Local\keybar-` + hex.EncodeToString(sum[:8]))
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(h)
		return false
	}
	instanceMutex = h
	return true
}

func startBackground() {
	exe, _ := os.Executable()
	c := exec.Command(exe)
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	c.Start()
}

func openPanel(addr string) {
	if recentlyOpened() {
		return
	}
	url := "http://" + addr + "/"
	pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")
	for _, p := range []string{
		filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
	} {
		if _, err := os.Stat(p); err == nil {
			if exec.Command(p, "--app="+url, "--window-size=1180,820").Start() == nil {
				return
			}
		}
	}
	shellOpen(url)
}

// autostart adds or removes Keybar from the user's sign-in programs.
func autostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		k.DeleteValue("Keybar")
		fmt.Println("Keybar removed from startup.")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := k.SetStringValue("Keybar", `"`+exe+`"`); err != nil {
		return err
	}
	fmt.Println("Keybar will start when you sign in:", exe)
	return nil
}

// ---------- tray icon ----------

// trayIconBytes wraps the PNG icon in an .ico container (Windows tray needs .ico).
func trayIconBytes(state string) []byte {
	png := trayIcon(state)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1})                  // reserved, type=icon, count
	b.Write([]byte{64, 64, 0, 0})                                             // 64x64, no palette
	binary.Write(&b, binary.LittleEndian, []uint16{1, 32})                    // planes, bpp
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(png)), 6 + 16}) // size, offset
	b.Write(png)
	return b.Bytes()
}

// ---------- installed applications (Start menu shortcuts) ----------

func startMenuDirs() []string {
	return []string{
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`),
		filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs`),
	}
}

func underStartMenu(p string) bool {
	p = strings.ToLower(filepath.Clean(p))
	for _, d := range startMenuDirs() {
		if strings.HasPrefix(p, strings.ToLower(filepath.Clean(d))+`\`) {
			return true
		}
	}
	return false
}

func listApps() []App {
	seen := map[string]bool{}
	apps := []App{}
	for _, dir := range startMenuDirs() {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".lnk") {
				return nil
			}
			name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			low := strings.ToLower(name)
			if seen[low] || strings.Contains(low, "uninstall") {
				return nil
			}
			seen[low] = true
			apps = append(apps, App{ID: p, Name: name})
			return nil
		})
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps
}

func findIcon(name string) string { return "" }
