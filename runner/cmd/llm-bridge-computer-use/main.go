// llm-bridge-computer-use is an MCP server, spoken over stdio, with one
// tool, "computer": it takes screenshots of the screen and moves the mouse
// and keyboard. A runner offers it to a Claude Code session on a machine
// whose owner switched computer use on.
//
// The tool's actions and their arguments follow Anthropic's computer-use
// tool (screenshot, left_click, type, key, scroll, left_click_drag, …),
// because that is the shape the model was trained on. Coordinates are in
// the screenshot's pixels; the screenshot is the screen scaled to fit
// 1280×800, and the tool maps every coordinate back to the screen.
//
// On macOS it drives the main display through screencapture and osascript
// (desktop_darwin.go); macOS asks once for Screen Recording and
// Accessibility. On Windows it drives the interactive desktop of the user it runs as. Run
// from WSL it is still a Windows program: Claude Code inside WSL starts the
// .exe through WSL's Windows interop, and it runs in the Windows session WSL
// was started from. It cannot act on a locked screen or on the secure
// desktop Windows shows for administrator prompts.
package main

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is set at build time (-ldflags "-X main.Version=…").
var Version = "devel"

// settleBeforeScreenshot is how long the tool waits after acting before it
// takes the screenshot it returns, so menus and pages have drawn.
const settleBeforeScreenshot = 400 * time.Millisecond

func main() {
	log.SetOutput(os.Stderr) // stdout carries the MCP protocol
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(Version)
		return
	}
	d, err := newDesktop()
	if err != nil {
		log.Fatalf("llm-bridge-computer-use: %v", err)
	}
	server := newComputerServer(d)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("llm-bridge-computer-use: %v", err)
	}
}

type computerInput struct {
	Action          string  `json:"action" jsonschema:"one of: screenshot, cursor_position, mouse_move, left_click, right_click, middle_click, double_click, triple_click, left_click_drag, left_mouse_down, left_mouse_up, scroll, type, key, hold_key, wait"`
	Coordinate      []int   `json:"coordinate,omitempty" jsonschema:"[x, y] on the screenshot: the target of mouse_move, clicks, left_click_drag (end) and scroll"`
	StartCoordinate []int   `json:"start_coordinate,omitempty" jsonschema:"[x, y] on the screenshot where left_click_drag starts"`
	Text            string  `json:"text,omitempty" jsonschema:"for type: the text; for key and hold_key: keys in xdotool style, e.g. ctrl+s or alt+Tab, several separated by spaces; for clicks and scroll: modifier keys to hold, e.g. shift"`
	ScrollDirection string  `json:"scroll_direction,omitempty" jsonschema:"up, down, left or right"`
	ScrollAmount    int     `json:"scroll_amount,omitempty" jsonschema:"notches of the wheel to scroll"`
	Duration        float64 `json:"duration,omitempty" jsonschema:"seconds, for wait and hold_key"`
}

func newComputerServer(d desktop) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "llm-bridge-computer-use", Version: Version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "computer",
		Description: "Use this computer's screen, mouse and keyboard. Take a screenshot first to see the screen; " +
			"coordinates are pixels on that screenshot. Every action except screenshot and cursor_position " +
			"answers with a new screenshot taken just after it.",
	}, func(ctx context.Context, request *mcp.CallToolRequest, input computerInput) (*mcp.CallToolResult, any, error) {
		return runComputerAction(ctx, d, input)
	})
	return server
}

