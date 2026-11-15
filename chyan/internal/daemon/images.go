// images.go — decode data-URL images from task requests (FR-10).
package daemon

import (
	"encoding/base64"
	"fmt"
	"strings"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
)

// maxTaskBodyBytes bounds a task request: 4 images × 5MB raw, base64 overhead
// and prompt text. The global 1MB JSON cap is too small for uploads.
const maxTaskBodyBytes = 32 << 20

// imagesFromRequest decodes data URLs into image inputs, enforcing the same
// count/size/format limits as the CLI.
func imagesFromRequest(dataURLs []string) ([]app.ImageInput, error) {
	if len(dataURLs) == 0 {
		return nil, nil
	}
	if len(dataURLs) > core.MaxImagesPerTurn {
		return nil, fmt.Errorf("at most %d images per task, got %d", core.MaxImagesPerTurn, len(dataURLs))
	}
	out := make([]app.ImageInput, 0, len(dataURLs))
	for i, s := range dataURLs {
		if !strings.HasPrefix(s, "data:") {
			return nil, fmt.Errorf("image %d: expected a data URL", i+1)
		}
		comma := strings.IndexByte(s, ',')
		if comma < 0 {
			return nil, fmt.Errorf("image %d: malformed data URL", i+1)
		}
		meta := s[5:comma]
		if !strings.HasSuffix(meta, ";base64") {
			return nil, fmt.Errorf("image %d: only base64 data URLs are supported", i+1)
		}
		mime := strings.TrimSuffix(meta, ";base64")
		raw, err := base64.StdEncoding.DecodeString(s[comma+1:])
		if err != nil {
			return nil, fmt.Errorf("image %d: decode base64: %w", i+1, err)
		}
		if len(raw) > core.MaxImageBytes {
			return nil, fmt.Errorf("image %d: %d bytes over the %d byte limit", i+1, len(raw), core.MaxImageBytes)
		}
		if detected := core.DetectImageMIME(raw); detected == "" {
			return nil, fmt.Errorf("image %d: unsupported format (png/jpg/webp/gif)", i+1)
		} else if mime == "" {
			mime = detected
		}
		out = append(out, app.ImageInput{MIME: mime, Data: raw})
	}
	return out, nil
}
