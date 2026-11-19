//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The main display, through tools every Mac has, so the program builds
// without cgo (and so cross-compiles from Linux): screencapture takes the
// screenshot, and osascript's JavaScript posts mouse events through
// CoreGraphics and keystrokes through System Events.
//
// Coordinates are points, which is what CoreGraphics events take; a Retina
// screenshot is twice that in pixels and is scaled down with the rest.
//
// macOS asks once for Screen Recording (without it screencapture shows only
// the wallpaper) and Accessibility (without it events are dropped), naming
// the app that started the chain.

// mouseScript runs one CoreGraphics operation described by its JSON argument.
const mouseScript = `
ObjC.import('Cocoa');
function run(argv) {
  const c = JSON.parse(argv[0]);
  const flagBits = {command: 0x100000, shift: 0x20000, option: 0x80000, control: 0x40000};
  let flags = 0;
  (c.modifiers || []).forEach(function (m) { flags |= flagBits[m]; });
  function post(e) {
    if (flags) { $.CGEventSetFlags(e, flags); }
    $.CGEventPost($.kCGHIDEventTap, e);
  }
  if (c.op === 'size') {
    const b = $.CGDisplayBounds($.CGMainDisplayID());
    return JSON.stringify({w: b.size.width, h: b.size.height});
  }
  if (c.op === 'cursor') {
    const p = $.CGEventGetLocation($.CGEventCreate($()));
    return JSON.stringify({x: p.x, y: p.y});
  }
  if (c.op === 'mouse') {
    const e = $.CGEventCreateMouseEvent($(), c.type, $.CGPointMake(c.x, c.y), c.button);
    if (c.clicks) { $.CGEventSetIntegerValueField(e, $.kCGMouseEventClickState, c.clicks); }
    post(e);
    return '';
  }
  if (c.op === 'scroll') {
    if (typeof $.CGEventCreateScrollWheelEvent2 !== 'function') {
      throw new Error('CGEventCreateScrollWheelEvent2 is not available to osascript on this macOS');
    }
    post($.CGEventCreateScrollWheelEvent2($(), $.kCGScrollEventUnitLine, 2, c.dy, c.dx, 0));
    return '';
  }
  throw new Error('unknown op ' + c.op);
}`

// keyScript types text or presses one key through System Events.
const keyScript = `
function run(argv) {
  const c = JSON.parse(argv[0]);
  const se = Application('System Events');
  const using = (c.modifiers || []).map(function (m) { return m + ' down'; });
  if (c.op === 'type') { se.keystroke(c.text); return ''; }
  if (c.op === 'keycode') { se.keyCode(c.code, {using: using}); return ''; }
  if (c.op === 'keystroke') { se.keystroke(c.text, {using: using}); return ''; }
  throw new Error('unknown op ' + c.op);
}`

// CoreGraphics event types and buttons.
const (
	cgLeftMouseDown    = 1
	cgLeftMouseUp      = 2
	cgRightMouseDown   = 3
	cgRightMouseUp     = 4
	cgMouseMoved       = 5
	cgLeftMouseDragged = 6
	cgOtherMouseDown   = 25
	cgOtherMouseUp     = 26
)

// A press within this long of the last, at the same spot, is the next click
// of a double or triple click; macOS counts clicks from the event's click
// state, not from timing, so the tool has to say which click it is.
const multipleClickWindow = 500 * time.Millisecond

type macDesktop struct {
	mu            sync.Mutex
	cursorX       int
	cursorY       int
	leftDown      bool
	heldModifiers []string
	lastPress     time.Time
	lastPressX    int
	lastPressY    int
	clickCount    int
}

func newDesktop() (desktop, error) {
	d := &macDesktop{}
	x, y, err := d.cursorPosition()
	if err != nil {
		return nil, fmt.Errorf("osascript cannot read the cursor (is this a GUI login session?): %w", err)
	}
	d.cursorX, d.cursorY = x, y
	return d, nil
}

func runOSAScript(script string, command any) (string, error) {
	argument, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("osascript", "-l", "JavaScript", "-e", script, string(argument))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *macDesktop) screenSize() (int, int, error) {
	out, err := runOSAScript(mouseScript, map[string]any{"op": "size"})
	if err != nil {
		return 0, 0, err
	}
	var size struct{ W, H float64 }
	if err := json.Unmarshal([]byte(out), &size); err != nil || size.W <= 0 || size.H <= 0 {
		return 0, 0, fmt.Errorf("read the display size from %q: %v", out, err)
	}
	return int(size.W), int(size.H), nil
}

func (d *macDesktop) screenshot() (*image.RGBA, error) {
	// Declare the user active, which lights a display that went dark, before
	// looking at it. Started, not waited for: caffeinate -u holds the
	// assertion for its -t seconds and exits on its own.
	if err := exec.Command("caffeinate", "-u", "-t", "2").Start(); err != nil {
		return nil, fmt.Errorf("wake the display: %w", err)
	}
	directory, err := os.MkdirTemp("", "llm-bridge-computer-use-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "screen.png")
	// -x: no sound, -m: the main display only.
	if out, err := exec.Command("screencapture", "-x", "-m", "-t", "png", path).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("screencapture: %w: %s", err, strings.TrimSpace(string(out)))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("screencapture wrote nothing (Screen Recording not allowed?): %w", err)
	}
	defer file.Close()
	decoded, err := png.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("read the screenshot: %w", err)
	}
	if rgba, ok := decoded.(*image.RGBA); ok {
		return rgba, nil
	}
	rgba := image.NewRGBA(decoded.Bounds())
	draw.Draw(rgba, rgba.Rect, decoded, decoded.Bounds().Min, draw.Src)
	return rgba, nil
}

