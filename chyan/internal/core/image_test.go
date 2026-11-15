// Image artifact tests: validation, persistence permissions and MIME sniffing.
package core

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// tinyPNG is a valid 1x1 PNG.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSaveImage(t *testing.T) {
	dir := t.TempDir()
	png := tinyPNG(t)
	part, err := SaveImage(dir, "ses1", "", png)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if part.Type != "image" || part.Image == nil {
		t.Fatalf("part = %+v", part)
	}
	if part.Image.MIME != "image/png" || part.Image.SHA256 == "" || part.Image.Size != len(png) {
		t.Fatalf("image meta = %+v", part.Image)
	}
	if !bytes.Equal(part.Image.Data, png) {
		t.Fatal("raw bytes not carried on the part")
	}
	saved, err := os.ReadFile(part.Image.Path)
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if !bytes.Equal(saved, png) {
		t.Fatal("artifact bytes differ")
	}
	if info, err := os.Stat(part.Image.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %v, %v", info.Mode().Perm(), err)
	}
	if filepath.Base(filepath.Dir(part.Image.Path)) != "image" {
		t.Fatalf("artifact path = %s", part.Image.Path)
	}
}

func TestSaveImageRejects(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveImage("", "s", "", tinyPNG(t)); err == nil {
		t.Fatal("empty dir must fail")
	}
	if _, err := SaveImage(dir, "s", "", []byte("not an image")); err == nil {
		t.Fatal("non-image bytes must fail")
	}
	if _, err := SaveImage(dir, "s", "", nil); err == nil {
		t.Fatal("empty data must fail")
	}
	big := make([]byte, MaxImageBytes+1)
	copy(big, tinyPNG(t))
	if _, err := SaveImage(dir, "s", "", big); err == nil {
		t.Fatal("oversized image must fail")
	}
}

func TestDetectImageMIME(t *testing.T) {
	if got := DetectImageMIME(tinyPNG(t)); got != "image/png" {
		t.Errorf("png = %q", got)
	}
	if got := DetectImageMIME([]byte("plain text")); got != "" {
		t.Errorf("text = %q", got)
	}
}

// makeImage builds a width×height png.
func makeImage(t *testing.T, width, height int, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		img.Set(x, 0, color.RGBA{R: uint8(x % 255), A: 255})
	}
	var buf bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&buf, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareImageDownscales(t *testing.T) {
	src := makeImage(t, 3000, 100, "png")
	out, mime, err := PrepareImage(src)
	if err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode resized: %v", err)
	}
	if cfg.Width != MaxImageEdge || cfg.Height != 68 {
		t.Fatalf("resized = %dx%d, want 2048x68", cfg.Width, cfg.Height)
	}

	// Small images are passed through untouched.
	small := makeImage(t, 64, 32, "png")
	out, _, err = PrepareImage(small)
	if err != nil || !bytes.Equal(out, small) {
		t.Fatalf("small image changed: err=%v same=%v", err, bytes.Equal(out, small))
	}
}

func TestPrepareImageJpegStaysJpeg(t *testing.T) {
	src := makeImage(t, 100, 3000, "jpeg")
	out, mime, err := PrepareImage(src)
	if err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Height != MaxImageEdge {
		t.Fatalf("resized = %+v, %v", cfg, err)
	}
}

func TestPrepareImageRejectsNonImage(t *testing.T) {
	if _, _, err := PrepareImage([]byte("plain text")); err == nil {
		t.Fatal("non-image must fail")
	}
}

func TestSaveImageKeepsOriginalAndSendsResized(t *testing.T) {
	dir := t.TempDir()
	src := makeImage(t, 3000, 100, "png")
	part, err := SaveImage(dir, "s", "", src)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	onDisk, err := os.ReadFile(part.Image.Path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(onDisk))
	if err != nil || cfg.Width != 3000 {
		t.Fatalf("artifact = %+v, %v (original must stay original)", cfg, err)
	}
	sent, _, err := image.DecodeConfig(bytes.NewReader(part.Image.Data))
	if err != nil || sent.Width != MaxImageEdge {
		t.Fatalf("sent = %+v, %v (must be resized)", sent, err)
	}
}
