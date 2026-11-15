// images.go — --image loading for one-shot runs and the REPL.
package main

import (
	"encoding/base64"
	"fmt"
	"os"

	"chyan.tech/chyan/internal/core"
)

// loadImages reads image files for one turn and encodes them as data URLs
// (the daemon API shape). Limits are shared with the app (4 images, 5MB each,
// png/jpg/webp/gif) so the CLI fails before any run.
func loadImages(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > core.MaxImagesPerTurn {
		return nil, fmt.Errorf("最多 %d 张图片，收到 %d 张", core.MaxImagesPerTurn, len(paths))
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("图片 %s: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("图片 %s 是目录", path)
		}
		if info.Size() > core.MaxImageBytes {
			return nil, fmt.Errorf("图片 %s 为 %d 字节，超过 %d 字节上限", path, info.Size(), core.MaxImageBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取图片 %s: %w", path, err)
		}
		mime := core.DetectImageMIME(data)
		if mime == "" {
			return nil, fmt.Errorf("图片 %s 不是支持的格式（png/jpg/webp/gif）", path)
		}
		out = append(out, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return out, nil
}
