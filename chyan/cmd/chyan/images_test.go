// images_test.go — --image loading and limits.
package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

func TestLoadImages(t *testing.T) {
	dir := t.TempDir()
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "a.png")
	if err := os.WriteFile(img, png, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadImages([]string{img})
	if err != nil || len(loaded) != 1 || !strings.HasPrefix(loaded[0], "data:image/png;base64,") {
		t.Fatalf("loadImages = %+v, %v", loaded, err)
	}

	text := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(text, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadImages([]string{text}); err == nil || !strings.Contains(err.Error(), "格式") {
		t.Fatalf("text image error = %v", err)
	}
	if _, err := loadImages([]string{filepath.Join(dir, "missing.png")}); err == nil {
		t.Fatal("missing file must fail")
	}
	tooMany := make([]string, core.MaxImagesPerTurn+1)
	for i := range tooMany {
		tooMany[i] = img
	}
	if _, err := loadImages(tooMany); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("too many error = %v", err)
	}
}
