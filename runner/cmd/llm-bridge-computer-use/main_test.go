package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeDesktop is a 2560×1600 screen that records what was done to it.
type fakeDesktop struct {
	actions          []string
	cursorX, cursorY int
}

func (f *fakeDesktop) screenSize() (int, int, error) { return 2560, 1600, nil }
func (f *fakeDesktop) screenshot() (*image.RGBA, error) {
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1600))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	return img, nil
}
func (f *fakeDesktop) moveMouse(x, y int) error {
	f.cursorX, f.cursorY = x, y
	f.actions = append(f.actions, fmt.Sprintf("move %d,%d", x, y))
	return nil
}
func (f *fakeDesktop) cursorPosition() (int, int, error) { return f.cursorX, f.cursorY, nil }
func (f *fakeDesktop) pressMouseButton(b mouseButton) error {
	f.actions = append(f.actions, fmt.Sprintf("down %d", b))
	return nil
}
func (f *fakeDesktop) releaseMouseButton(b mouseButton) error {
	f.actions = append(f.actions, fmt.Sprintf("up %d", b))
	return nil
}
func (f *fakeDesktop) scroll(v, h int) error {
	f.actions = append(f.actions, fmt.Sprintf("scroll %d,%d", v, h))
	return nil
}
func (f *fakeDesktop) typeText(t string) error {
	f.actions = append(f.actions, "type "+t)
	return nil
}
func (f *fakeDesktop) pressKeys(k []string) error {
	f.actions = append(f.actions, "keys "+strings.Join(k, "+"))
	return nil
}
func (f *fakeDesktop) holdKeys(k []string) error {
	f.actions = append(f.actions, "hold "+strings.Join(k, "+"))
	return nil
}
func (f *fakeDesktop) releaseKeys(k []string) error {
	f.actions = append(f.actions, "release "+strings.Join(k, "+"))
	return nil
}

func connect(t *testing.T, d desktop) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := newComputerServer(d).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "computer", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScreenshotIsTheScreenScaledToFitAPNG(t *testing.T) {
	session := connect(t, &fakeDesktop{})
	result := call(t, session, map[string]any{"action": "screenshot"})
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("result = %+v", result)
	}
	img, ok := result.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" {
		t.Fatalf("content = %#v", result.Content[0])
	}
	decoded, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	if b := decoded.Bounds(); b.Dx() != 1280 || b.Dy() != 800 {
		t.Errorf("screenshot is %dx%d, want 1280x800 (2560x1600 halved)", b.Dx(), b.Dy())
	}
}

func TestClicksLandOnTheScreenPixelUnderTheScreenshotPoint(t *testing.T) {
	d := &fakeDesktop{}
	session := connect(t, d)
	result := call(t, session, map[string]any{"action": "double_click", "coordinate": []int{100, 50}, "text": "shift"})
	if result.IsError {
		t.Fatalf("error: %+v", result.Content)
	}
	want := "move 200,100|hold shift|down 0|up 0|down 0|up 0|release shift"
	if got := strings.Join(d.actions, "|"); got != want {
		t.Errorf("actions = %s\nwant      %s", got, want)
	}
	if _, ok := result.Content[len(result.Content)-1].(*mcp.ImageContent); !ok {
		t.Error("an action should answer with a screenshot")
	}
}

func TestDragKeysScrollTypeAndCursor(t *testing.T) {
	d := &fakeDesktop{}
	session := connect(t, d)
	call(t, session, map[string]any{"action": "left_click_drag", "start_coordinate": []int{10, 10}, "coordinate": []int{20, 30}})
	call(t, session, map[string]any{"action": "key", "text": "ctrl+a Delete"})
	call(t, session, map[string]any{"action": "scroll", "coordinate": []int{5, 5}, "scroll_direction": "up", "scroll_amount": 3})
	call(t, session, map[string]any{"action": "type", "text": "héllo"})
	want := "move 20,20|down 0|move 40,60|up 0|keys ctrl+a|keys Delete|move 10,10|scroll -3,0|type héllo"
	if got := strings.Join(d.actions, "|"); got != want {
		t.Errorf("actions = %s\nwant      %s", got, want)
	}
	result := call(t, session, map[string]any{"action": "cursor_position"})
	if text := result.Content[0].(*mcp.TextContent).Text; text != "X=5,Y=5" {
		t.Errorf("cursor = %q, want the screenshot's coordinates X=5,Y=5", text)
	}
}

