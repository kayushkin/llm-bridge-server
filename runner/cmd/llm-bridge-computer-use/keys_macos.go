package main

import "strings"

// macModifierNamed maps an xdotool modifier name to System Events' word for
// it. super, cmd, meta and win are all the Command key on a Mac.
func macModifierNamed(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "cmd", "command", "super", "super_l", "super_r", "meta", "win":
		return "command", true
	case "ctrl", "control", "control_l", "control_r":
		return "control", true
	case "alt", "alt_l", "alt_r", "option":
		return "option", true
	case "shift", "shift_l", "shift_r":
		return "shift", true
	}
	return "", false
}

// macKeyCodes maps xdotool's names for keys that are not characters,
// lower-cased, to macOS virtual key codes. Characters go through System
// Events' keystroke instead, which follows the keyboard layout. Kept free of
// build tags so a test on any system checks the table.
var macKeyCodes = map[string]int{
	"return": 36, "enter": 36, "kp_enter": 76, "tab": 48, "space": 49,
	"backspace": 51, "delete": 117, "escape": 53, "esc": 53,
	"home": 115, "end": 119, "page_up": 116, "prior": 116, "pageup": 116,
	"page_down": 121, "next": 121, "pagedown": 121,
	"left": 123, "right": 124, "down": 125, "up": 126,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100,
	"f9": 101, "f10": 109, "f11": 103, "f12": 111,
}
