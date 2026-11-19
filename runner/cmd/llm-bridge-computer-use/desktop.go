package main

import (
	"errors"
	"image"
)

// desktop is what the computer tool drives: one screen, its mouse and its
// keyboard. Coordinates are the screen's own pixels; scaling to and from
// the screenshot the model sees happens in the tool, not here.
type desktop interface {
	// screenSize is the size of the screen in its own pixels.
	screenSize() (width, height int, err error)
	screenshot() (*image.RGBA, error)
	moveMouse(x, y int) error
	cursorPosition() (x, y int, err error)
	// pressMouseButton and releaseMouseButton act at the cursor.
	pressMouseButton(button mouseButton) error
	releaseMouseButton(button mouseButton) error
	// scroll turns the wheel at the cursor: positive down/right, in notches.
	scroll(vertical, horizontal int) error
	typeText(text string) error
	// pressKeys holds the keys down in order and lets them go in reverse:
	// "ctrl", "shift", "t" is ctrl+shift+t.
	pressKeys(keys []string) error
	// holdKeys holds the keys down; releaseKeys lets them go.
	holdKeys(keys []string) error
	releaseKeys(keys []string) error
}

type mouseButton int

const (
	leftButton mouseButton = iota
	rightButton
	middleButton
)

var errDesktopUnsupported = errors.New("computer use is not built for this operating system yet")
