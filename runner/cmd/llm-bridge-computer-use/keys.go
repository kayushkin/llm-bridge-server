package main

import "fmt"

// virtualKey is a Windows virtual-key code, and whether Windows sends it as
// an extended key (the arrows and the navigation block, which otherwise
// arrive as the number pad's versions).
type virtualKey struct {
	code     uint16
	extended bool
}

var keyReturn = virtualKey{code: 0x0D}

// namedVirtualKeys maps xdotool's key names, lower-cased, to Windows keys.
// Kept free of build tags so a test on any system checks the table.
var namedVirtualKeys = func() map[string]virtualKey {
	keys := map[string]virtualKey{
		"return": keyReturn, "enter": keyReturn, "kp_enter": {code: 0x0D, extended: true},
		"tab": {code: 0x09}, "iso_left_tab": {code: 0x09},
		"escape": {code: 0x1B}, "esc": {code: 0x1B},
		"backspace": {code: 0x08}, "space": {code: 0x20},
		"delete": {code: 0x2E, extended: true}, "insert": {code: 0x2D, extended: true},
		"home": {code: 0x24, extended: true}, "end": {code: 0x23, extended: true},
		"page_up": {code: 0x21, extended: true}, "prior": {code: 0x21, extended: true}, "pageup": {code: 0x21, extended: true},
		"page_down": {code: 0x22, extended: true}, "next": {code: 0x22, extended: true}, "pagedown": {code: 0x22, extended: true},
		"up": {code: 0x26, extended: true}, "down": {code: 0x28, extended: true},
		"left": {code: 0x25, extended: true}, "right": {code: 0x27, extended: true},
		"ctrl": {code: 0x11}, "control": {code: 0x11}, "control_l": {code: 0xA2}, "control_r": {code: 0xA3, extended: true},
		"alt": {code: 0x12}, "alt_l": {code: 0xA4}, "alt_r": {code: 0xA5, extended: true},
		"shift": {code: 0x10}, "shift_l": {code: 0xA0}, "shift_r": {code: 0xA1},
		// The Windows key; Claude says super, cmd or meta depending on the system it thinks it is on.
		"super": {code: 0x5B, extended: true}, "super_l": {code: 0x5B, extended: true}, "super_r": {code: 0x5C, extended: true},
		"win": {code: 0x5B, extended: true}, "cmd": {code: 0x5B, extended: true}, "meta": {code: 0x5B, extended: true},
		"menu": {code: 0x5D, extended: true}, "print": {code: 0x2C, extended: true},
		"caps_lock": {code: 0x14}, "num_lock": {code: 0x90, extended: true}, "scroll_lock": {code: 0x91}, "pause": {code: 0x13},
		"minus": {code: 0xBD}, "equal": {code: 0xBB}, "plus": {code: 0xBB}, "comma": {code: 0xBC}, "period": {code: 0xBE},
		"slash": {code: 0xBF}, "backslash": {code: 0xDC}, "semicolon": {code: 0xBA}, "apostrophe": {code: 0xDE},
		"grave": {code: 0xC0}, "bracketleft": {code: 0xDB}, "bracketright": {code: 0xDD},
		"audiovolumeup": {code: 0xAF, extended: true}, "audiovolumedown": {code: 0xAE, extended: true}, "audiomute": {code: 0xAD, extended: true},
	}
	for n := 1; n <= 24; n++ {
		keys[fmt.Sprintf("f%d", n)] = virtualKey{code: uint16(0x6F + n)}
	}
	return keys
}()
