//go:build windows

package main

import "context"

// On Windows Keybar only receives presses from a paired machine the Keydial is
// plugged into; reading the device here is a later step. These stand-ins keep
// the rest of the program the same on both systems.

type DeviceManager struct {
	onClose func()
}

func NewDeviceManager(store *ConfigStore, em *Emitter, out chan<- InputEvent, onChange func()) *DeviceManager {
	return &DeviceManager{}
}

func (m *DeviceManager) Run()                  {}
func (m *DeviceManager) Reset()                {}
func (m *DeviceManager) Devices() []DeviceInfo { return []DeviceInfo{} }
func (m *DeviceManager) AnyGrabbed() bool      { return false }
func (m *DeviceManager) Paused() bool          { return false }
func (m *DeviceManager) SetPaused(bool)        {}

func batteryPercent(DeviceMatch) int { return -1 }

// captureShortcut: the panel records shortcuts in the browser on Windows.
func captureShortcut(ctx context.Context, skip DeviceMatch) string { return "" }