func (d *macDesktop) mouse(eventType, button, x, y, clicks int) error {
	d.mu.Lock()
	modifiers := append([]string(nil), d.heldModifiers...)
	d.mu.Unlock()
	_, err := runOSAScript(mouseScript, map[string]any{
		"op": "mouse", "type": eventType, "button": button, "x": x, "y": y, "clicks": clicks, "modifiers": modifiers,
	})
	return err
}

func (d *macDesktop) moveMouse(x, y int) error {
	d.mu.Lock()
	eventType := cgMouseMoved
	if d.leftDown {
		eventType = cgLeftMouseDragged
	}
	d.mu.Unlock()
	if err := d.mouse(eventType, 0, x, y, 0); err != nil {
		return err
	}
	d.mu.Lock()
	d.cursorX, d.cursorY = x, y
	d.mu.Unlock()
	return nil
}

func (d *macDesktop) cursorPosition() (int, int, error) {
	out, err := runOSAScript(mouseScript, map[string]any{"op": "cursor"})
	if err != nil {
		return 0, 0, err
	}
	var p struct{ X, Y float64 }
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return 0, 0, fmt.Errorf("read the cursor from %q: %w", out, err)
	}
	return int(p.X), int(p.Y), nil
}

func (d *macDesktop) pressMouseButton(button mouseButton) error {
	d.mu.Lock()
	now := time.Now()
	if now.Sub(d.lastPress) < multipleClickWindow && d.lastPressX == d.cursorX && d.lastPressY == d.cursorY {
		d.clickCount++
	} else {
		d.clickCount = 1
	}
	d.lastPress, d.lastPressX, d.lastPressY = now, d.cursorX, d.cursorY
	x, y, clicks := d.cursorX, d.cursorY, d.clickCount
	if button == leftButton {
		d.leftDown = true
	}
	d.mu.Unlock()
	eventType, cgButton := [...]int{cgLeftMouseDown, cgRightMouseDown, cgOtherMouseDown}[button], [...]int{0, 1, 2}[button]
	return d.mouse(eventType, cgButton, x, y, clicks)
}

func (d *macDesktop) releaseMouseButton(button mouseButton) error {
	d.mu.Lock()
	x, y, clicks := d.cursorX, d.cursorY, d.clickCount
	if button == leftButton {
		d.leftDown = false
	}
	d.mu.Unlock()
	eventType, cgButton := [...]int{cgLeftMouseUp, cgRightMouseUp, cgOtherMouseUp}[button], [...]int{0, 1, 2}[button]
	return d.mouse(eventType, cgButton, x, y, clicks)
}

func (d *macDesktop) scroll(vertical, horizontal int) error {
	d.mu.Lock()
	modifiers := append([]string(nil), d.heldModifiers...)
	d.mu.Unlock()
	// CoreGraphics counts a positive wheel as up/left; the interface counts down/right.
	_, err := runOSAScript(mouseScript, map[string]any{"op": "scroll", "dy": -vertical, "dx": -horizontal, "modifiers": modifiers})
	return err
}

func (d *macDesktop) typeText(text string) error {
	_, err := runOSAScript(keyScript, map[string]any{"op": "type", "text": text})
	return err
}

// pressKeys presses one combination: modifiers held, one other key.
func (d *macDesktop) pressKeys(keys []string) error {
	var modifiers []string
	var main []string
	for _, k := range keys {
		if m, ok := macModifierNamed(k); ok {
			modifiers = append(modifiers, m)
		} else {
			main = append(main, k)
		}
	}
	d.mu.Lock()
	modifiers = append(modifiers, d.heldModifiers...)
	d.mu.Unlock()
	if len(main) != 1 {
		return fmt.Errorf("a key combination needs exactly one key besides modifiers, got %q", strings.Join(keys, "+"))
	}
	if code, ok := macKeyCodes[strings.ToLower(main[0])]; ok {
		_, err := runOSAScript(keyScript, map[string]any{"op": "keycode", "code": code, "modifiers": modifiers})
		return err
	}
	if len([]rune(main[0])) == 1 {
		_, err := runOSAScript(keyScript, map[string]any{"op": "keystroke", "text": strings.ToLower(main[0]), "modifiers": modifiers})
		return err
	}
	return fmt.Errorf("unknown key %q", main[0])
}

// holdKeys holds modifiers for the clicks and scrolls that follow; System
// Events cannot hold an ordinary key down, so those are refused.
func (d *macDesktop) holdKeys(keys []string) error {
	var modifiers []string
	for _, k := range keys {
		m, ok := macModifierNamed(k)
		if !ok {
			return fmt.Errorf("on macOS only modifier keys can be held, not %q", k)
		}
		modifiers = append(modifiers, m)
	}
	d.mu.Lock()
	d.heldModifiers = append(d.heldModifiers, modifiers...)
	d.mu.Unlock()
	return nil
}

func (d *macDesktop) releaseKeys(keys []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range keys {
		m, ok := macModifierNamed(k)
		if !ok {
			continue
		}
		for i, held := range d.heldModifiers {
			if held == m {
				d.heldModifiers = append(d.heldModifiers[:i], d.heldModifiers[i+1:]...)
				break
			}
		}
	}
	return nil
}
