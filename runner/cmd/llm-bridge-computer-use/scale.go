package main

import (
	"image"
	"image/color"
)

// The model sees a smaller copy of the screen: Anthropic's computer-use
// guidance is that accuracy drops above roughly XGA-to-WXGA sizes, and a
// 4K screenshot costs several times the tokens. The tool keeps the aspect
// ratio, fits the copy inside these bounds, and maps every coordinate the
// model sends back to the screen's own pixels.
const (
	maximumScreenshotWidth  = 1280
	maximumScreenshotHeight = 800
)

// screenScale converts between the screen and the screenshot the model sees.
type screenScale struct {
	screenWidth, screenHeight         int
	screenshotWidth, screenshotHeight int
}

func newScreenScale(screenWidth, screenHeight int) screenScale {
	factor := 1.0
	if screenWidth > maximumScreenshotWidth {
		factor = float64(maximumScreenshotWidth) / float64(screenWidth)
	}
	if f := float64(maximumScreenshotHeight) / float64(screenHeight); screenHeight > maximumScreenshotHeight && f < factor {
		factor = f
	}
	return screenScale{
		screenWidth: screenWidth, screenHeight: screenHeight,
		screenshotWidth:  max(1, int(float64(screenWidth)*factor+0.5)),
		screenshotHeight: max(1, int(float64(screenHeight)*factor+0.5)),
	}
}

// toScreen maps a point on the screenshot to the screen, clamped to it.
func (s screenScale) toScreen(x, y int) (int, int) {
	sx := int(float64(x)*float64(s.screenWidth)/float64(s.screenshotWidth) + 0.5)
	sy := int(float64(y)*float64(s.screenHeight)/float64(s.screenshotHeight) + 0.5)
	return min(max(sx, 0), s.screenWidth-1), min(max(sy, 0), s.screenHeight-1)
}

// toScreenshot maps a point on the screen to the screenshot.
func (s screenScale) toScreenshot(x, y int) (int, int) {
	return int(float64(x)*float64(s.screenshotWidth)/float64(s.screenWidth) + 0.5),
		int(float64(y)*float64(s.screenshotHeight)/float64(s.screenHeight) + 0.5)
}

// inScreenshot reports whether a point lies on the screenshot.
func (s screenScale) inScreenshot(x, y int) bool {
	return x >= 0 && y >= 0 && x < s.screenshotWidth && y < s.screenshotHeight
}

// shrink returns the screenshot-sized copy of a full-size capture, each
// pixel the average of the screen pixels it covers (a box filter, which
// keeps thin text legible better than picking one pixel).
func (s screenScale) shrink(full *image.RGBA) *image.RGBA {
	if s.screenshotWidth == full.Rect.Dx() && s.screenshotHeight == full.Rect.Dy() {
		return full
	}
	out := image.NewRGBA(image.Rect(0, 0, s.screenshotWidth, s.screenshotHeight))
	fw, fh := full.Rect.Dx(), full.Rect.Dy()
	for y := 0; y < s.screenshotHeight; y++ {
		y0 := y * fh / s.screenshotHeight
		y1 := max((y+1)*fh/s.screenshotHeight, y0+1)
		for x := 0; x < s.screenshotWidth; x++ {
			x0 := x * fw / s.screenshotWidth
			x1 := max((x+1)*fw/s.screenshotWidth, x0+1)
			var r, g, b, n int
			for yy := y0; yy < y1; yy++ {
				row := full.Pix[(yy)*full.Stride:]
				for xx := x0; xx < x1; xx++ {
					p := row[xx*4 : xx*4+4]
					r += int(p[0])
					g += int(p[1])
					b += int(p[2])
					n++
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
		}
	}
	return out
}
