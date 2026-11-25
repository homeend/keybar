//go:build windows

package main

import (
	"log"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A small always-on-top window in the middle of the screen (like Alt+Tab) for
// the machine picker and "Keydial → this machine". It never takes focus, so
// keys keep going to the app you were using.

var (
	gdi32                          = windows.NewLazySystemDLL("gdi32.dll")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procSetWindowPos               = user32.NewProc("SetWindowPos")
	procInvalidateRect             = user32.NewProc("InvalidateRect")
	procBeginPaint                 = user32.NewProc("BeginPaint")
	procEndPaint                   = user32.NewProc("EndPaint")
	procGetSystemMetrics           = user32.NewProc("GetSystemMetrics")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procSetTimer                   = user32.NewProc("SetTimer")
	procKillTimer                  = user32.NewProc("KillTimer")
	procFillRect                   = user32.NewProc("FillRect")
	procDrawTextW                  = user32.NewProc("DrawTextW")
	procGetDpiForSystem            = user32.NewProc("GetDpiForSystem")
	procSetProcessDPIAware         = user32.NewProc("SetProcessDPIAware")
	procCreateFontW                = gdi32.NewProc("CreateFontW")
	procSelectObject               = gdi32.NewProc("SelectObject")
	procSetTextColor               = gdi32.NewProc("SetTextColor")
	procSetBkMode                  = gdi32.NewProc("SetBkMode")
	procCreateSolidBrush           = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject               = gdi32.NewProc("DeleteObject")
	procCreateRoundRectRgn         = gdi32.NewProc("CreateRoundRectRgn")
	procSetWindowRgn               = user32.NewProc("SetWindowRgn")
)

const (
	wsPopup          = 0x80000000
	wsExTopmost      = 0x00000008
	wsExToolWindow   = 0x00000080
	wsExLayered      = 0x00080000
	wsExNoActivate   = 0x08000000
	wsExTransparent  = 0x00000020
	wmPaint          = 0x000F
	wmTimer          = 0x0113
	wmApp            = 0x8000
	swHide           = 0
	swShowNoActivate = 4
	swpNoActivate    = 0x0010
	swpShowWindow    = 0x0040
	lwaAlpha         = 0x2
	dtCenter         = 0x1
	dtVCenter        = 0x4
	dtSingleLine     = 0x20
	dtEndEllipsis    = 0x8000
	dtLeft           = 0x0
	smCxScreen       = 0
	smCyScreen       = 1
	hideTimer        = 1
)

type rect struct{ l, t, r, b int32 }

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
	private uint32
}

var ov struct {
	once  sync.Once
	ready chan struct{}
	hwnd  uintptr
	scale float64

	mu    sync.Mutex
	title string
	lines []string
	sel   int
	hold  bool
	show  bool
}

func rgb(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func osOverlay(title string, lines []string, sel int, hold bool) {
	log.Printf("overlay: show %q %q sel=%d hold=%v", title, lines, sel, hold)
	ov.once.Do(startOverlay)
	<-ov.ready
	if ov.hwnd == 0 {
		return
	}
	ov.mu.Lock()
	ov.title, ov.lines, ov.sel, ov.hold, ov.show = title, append([]string(nil), lines...), sel, hold, true
	ov.mu.Unlock()
	procPostMessageW.Call(ov.hwnd, wmApp, 0, 0)
}

func osOverlayHide() {
	log.Printf("overlay: hide")
	ov.once.Do(startOverlay)
	<-ov.ready
	if ov.hwnd == 0 {
		return
	}
	ov.mu.Lock()
	ov.show = false
	ov.mu.Unlock()
	procPostMessageW.Call(ov.hwnd, wmApp, 0, 0)
}

// startOverlay creates the window and runs its message loop on one locked
// thread (a window belongs to the thread that made it).
func startOverlay() {
	ov.ready = make(chan struct{})
	go func() {
		runtime.LockOSThread()
		ok := createOverlay()
		close(ov.ready)
		if !ok {
			return
		}
		var m winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
}

func createOverlay() bool {
	procSetProcessDPIAware.Call()
	ov.scale = 1
	if procGetDpiForSystem.Find() == nil {
		if dpi, _, _ := procGetDpiForSystem.Call(); dpi > 0 {
			ov.scale = float64(dpi) / 96
		}
	}
	cls, _ := windows.UTF16PtrFromString("KeybarOverlay")
	wc := wndClassEx{wndProc: windows.NewCallback(overlayProc), className: cls}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		log.Printf("overlay: register class: %v", err)
		return false
	}
	h, _, err := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow|wsExLayered|wsExNoActivate|wsExTransparent,
		uintptr(unsafe.Pointer(cls)), 0, wsPopup, 0, 0, 100, 100, 0, 0, 0, 0)
	if h == 0 {
		log.Printf("overlay: create window: %v", err)
		return false
	}
	procSetLayeredWindowAttributes.Call(h, 0, 238, lwaAlpha)
	ov.hwnd = h
	return true
}

