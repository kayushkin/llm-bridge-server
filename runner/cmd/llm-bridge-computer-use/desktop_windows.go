//go:build windows

package main

import (
	"fmt"
	"image"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The primary screen, through user32 and gdi32. The process declares itself
// per-monitor DPI aware first, so the screen size, the cursor and the
// capture are all in physical pixels; without it Windows hands a scaled
// display a smaller, virtual size and clicks land off target.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procSetCursorPos                  = user32.NewProc("SetCursorPos")
	procGetCursorPos                  = user32.NewProc("GetCursorPos")
	procSendInput                     = user32.NewProc("SendInput")
	procVkKeyScanW                    = user32.NewProc("VkKeyScanW")
	procCreateCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap        = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject                  = gdi32.NewProc("SelectObject")
	procBitBlt                        = gdi32.NewProc("BitBlt")
	procGetDIBits                     = gdi32.NewProc("GetDIBits")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procDeleteDC                      = gdi32.NewProc("DeleteDC")
)

const (
	dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4
	smCxScreen                           = 0
	smCyScreen                           = 1
	srcCopy                              = 0x00CC0020
	captureBlt                           = 0x40000000
	biRGB                                = 0
	dibRGBColors                         = 0

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventLeftDown   = 0x0002
	mouseEventLeftUp     = 0x0004
	mouseEventRightDown  = 0x0008
	mouseEventRightUp    = 0x0010
	mouseEventMiddleDown = 0x0020
	mouseEventMiddleUp   = 0x0040
	mouseEventWheel      = 0x0800
	mouseEventHWheel     = 0x1000
	wheelDelta           = 120

	keyEventExtendedKey = 0x0001
	keyEventKeyUp       = 0x0002
	keyEventUnicode     = 0x0004
)

// The INPUT union is 32 bytes on 64-bit Windows, after a 4-byte type and
// 4 bytes of padding: 40 bytes in all.
type mouseInput struct {
	dx, dy                 int32
	mouseData, flags, time uint32
	extraInfo              uintptr
}

type keyboardInput struct {
	virtualKey, scanCode uint16
	flags, time          uint32
	extraInfo            uintptr
	_                    [8]byte
}

type mouseInputEvent struct {
	kind  uint32
	mouse mouseInput
}

type keyboardInputEvent struct {
	kind     uint32
	keyboard keyboardInput
}

type bitmapInfoHeader struct {
	size                         uint32
	width, height                int32
	planes, bitCount             uint16
	compression, sizeImage       uint32
	xPelsPerMeter, yPelsPerMeter int32
	colorsUsed, colorsImportant  uint32
}

type windowsDesktop struct{}

func newDesktop() (desktop, error) {
	if err := procSetProcessDpiAwarenessContext.Find(); err != nil {
		return nil, fmt.Errorf("SetProcessDpiAwarenessContext is missing (Windows 10 1703 or later is needed): %w", err)
	}
	if ok, _, err := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2); ok == 0 {
		return nil, fmt.Errorf("declare per-monitor DPI awareness: %w", err)
	}
	return windowsDesktop{}, nil
}

func (windowsDesktop) screenSize() (int, int, error) {
	w, _, _ := procGetSystemMetrics.Call(smCxScreen)
	h, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if w == 0 || h == 0 {
		return 0, 0, fmt.Errorf("GetSystemMetrics gave a %dx%d screen; is there an interactive desktop (locked screen, or a service session)?", w, h)
	}
	return int(w), int(h), nil
}

func (d windowsDesktop) screenshot() (*image.RGBA, error) {
	width, height, err := d.screenSize()
	if err != nil {
		return nil, err
	}
	screenDC, _, err := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("GetDC: %w", err)
	}
	defer procReleaseDC.Call(0, screenDC)
	memoryDC, _, err := procCreateCompatibleDC.Call(screenDC)
	if memoryDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC: %w", err)
	}
	defer procDeleteDC.Call(memoryDC)
	bitmap, _, err := procCreateCompatibleBitmap.Call(screenDC, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap: %w", err)
	}
	defer procDeleteObject.Call(bitmap)
	previous, _, _ := procSelectObject.Call(memoryDC, bitmap)
	defer procSelectObject.Call(memoryDC, previous)
	if ok, _, err := procBitBlt.Call(memoryDC, 0, 0, uintptr(width), uintptr(height), screenDC, 0, 0, srcCopy|captureBlt); ok == 0 {
		return nil, fmt.Errorf("BitBlt (a locked screen or the secure desktop cannot be captured): %w", err)
	}
	header := bitmapInfoHeader{
		width: int32(width), height: -int32(height), // negative: rows top to bottom
		planes: 1, bitCount: 32, compression: biRGB,
	}
	header.size = uint32(unsafe.Sizeof(header))
	// BITMAPINFO is the header plus a color table, unused at 32 bits per pixel.
	bitmapInfo := struct {
		header bitmapInfoHeader
		colors [4]byte
	}{header: header}
	pixels := make([]byte, width*height*4)
	lines, _, err := procGetDIBits.Call(memoryDC, bitmap, 0, uintptr(height),
		uintptr(unsafe.Pointer(&pixels[0])), uintptr(unsafe.Pointer(&bitmapInfo)), dibRGBColors)
	if int(lines) != height {
		return nil, fmt.Errorf("GetDIBits copied %d of %d rows: %w", lines, height, err)
	}
	// Windows gives blue, green, red, unused; image.RGBA wants red, green, blue, alpha.
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+2], pixels[i+3] = pixels[i+2], pixels[i], 255
	}
	return &image.RGBA{Pix: pixels, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}, nil
}