func TestBadActionsAreToolErrorsAndDoNothing(t *testing.T) {
	d := &fakeDesktop{}
	session := connect(t, d)
	for _, arguments := range []map[string]any{
		{"action": "explode"},
		{"action": "left_click", "coordinate": []int{1280, 10}}, // off the 1280-wide screenshot
		{"action": "mouse_move"},
		{"action": "scroll", "scroll_direction": "sideways", "scroll_amount": 1},
		{"action": "type"},
	} {
		if result := call(t, session, arguments); !result.IsError {
			t.Errorf("%v: no error", arguments)
		}
	}
	if len(d.actions) != 0 {
		t.Errorf("refused actions still acted: %v", d.actions)
	}
}

func TestScaleRoundTripsAndKeepsSmallScreens(t *testing.T) {
	s := newScreenScale(1920, 1080)
	if s.screenshotWidth != 1280 || s.screenshotHeight != 720 {
		t.Fatalf("1920x1080 -> %dx%d", s.screenshotWidth, s.screenshotHeight)
	}
	if x, y := s.toScreen(1279, 719); x != 1919 || y != 1079 {
		t.Errorf("far corner -> %d,%d", x, y)
	}
	if x, y := s.toScreenshot(960, 540); x != 640 || y != 360 {
		t.Errorf("center -> %d,%d", x, y)
	}
	if s := newScreenScale(1024, 768); s.screenshotWidth != 1024 || s.screenshotHeight != 768 {
		t.Errorf("a small screen was scaled: %dx%d", s.screenshotWidth, s.screenshotHeight)
	}
	if s := newScreenScale(1600, 1200); s.screenshotWidth != 1067 || s.screenshotHeight != 800 {
		t.Errorf("a 4:3 screen is limited by height: %dx%d", s.screenshotWidth, s.screenshotHeight)
	}
}

func TestShrinkAveragesThePixelsItCovers(t *testing.T) {
	full := image.NewRGBA(image.Rect(0, 0, 4, 2))
	full.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	full.SetRGBA(1, 0, color.RGBA{255, 0, 0, 255})
	small := screenScale{screenWidth: 4, screenHeight: 2, screenshotWidth: 2, screenshotHeight: 1}.shrink(full)
	if got := small.RGBAAt(0, 0); got != (color.RGBA{127, 0, 0, 255}) {
		t.Errorf("left pixel = %v, want the average of two red and two black", got)
	}
}

func TestEveryKeyClaudeNamesIsKnown(t *testing.T) {
	for _, name := range []string{"Return", "Tab", "Escape", "BackSpace", "Delete", "Page_Down", "Up", "ctrl", "alt", "shift", "super", "cmd", "F5", "F12", "space"} {
		if _, ok := namedVirtualKeys[strings.ToLower(name)]; !ok {
			t.Errorf("key %q is not mapped", name)
		}
	}
}

func TestEveryMacKeyClaudeNamesIsKnown(t *testing.T) {
	for _, name := range []string{"Return", "Tab", "Escape", "BackSpace", "Delete", "Page_Down", "Up", "F5", "space"} {
		if _, ok := macKeyCodes[strings.ToLower(name)]; !ok {
			t.Errorf("key %q has no macOS key code", name)
		}
	}
	for _, name := range []string{"cmd", "super", "ctrl", "alt", "shift"} {
		if _, ok := macModifierNamed(name); !ok {
			t.Errorf("modifier %q is not mapped", name)
		}
	}
}
