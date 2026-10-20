package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPreviewKeepsTheShapeAndFitsTheLongestSide(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 2000; x++ {
			source.Set(x, y, color.RGBA{R: 200, G: 40, B: 30, A: 255})
		}
	}
	path := filepath.Join(t.TempDir(), "wide.png")
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	preview, err := previewOfImage(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(preview))
	if err != nil {
		t.Fatalf("the preview is not a JPEG: %v", err)
	}
	if size := decoded.Bounds().Size(); size.X != 1024 || size.Y != 512 {
		t.Errorf("preview is %v, want 1024x512", size)
	}
	red, green, _, _ := decoded.At(500, 250).RGBA()
	if red>>8 < 180 || green>>8 > 70 {
		t.Errorf("the colour changed: red %d green %d", red>>8, green>>8)
	}
}

func TestAPreviewOfASmallImageIsNotEnlarged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.png")
	var encoded bytes.Buffer
	png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 40, 30)))
	os.WriteFile(path, encoded.Bytes(), 0o644)
	preview, err := previewOfImage(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := jpeg.Decode(bytes.NewReader(preview))
	if size := decoded.Bounds().Size(); size.X != 40 || size.Y != 30 {
		t.Errorf("preview is %v, want 40x30", size)
	}
}

func TestAProcessTheServerDidNotStartIsToldWhy(t *testing.T) {
	t.Setenv(postingTokenEnvironmentVariable, "")
	t.Setenv(imagesURLEnvironmentVariable, "http://127.0.0.1:8160/sessions/br_1/images")
	if _, _, err := sessionAddress(imagesURLEnvironmentVariable); err == nil || !strings.Contains(err.Error(), "not started by llm-bridge-server") {
		t.Errorf("err = %v", err)
	}
	t.Setenv(postingTokenEnvironmentVariable, "token")
	t.Setenv(imagesURLEnvironmentVariable, "")
	if _, _, err := sessionAddress(imagesURLEnvironmentVariable); err == nil || !strings.Contains(err.Error(), imagesURLEnvironmentVariable) {
		t.Errorf("err = %v", err)
	}
}