func runComputerAction(ctx context.Context, d desktop, input computerInput) (*mcp.CallToolResult, any, error) {
	width, height, err := d.screenSize()
	if err != nil {
		return nil, nil, fmt.Errorf("read the screen size: %w", err)
	}
	scale := newScreenScale(width, height)

	point := func(name string, coordinate []int) (int, int, error) {
		if len(coordinate) != 2 {
			return 0, 0, fmt.Errorf("%s must be [x, y], got %v", name, coordinate)
		}
		if !scale.inScreenshot(coordinate[0], coordinate[1]) {
			return 0, 0, fmt.Errorf("%s %v is off the %dx%d screenshot", name, coordinate, scale.screenshotWidth, scale.screenshotHeight)
		}
		x, y := scale.toScreen(coordinate[0], coordinate[1])
		return x, y, nil
	}
	moveTo := func(name string, coordinate []int, required bool) error {
		if coordinate == nil && !required {
			return nil
		}
		x, y, err := point(name, coordinate)
		if err != nil {
			return err
		}
		return d.moveMouse(x, y)
	}
	withModifiers := func(act func() error) error {
		modifiers := strings.Fields(strings.ReplaceAll(input.Text, "+", " "))
		if len(modifiers) == 0 {
			return act()
		}
		if err := d.holdKeys(modifiers); err != nil {
			return err
		}
		actErr := act()
		if err := d.releaseKeys(modifiers); err != nil && actErr == nil {
			actErr = err
		}
		return actErr
	}
	click := func(button mouseButton, count int) error {
		if err := moveTo("coordinate", input.Coordinate, false); err != nil {
			return err
		}
		return withModifiers(func() error {
			for i := 0; i < count; i++ {
				if err := d.pressMouseButton(button); err != nil {
					return err
				}
				if err := d.releaseMouseButton(button); err != nil {
					return err
				}
			}
			return nil
		})
	}
	requireText := func() error {
		if input.Text == "" {
			return fmt.Errorf("%s needs text", input.Action)
		}
		return nil
	}

	var actErr error
	switch input.Action {
	case "screenshot":
		return screenshotResult(d, scale, "")
	case "cursor_position":
		x, y, err := d.cursorPosition()
		if err != nil {
			return nil, nil, err
		}
		sx, sy := scale.toScreenshot(x, y)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("X=%d,Y=%d", sx, sy)}}}, nil, nil
	case "mouse_move":
		actErr = moveTo("coordinate", input.Coordinate, true)
	case "left_click":
		actErr = click(leftButton, 1)
	case "right_click":
		actErr = click(rightButton, 1)
	case "middle_click":
		actErr = click(middleButton, 1)
	case "double_click":
		actErr = click(leftButton, 2)
	case "triple_click":
		actErr = click(leftButton, 3)
	case "left_click_drag":
		if actErr = moveTo("start_coordinate", input.StartCoordinate, true); actErr != nil {
			break
		}
		if _, _, actErr = point("coordinate", input.Coordinate); actErr != nil {
			break
		}
		if actErr = d.pressMouseButton(leftButton); actErr != nil {
			break
		}
		actErr = moveTo("coordinate", input.Coordinate, true)
		if err := d.releaseMouseButton(leftButton); err != nil && actErr == nil {
			actErr = err
		}
	case "left_mouse_down":
		actErr = d.pressMouseButton(leftButton)
	case "left_mouse_up":
		actErr = d.releaseMouseButton(leftButton)
	case "scroll":
		if actErr = moveTo("coordinate", input.Coordinate, false); actErr != nil {
			break
		}
		amount := input.ScrollAmount
		if amount <= 0 {
			actErr = fmt.Errorf("scroll needs a scroll_amount above 0")
			break
		}
		vertical, horizontal := 0, 0
		switch input.ScrollDirection {
		case "down":
			vertical = amount
		case "up":
			vertical = -amount
		case "right":
			horizontal = amount
		case "left":
			horizontal = -amount
		default:
			actErr = fmt.Errorf("scroll_direction must be up, down, left or right, got %q", input.ScrollDirection)
		}
		if actErr == nil {
			actErr = withModifiers(func() error { return d.scroll(vertical, horizontal) })
		}
	case "type":
		if actErr = requireText(); actErr == nil {
			actErr = d.typeText(input.Text)
		}
	case "key":
		if actErr = requireText(); actErr == nil {
			for _, combination := range strings.Fields(input.Text) {
				if actErr = d.pressKeys(strings.Split(combination, "+")); actErr != nil {
					break
				}
			}
		}
	case "hold_key":
		if actErr = requireText(); actErr != nil {
			break
		}
		keys := strings.Split(input.Text, "+")
		if actErr = d.holdKeys(keys); actErr != nil {
			break
		}
		sleep(ctx, input.Duration)
		actErr = d.releaseKeys(keys)
	case "wait":
		sleep(ctx, input.Duration)
	default:
		return nil, nil, fmt.Errorf("unknown action %q", input.Action)
	}
	if actErr != nil {
		return nil, nil, fmt.Errorf("%s: %w", input.Action, actErr)
	}
	time.Sleep(settleBeforeScreenshot)
	return screenshotResult(d, scale, input.Action+" done")
}

func sleep(ctx context.Context, seconds float64) {
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(seconds * float64(time.Second))):
	}
}

func screenshotResult(d desktop, scale screenScale, note string) (*mcp.CallToolResult, any, error) {
	full, err := d.screenshot()
	if err != nil {
		return nil, nil, fmt.Errorf("take a screenshot: %w", err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, scale.shrink(full)); err != nil {
		return nil, nil, fmt.Errorf("encode the screenshot: %w", err)
	}
	content := []mcp.Content{&mcp.ImageContent{Data: encoded.Bytes(), MIMEType: "image/png"}}
	if note != "" {
		content = append([]mcp.Content{&mcp.TextContent{Text: note}}, content...)
	}
	return &mcp.CallToolResult{Content: content}, nil, nil
}
