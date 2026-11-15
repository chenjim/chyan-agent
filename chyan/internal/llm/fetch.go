package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// httpClient is shared by the BPE table downloader and the online model
// catalog; timeouts are fixed because both callers are background lookups.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// fetchBytes GETs url and reads the full body into memory. Callers only use
// this for small reference documents (BPE tables, catalog JSON).
func fetchBytes(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// fetchJSON GETs url and decodes the body into out.
func fetchJSON(url string, out any) error {
	data, err := fetchBytes(url)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// parseBpeBytes parses the .tiktoken format: one "base64token rank" per line.
func parseBpeBytes(data []byte) (map[string]int, error) {
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed bpe line: %q", line)
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

// parseBpeFile reads and parses a cached .tiktoken file.
func parseBpeFile(path string) (map[string]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseBpeBytes(data)
}
