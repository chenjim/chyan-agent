// Image preparation for model input: validate, cap the pixel count and
// downscale the long edge to MaxImageEdge before base64 encoding (FR-10).
// The original file stays untouched under artifacts for audit.
package core

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the webp decoder for image.Decode
)

const (
	// MaxImageEdge is the long-edge cap sent to the model.
	MaxImageEdge = 2048
	// MaxImagePixels rejects decompression bombs: a 5MB png can claim huge
	// dimensions and force a multi-GB decode.
	MaxImagePixels = 40_000_000
	// jpegQuality is used when re-encoding resized jpeg input.
	jpegQuality = 85
)

// PrepareImage validates an image and returns the bytes to send plus their
// MIME. Images already within MaxImageEdge are returned unchanged; larger
// ones are scaled down (CatmullRom) and re-encoded (jpeg stays jpeg, other
// formats become png).
func PrepareImage(data []byte) ([]byte, string, error) {
	mime := DetectImageMIME(data)
	if mime == "" {
		return nil, "", fmt.Errorf("image: unsupported format (png/jpg/webp/gif)")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("image: decode header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("image: empty dimensions")
	}
	if cfg.Width*cfg.Height > MaxImagePixels {
		return nil, "", fmt.Errorf("image: %dx%d exceeds the %d pixel limit",
			cfg.Width, cfg.Height, MaxImagePixels)
	}
	if cfg.Width <= MaxImageEdge && cfg.Height <= MaxImageEdge {
		return data, mime, nil
	}

	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("image: decode: %w", err)
	}
	width, height := fitDimensions(cfg.Width, cfg.Height, MaxImageEdge)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", fmt.Errorf("image: encode jpeg: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	default:
		if err := png.Encode(&buf, dst); err != nil {
			return nil, "", fmt.Errorf("image: encode png: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
}

// fitDimensions scales w×h so the long edge equals maxEdge, never below 1.
func fitDimensions(w, h, maxEdge int) (int, int) {
	if w >= h {
		nh := h * maxEdge / w
		if nh < 1 {
			nh = 1
		}
		return maxEdge, nh
	}
	nw := w * maxEdge / h
	if nw < 1 {
		nw = 1
	}
	return nw, maxEdge
}