func (windowsDesktop) moveMouse(x, y int) error {
	if ok, _, err := procSetCursorPos.Call(uintptr(x), uintptr(y)); ok == 0 {
		return fmt.Errorf("SetCursorPos(%d, %d): %w", x, y, err)
	}
	return nil
}

func (windowsDesktop) cursorPosition() (int, int, error) {
	var p struct{ x, y int32 }
	if ok, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); ok == 0 {
		return 0, 0, fmt.Errorf("GetCursorPos: %w", err)
	}
	return int(p.x), int(p.y), nil
}

func sendMouse(flags uint32, data int32) error {
	event := mouseInputEvent{kind: inputMouse, mouse: mouseInput{mouseData: uint32(data), flags: flags}}
	sent, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(&event)), unsafe.Sizeof(event))
	if sent != 1 {
		return fmt.Errorf("SendInput refused the mouse event (blocked by a window running as administrator?): %w", err)
	}
	return nil
}

func (windowsDesktop) pressMouseButton(button mouseButton) error {
	return sendMouse([...]uint32{mouseEventLeftDown, mouseEventRightDown, mouseEventMiddleDown}[button], 0)
}

func (windowsDesktop) releaseMouseButton(button mouseButton) error {
	return sendMouse([...]uint32{mouseEventLeftUp, mouseEventRightUp, mouseEventMiddleUp}[button], 0)
}

func (windowsDesktop) scroll(vertical, horizontal int) error {
	// The wheel counts up as positive; the desktop interface counts down.
	if vertical != 0 {
		if err := sendMouse(mouseEventWheel, int32(-vertical*wheelDelta)); err != nil {
			return err
		}
	}
	if horizontal != 0 {
		return sendMouse(mouseEventHWheel, int32(horizontal*wheelDelta))
	}
	return nil
}

func sendKeyboard(events []keyboardInputEvent) error {
	if len(events) == 0 {
		return nil
	}
	sent, _, err := procSendInput.Call(uintptr(len(events)), uintptr(unsafe.Pointer(&events[0])), unsafe.Sizeof(events[0]))
	if int(sent) != len(events) {
		return fmt.Errorf("SendInput sent %d of %d key events (blocked by a window running as administrator?): %w", sent, len(events), err)
	}
	return nil
}

func (windowsDesktop) typeText(text string) error {
	// Each UTF-16 unit as a Unicode key press: no keyboard layout involved,
	// and characters outside it (é, emoji) arrive as typed.
	var events []keyboardInputEvent
	for _, unit := range windows.StringToUTF16(text) {
		if unit == 0 {
			continue
		}
		if unit == '\n' {
			events = append(events, virtualKeyEvent(keyReturn, false), virtualKeyEvent(keyReturn, true))
			continue
		}
		events = append(events,
			keyboardInputEvent{kind: inputKeyboard, keyboard: keyboardInput{scanCode: unit, flags: keyEventUnicode}},
			keyboardInputEvent{kind: inputKeyboard, keyboard: keyboardInput{scanCode: unit, flags: keyEventUnicode | keyEventKeyUp}})
	}
	// Long text goes in chunks so a slow application keeps up.
	for start := 0; start < len(events); start += 200 {
		if err := sendKeyboard(events[start:min(start+200, len(events))]); err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (d windowsDesktop) pressKeys(keys []string) error {
	if err := d.holdKeys(keys); err != nil {
		return err
	}
	return d.releaseKeys(keys)
}

func (windowsDesktop) holdKeys(keys []string) error {
	var events []keyboardInputEvent
	for _, name := range keys {
		key, err := virtualKeyNamed(name)
		if err != nil {
			return err
		}
		events = append(events, virtualKeyEvent(key, false))
	}
	return sendKeyboard(events)
}

func (windowsDesktop) releaseKeys(keys []string) error {
	var events []keyboardInputEvent
	for i := len(keys) - 1; i >= 0; i-- {
		key, err := virtualKeyNamed(keys[i])
		if err != nil {
			return err
		}
		events = append(events, virtualKeyEvent(key, true))
	}
	return sendKeyboard(events)
}

func virtualKeyEvent(key virtualKey, up bool) keyboardInputEvent {
	flags := uint32(0)
	if key.extended {
		flags |= keyEventExtendedKey
	}
	if up {
		flags |= keyEventKeyUp
	}
	return keyboardInputEvent{kind: inputKeyboard, keyboard: keyboardInput{virtualKey: key.code, flags: flags}}
}

// virtualKeyNamed reads an xdotool key name (Return, ctrl, alt, super, F5,
// Page_Down, a, 7, …), which is how Claude names keys; a single printable
// character falls back to the keyboard layout through VkKeyScanW.
func virtualKeyNamed(name string) (virtualKey, error) {
	if key, ok := namedVirtualKeys[strings.ToLower(name)]; ok {
		return key, nil
	}
	runes := []rune(name)
	if len(runes) == 1 && runes[0] < 0x10000 {
		if c := runes[0]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return virtualKey{code: uint16(strings.ToUpper(string(c))[0])}, nil
		}
		if c := runes[0]; c >= '0' && c <= '9' {
			return virtualKey{code: uint16(c)}, nil
		}
		scan, _, _ := procVkKeyScanW.Call(uintptr(runes[0]))
		if int16(scan) != -1 {
			return virtualKey{code: uint16(scan & 0xff)}, nil
		}
	}
	return virtualKey{}, fmt.Errorf("unknown key %q", name)
}