func px(v float64) int32 { return int32(v*ov.scale + 0.5) }

// layout sizes and places the window for the current content (window thread).
func overlayLayout(h uintptr) {
	ov.mu.Lock()
	n := len(ov.lines)
	show := ov.show
	hold := ov.hold
	ov.mu.Unlock()
	if !show {
		procKillTimer.Call(h, hideTimer)
		procShowWindow.Call(h, swHide)
		return
	}
	w := px(460)
	ht := px(64) + int32(n)*px(42)
	if n > 0 {
		ht += px(14)
	}
	sw, _, _ := procGetSystemMetrics.Call(smCxScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCyScreen)
	x, y := (int32(sw)-w)/2, (int32(sh)-ht)/2
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(ht+1), uintptr(px(22)), uintptr(px(22)))
	procSetWindowRgn.Call(h, rgn, 1) // the window owns the region from now on
	hwndTopmost := ^uintptr(0)       // HWND_TOPMOST = -1
	procSetWindowPos.Call(h, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(ht), swpNoActivate|swpShowWindow)
	procShowWindow.Call(h, swShowNoActivate)
	procInvalidateRect.Call(h, 0, 1)
	procKillTimer.Call(h, hideTimer)
	if !hold {
		procSetTimer.Call(h, hideTimer, 1400, 0)
	}
}

func overlayProc(h, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmApp:
		overlayLayout(h)
		return 0
	case wmTimer:
		if wp == hideTimer {
			ov.mu.Lock()
			ov.show = false
			ov.mu.Unlock()
			overlayLayout(h)
		}
		return 0
	case wmPaint:
		overlayPaint(h)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(h, msg, wp, lp)
	return r
}

func font(size float64, weight int) uintptr {
	face, _ := windows.UTF16PtrFromString("Segoe UI")
	f, _, _ := procCreateFontW.Call(uintptr(-px(size)), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(face)))
	return f
}

func drawText(hdc uintptr, s string, r rect, flags uintptr) {
	p, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)-1), uintptr(unsafe.Pointer(&r)), flags)
}

func overlayPaint(h uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(h, uintptr(unsafe.Pointer(&ps)))
	ov.mu.Lock()
	title, lines, sel := ov.title, ov.lines, ov.sel
	ov.mu.Unlock()

	w := px(460)
	ht := px(64) + int32(len(lines))*px(42)
	if len(lines) > 0 {
		ht += px(14)
	}
	bg, _, _ := procCreateSolidBrush.Call(rgb(0x20, 0x22, 0x27))
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect{0, 0, w, ht})), bg)
	procDeleteObject.Call(bg)
	procSetBkMode.Call(hdc, 1) // transparent

	tf := font(17, 600)
	old, _, _ := procSelectObject.Call(hdc, tf)
	procSetTextColor.Call(hdc, rgb(0xee, 0xef, 0xf1))
	drawText(hdc, title, rect{px(20), 0, w - px(20), px(64)}, dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis)

	lf := font(16, 400)
	sf := font(16, 700)
	for i, l := range lines {
		top := px(64) + int32(i)*px(42)
		row := rect{px(16), top, w - px(16), top + px(38)}
		if i == sel {
			hb, _, _ := procCreateSolidBrush.Call(rgb(0x3d, 0x7b, 0xff))
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&row)), hb)
			procDeleteObject.Call(hb)
			procSelectObject.Call(hdc, sf)
			procSetTextColor.Call(hdc, rgb(0xff, 0xff, 0xff))
		} else {
			procSelectObject.Call(hdc, lf)
			procSetTextColor.Call(hdc, rgb(0xb8, 0xbe, 0xc7))
		}
		text := rect{row.l + px(16), row.t, row.r - px(16), row.b}
		drawText(hdc, l, text, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
	}
	procSelectObject.Call(hdc, old)
	procDeleteObject.Call(tf)
	procDeleteObject.Call(lf)
	procDeleteObject.Call(sf)
}
