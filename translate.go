package main

import (
	"fmt"
	"strings"
)

// Copying layouts between machines. Shortcuts are stored with Linux key names
// on every OS (Super is the Windows key), so most copy unchanged; only
// OS-specific gestures are converted. Apps are matched by name; commands only
// make sense on the same OS.

// shortcutEquivalents: the same gesture on each OS.
var shortcutEquivalents = []map[string]string{
	{"linux": "super", "windows": "super+tab"},                         // overview / task view
	{"linux": "super+pageup", "windows": "ctrl+super+left"},            // previous workspace / desktop
	{"linux": "super+pagedown", "windows": "ctrl+super+right"},         // next workspace / desktop
	{"linux": "print", "windows": "super+shift+s"},                     // screenshot
	{"linux": "super+a", "windows": "super+a"},                         // quick settings (same)
	{"linux": "super+alt+left", "windows": "ctrl+super+left"},          // alt. workspace keys
	{"linux": "super+alt+right", "windows": "ctrl+super+right"},
}

func convertShortcut(keys, from, to string) (string, bool) {
	if from == to {
		return keys, false
	}
	norm := strings.ToLower(strings.ReplaceAll(keys, " ", ""))
	for _, eq := range shortcutEquivalents {
		if eq[from] == norm && eq[to] != "" && eq[to] != norm {
			return eq[to], true
		}
	}
	return keys, false
}

// CopyReport summarises an import for the panel.
type CopyReport struct {
	Layouts   int `json:"layouts"`
	Copied    int `json:"copied"`    // unchanged
	Converted int `json:"converted"` // changed to this OS's equivalent
	Attention int `json:"attention"` // marked ⚠
}

var osName = map[string]string{"linux": "Linux", "windows": "Windows", "darwin": "macOS"}

// importLayouts converts layouts from another machine for this one and gives
// them fresh IDs and unique names.
func importLayouts(src []Layout, fromOS, fromName string, existing []Layout, apps []App) ([]Layout, CopyReport) {
	to := hostOS()
	rep := CopyReport{}
	names := map[string]bool{}
	for _, l := range existing {
		names[strings.ToLower(l.Name)] = true
	}
	newID := map[string]string{}
	for i, l := range src {
		newID[l.ID] = fmt.Sprintf("i%s%d", strings.ReplaceAll(randID(), "-", ""), i)
	}
	byName := map[string]App{}
	for _, a := range apps {
		byName[strings.ToLower(a.Name)] = a
	}
	var out []Layout
	for _, l := range src {
		name := l.Name + " (" + fromName + ")"
		for i := 2; names[strings.ToLower(name)]; i++ {
			name = fmt.Sprintf("%s (%s) %d", l.Name, fromName, i)
		}
		names[strings.ToLower(name)] = true
		nl := Layout{ID: newID[l.ID], Name: name, Keys: map[string]Key{}}
		for slot, k := range l.Keys {
			k.Note, k.Info = "", ""
			a := &k.Action
			switch a.Type {
			case "none", "":
				nl.Keys[slot] = k
				continue
			case "keys":
				if c, ok := convertShortcut(a.Keys, fromOS, to); ok {
					k.Info = fmt.Sprintf("Converted from the %s shortcut %s", osName[fromOS], a.Keys)
					a.Keys = c
					rep.Converted++
				} else {
					rep.Copied++
				}
			case "app":
				if app, ok := byName[strings.ToLower(a.AppName)]; ok && a.AppName != "" {
					if app.ID != a.App {
						k.Info = fmt.Sprintf("Matched “%s” to this machine's app", app.Name)
						rep.Converted++
					} else {
						rep.Copied++
					}
					a.App, a.AppName = app.ID, app.Name
				} else {
					k.Note = fmt.Sprintf("Opened “%s” on %s; no app with that name here", firstNonEmpty(a.AppName, a.App), fromName)
					*a = Action{Type: "none"}
					rep.Attention++
				}
			case "command":
				if fromOS == to {
					rep.Copied++
				} else {
					k.Note = fmt.Sprintf("Ran on %s: %s", osName[fromOS], strings.TrimSpace(a.Command+" "+a.ReleaseCommand))
					*a = Action{Type: "none"}
					rep.Attention++
				}
			case "layout":
				if id, ok := newID[a.Layout]; ok {
					a.Layout = id
				} else if a.Layout != "next" && a.Layout != "prev" {
					a.Layout = "next"
				}
				rep.Copied++
			case "passthrough":
				k.Note = "Was the device's factory key on " + fromName
				*a = Action{Type: "none"}
				rep.Attention++
			default: // text, mouse, machine
				rep.Copied++
			}
			nl.Keys[slot] = k
		}
		out = append(out, nl)
		rep.Layouts++
	}
	return out, rep
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func randID() string { return newToken()[:8] }
