//go:build windows

package main

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetupDiGetDeviceProperty = windows.NewLazySystemDLL("setupapi.dll").NewProc("SetupDiGetDevicePropertyW")

// devpkeyBatteryLevel: the battery level Windows reads from a Bluetooth LE
// device's battery service (shown in Settings → Bluetooth & devices).
var devpkeyBatteryLevel = windows.DEVPROPKEY{
	FmtID: windows.DEVPROPGUID{0x104EA319, 0x6EE2, 0x4701, [8]byte{0xBD, 0x47, 0x8D, 0xDB, 0xF4, 0x25, 0xBB, 0xE5}},
	PID:   2,
}

// batteryPercent: the connected Keydial's battery level, -1 if unknown.
func batteryPercent(DeviceMatch) int {
	if theDM == nil {
		return -1
	}
	theDM.mu.Lock()
	name := theDM.present
	theDM.mu.Unlock()
	mac := bluetoothAddress(name)
	if mac == "" {
		return -1
	}
	return bleBattery(mac)
}

// Address: the connected Keydial's Bluetooth address ("20:25:03:11:6D:DE"),
// "" if it isn't connected over Bluetooth.
func (m *DeviceManager) Address() string {
	m.mu.Lock()
	name := m.present
	m.mu.Unlock()
	return formatMAC(bluetoothAddress(name))
}

// bluetoothAddress takes the device address out of a Bluetooth LE Raw Input
// name: …_dev_vid&02256c_pid&8251_rev&0001_202503116dde#… → "202503116dde".
func bluetoothAddress(name string) string {
	name = strings.ToLower(name)
	if !strings.Contains(name, "_dev_vid&02") { // not Bluetooth LE
		return ""
	}
	end := strings.Index(name, "#b&")
	if end < 0 {
		end = len(name)
	}
	i := strings.LastIndex(name[:end], "_")
	if i < 0 || end-i-1 != 12 {
		return ""
	}
	mac := name[i+1 : end]
	for _, c := range mac {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return mac
}

// bleBattery reads the battery level of the device node BTHLE\DEV_<mac>\….
func bleBattery(mac string) int {
	set, err := windows.SetupDiGetClassDevsEx(nil, "BTHLE", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return -1
	}
	defer set.Close()
	want := `BTHLE\DEV_` + strings.ToUpper(mac) + `\`
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if err != nil {
			return -1 // no more devices
		}
		id, err := windows.SetupDiGetDeviceInstanceId(set, data)
		if err != nil || !strings.HasPrefix(strings.ToUpper(id), want) {
			continue
		}
		var typ uint32
		var buf [8]byte
		var size uint32
		ok, _, _ := procSetupDiGetDeviceProperty.Call(uintptr(set), uintptr(unsafe.Pointer(data)),
			uintptr(unsafe.Pointer(&devpkeyBatteryLevel)), uintptr(unsafe.Pointer(&typ)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&size)), 0)
		if ok == 0 || size < 1 || buf[0] > 100 {
			return -1
		}
		return int(buf[0]) // DEVPROP_TYPE_BYTE
	}
}
