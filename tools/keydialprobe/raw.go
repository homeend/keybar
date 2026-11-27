//go:build windows

package main

// Raw Input: reports every key / mouse event with the device it came from, so
// the Keydial's keys can be told apart from the real keyboard (read-only).

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	u32                    = syscall.NewLazyDLL("user32.dll")
	pCreateWindowEx        = u32.NewProc("CreateWindowExW")
	pRegisterRawInput      = u32.NewProc("RegisterRawInputDevices")
	pGetRawInputData       = u32.NewProc("GetRawInputData")
	pGetRawInputDeviceInfo = u32.NewProc("GetRawInputDeviceInfoW")
	pGetMessage            = u32.NewProc("GetMessageW")
)

type rawDevice struct {
	page, usage uint16
	flags       uint32
	target      uintptr
}

func deviceName(h uintptr) string {
	var n uint32
	pGetRawInputDeviceInfo.Call(h, 0x20000007, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return "?"
	}
	buf := make([]uint16, n+1)
	pGetRawInputDeviceInfo.Call(h, 0x20000007, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	return syscall.UTF16ToString(buf)
}

func watchRaw(start time.Time) {
	runtime.LockOSThread()
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0,
		^uintptr(2), 0, 0, 0) // HWND_MESSAGE = -3
	if hwnd == 0 {
		fmt.Println("window:", err)
		return
	}
	devs := []rawDevice{
		{1, 6, 0x100, hwnd}, // keyboard, RIDEV_INPUTSINK
		{1, 2, 0x100, hwnd}, // mouse
	}
	if ok, _, err := pRegisterRawInput.Call(uintptr(unsafe.Pointer(&devs[0])), uintptr(len(devs)), unsafe.Sizeof(devs[0])); ok == 0 {
		fmt.Println("register raw input:", err)
		return
	}
	names := map[uintptr]string{}
	var msg [64]byte
	buf := make([]byte, 256)
	for {
		if r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0); int32(r) <= 0 {
			return
		}
		if *(*uint32)(unsafe.Pointer(&msg[8])) != 0x00FF { // WM_INPUT
			continue
		}
		lParam := *(*uintptr)(unsafe.Pointer(&msg[24]))
		size := uint32(len(buf))
		if r, _, _ := pGetRawInputData.Call(lParam, 0x10000003, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 24); int32(r) <= 0 {
			continue
		}
		typ := *(*uint32)(unsafe.Pointer(&buf[0]))
		hdev := *(*uintptr)(unsafe.Pointer(&buf[8]))
		name, ok := names[hdev]
		if !ok {
			name = deviceName(hdev)
			names[hdev] = name
		}
		from := "other device"
		if strings.Contains(strings.ToLower(name), "256c") {
			from = "KEYDIAL"
		}
		d := buf[24:]
		t := time.Since(start).Seconds()
		switch typ {
		case 1: // keyboard
			make_ := *(*uint16)(unsafe.Pointer(&d[0]))
			flags := *(*uint16)(unsafe.Pointer(&d[2]))
			vk := *(*uint16)(unsafe.Pointer(&d[6]))
			state := "down"
			if flags&1 != 0 {
				state = "up"
			}
			ext := ""
			if flags&2 != 0 {
				ext = " E0"
			}
			fmt.Printf("%6.2fs  %-12s key  vk=0x%02x scan=0x%02x%s %s\n", t, from, vk, make_, ext, state)
		case 0: // mouse
			bf := *(*uint16)(unsafe.Pointer(&d[4]))
			bd := *(*int16)(unsafe.Pointer(&d[6]))
			dx := *(*int32)(unsafe.Pointer(&d[12]))
			dy := *(*int32)(unsafe.Pointer(&d[16]))
			if from != "KEYDIAL" && bf == 0 {
				continue // ignore ordinary mouse movement
			}
			fmt.Printf("%6.2fs  %-12s mouse buttons=0x%04x data=%d move=%d,%d\n", t, from, bf, bd, dx, dy)
		}
	}
}
