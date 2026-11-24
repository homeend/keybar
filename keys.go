package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Linux input event types / codes we care about.
const (
	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02

	synReport = 0

	relX           = 0x00
	relY           = 0x01
	relHWheel      = 0x06
	relDial        = 0x07
	relWheel       = 0x08
	relMisc        = 0x09
	relWheelHiRes  = 0x0b
	relHWheelHiRes = 0x0c

	btnLeft    = 0x110
	btnRight   = 0x111
	btnMiddle  = 0x112
	btnSide    = 0x113
	btnExtra   = 0x114
	btnForward = 0x115
	btnBack    = 0x116
	btnTask    = 0x117
)

var relNames = map[uint16]string{
	relX: "REL_X", relY: "REL_Y", relHWheel: "REL_HWHEEL", relDial: "REL_DIAL",
	relWheel: "REL_WHEEL", relMisc: "REL_MISC",
}

var (
	keyByName = map[string]uint16{} // "KEY_A" -> 30
	nameByKey = map[uint16]string{} // 30 -> "KEY_A"
)

// Group names that share a value with a more specific name; never used for display.
var groupNames = map[string]bool{
	"KEY_MIN_INTERESTING": true, "BTN_MISC": true, "BTN_MOUSE": true, "BTN_JOYSTICK": true,
	"BTN_GAMEPAD": true, "BTN_DIGI": true, "BTN_WHEEL": true, "BTN_TRIGGER_HAPPY": true,
}

func init() {
	pending := rawKeyDefs
	for pass := 0; pass < 4 && len(pending) > 0; pass++ {
		var next [][2]string
		for _, d := range pending {
			name, val := d[0], d[1]
			var code uint16
			if n, err := strconv.ParseUint(val, 0, 16); err == nil {
				code = uint16(n)
			} else if c, ok := keyByName[val]; ok {
				code = c
			} else {
				next = append(next, d)
				continue
			}
			keyByName[name] = code
			if _, taken := nameByKey[code]; !taken && !groupNames[name] {
				nameByKey[code] = name
			}
		}
		pending = next
	}
	typeMap = buildTypeMap()
}

func keyName(code uint16) string {
	if n, ok := nameByKey[code]; ok {
		return n
	}
	return fmt.Sprintf("KEY_%d", code)
}

func isModifier(code uint16) bool {
	switch code {
	case 29, 97, 42, 54, 56, 100, 125, 126: // L/R ctrl, shift, alt, meta
		return true
	}
	return false
}

// Friendly aliases accepted in shortcut strings ("ctrl+shift+t", "super", "esc").
var keyAliases = map[string]string{
	"ctrl": "KEY_LEFTCTRL", "control": "KEY_LEFTCTRL", "lctrl": "KEY_LEFTCTRL", "rctrl": "KEY_RIGHTCTRL",
	"shift": "KEY_LEFTSHIFT", "lshift": "KEY_LEFTSHIFT", "rshift": "KEY_RIGHTSHIFT",
	"alt": "KEY_LEFTALT", "lalt": "KEY_LEFTALT", "ralt": "KEY_RIGHTALT", "altgr": "KEY_RIGHTALT",
	"super": "KEY_LEFTMETA", "meta": "KEY_LEFTMETA", "win": "KEY_LEFTMETA", "cmd": "KEY_LEFTMETA", "rsuper": "KEY_RIGHTMETA",
	"esc": "KEY_ESC", "escape": "KEY_ESC", "return": "KEY_ENTER", "del": "KEY_DELETE", "ins": "KEY_INSERT",
	"pgup": "KEY_PAGEUP", "pgdn": "KEY_PAGEDOWN", "pgdown": "KEY_PAGEDOWN", "bksp": "KEY_BACKSPACE",
	"print": "KEY_SYSRQ", "prtsc": "KEY_SYSRQ", "printscreen": "KEY_SYSRQ", "menu": "KEY_COMPOSE",
	"volup": "KEY_VOLUMEUP", "voldown": "KEY_VOLUMEDOWN", "play": "KEY_PLAYPAUSE", "pause": "KEY_PLAYPAUSE",
	"next": "KEY_NEXTSONG", "prev": "KEY_PREVIOUSSONG", "previous": "KEY_PREVIOUSSONG",
	"-": "KEY_MINUS", "=": "KEY_EQUAL", ",": "KEY_COMMA", ".": "KEY_DOT", "/": "KEY_SLASH",
	";": "KEY_SEMICOLON", "'": "KEY_APOSTROPHE", "[": "KEY_LEFTBRACE", "]": "KEY_RIGHTBRACE",
	"\\": "KEY_BACKSLASH", "`": "KEY_GRAVE", "period": "KEY_DOT", "quote": "KEY_APOSTROPHE",
}

