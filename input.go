package main

// InputEvent is one event from the device (Linux input_event semantics on every OS).
type InputEvent struct {
	Type  uint16
	Code  uint16
	Value int32
}

// DeviceInfo describes an open Keydial input node for the panel.
type DeviceInfo struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Grabbed bool   `json:"grabbed"`
}
