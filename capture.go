//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"sync"
	"syscall"
)

// captureShortcut listens (without grabbing) to every physical keyboard and
// returns the next shortcut pressed, e.g. "ctrl+alt+q". Reading evdev directly
// sees shortcuts GNOME consumes before any window gets them.
func captureShortcut(ctx context.Context, skip DeviceMatch) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type keyEv struct {
		code uint16
		down bool
	}
	evs := make(chan keyEv, 64)
	var files []*os.File
	for _, d := range listInputDevices() {
		if !d.Keyboard || skip.matches(d) || strings.HasPrefix(d.Name, virtualPrefix) {
			continue
		}
		for _, p := range d.Events {
			f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				files = append(files, f)
			}
		}
	}
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		go func(f *os.File) {
			defer wg.Done()
			buf := make([]byte, eventSize*32)
			for {
				n, err := f.Read(buf)
				if err != nil {
					return
				}
				for off := 0; off+eventSize <= n; off += eventSize {
					b := buf[off:]
					if binary.LittleEndian.Uint16(b[16:]) != evKey {
						continue
					}
					v := int32(binary.LittleEndian.Uint32(b[20:]))
					if v == 2 {
						continue
					}
					select {
					case evs <- keyEv{binary.LittleEndian.Uint16(b[18:]), v == 1}:
					case <-ctx.Done():
						return
					}
				}
			}
		}(f)
	}
	defer func() {
		for _, f := range files {
			f.Close() // unblocks the readers
		}
		wg.Wait()
	}()

	held := map[uint16]bool{}
	seen := map[uint16]bool{}
	for {
		select {
		case <-ctx.Done():
			return ""
		case e := <-evs:
			if isModifier(e.code) {
				if e.down {
					held[e.code], seen[e.code] = true, true
					continue
				}
				delete(held, e.code)
				if len(held) == 0 && len(seen) > 0 { // modifier(s) alone, e.g. Super
					return shortcutString(seen, 0)
				}
				continue
			}
			if e.down && (e.code < btnLeft || e.code > btnTask) {
				return shortcutString(held, e.code)
			}
		}
	}
}