func parseKeyName(s string) (uint16, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty key name")
	}
	if a, ok := keyAliases[strings.ToLower(t)]; ok {
		t = a
	}
	u := strings.ToUpper(t)
	if c, ok := keyByName[u]; ok {
		return c, nil
	}
	if c, ok := keyByName["KEY_"+u]; ok {
		return c, nil
	}
	if c, ok := keyByName["BTN_"+u]; ok {
		return c, nil
	}
	return 0, fmt.Errorf("unknown key %q", s)
}

// parseShortcut turns "ctrl+shift+t, esc" into a sequence of combos.
func parseShortcut(s string) ([][]uint16, error) {
	var seq [][]uint16
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		var combo []uint16
		for _, k := range strings.Split(part, "+") {
			if k == "" {
				continue
			}
			c, err := parseKeyName(k)
			if err != nil {
				return nil, err
			}
			combo = append(combo, c)
		}
		if len(combo) > 0 {
			seq = append(seq, combo)
		}
	}
	if len(seq) == 0 {
		return nil, fmt.Errorf("no keys in %q", s)
	}
	return seq, nil
}

// signature builds the canonical trigger name: modifiers sorted by code, then the key.
func signature(mods map[uint16]bool, last string) string {
	codes := make([]int, 0, len(mods))
	for c := range mods {
		codes = append(codes, int(c))
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes)+1)
	for _, c := range codes {
		parts = append(parts, keyName(uint16(c)))
	}
	if last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, "+")
}

// US-layout typing table: rune -> (key name, needs shift).
type typedKey struct {
	code  uint16
	shift bool
}

var typeMap map[rune]typedKey // built in init, after keyByName

func buildTypeMap() map[rune]typedKey {
	m := map[rune]typedKey{}
	add := func(name string, plain, shifted rune) {
		c := keyByNameMust(name)
		m[plain] = typedKey{c, false}
		if shifted != 0 {
			m[shifted] = typedKey{c, true}
		}
	}
	for r := 'a'; r <= 'z'; r++ {
		add("KEY_"+strings.ToUpper(string(r)), r, r-'a'+'A')
	}
	pairs := []struct {
		name    string
		p, s    rune
	}{
		{"KEY_GRAVE", '`', '~'}, {"KEY_1", '1', '!'}, {"KEY_2", '2', '@'}, {"KEY_3", '3', '#'},
		{"KEY_4", '4', '$'}, {"KEY_5", '5', '%'}, {"KEY_6", '6', '^'}, {"KEY_7", '7', '&'},
		{"KEY_8", '8', '*'}, {"KEY_9", '9', '('}, {"KEY_0", '0', ')'}, {"KEY_MINUS", '-', '_'},
		{"KEY_EQUAL", '=', '+'}, {"KEY_LEFTBRACE", '[', '{'}, {"KEY_RIGHTBRACE", ']', '}'},
		{"KEY_BACKSLASH", '\\', '|'}, {"KEY_SEMICOLON", ';', ':'}, {"KEY_APOSTROPHE", '\'', '"'},
		{"KEY_COMMA", ',', '<'}, {"KEY_DOT", '.', '>'}, {"KEY_SLASH", '/', '?'},
		{"KEY_SPACE", ' ', 0}, {"KEY_ENTER", '\n', 0}, {"KEY_TAB", '\t', 0},
	}
	for _, p := range pairs {
		add(p.name, p.p, p.s)
	}
	return m
}

func keyByNameMust(n string) uint16 {
	c, ok := keyByName[n]
	if !ok {
		panic("missing key " + n)
	}
	return c
}
