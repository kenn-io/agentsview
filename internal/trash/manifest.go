package trash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// marshalItem renders one manifest line. Timestamps use RFC 3339 with
// second precision so diffs stay readable.
func marshalItem(item Item) (string, error) {
	payload := struct {
		Item
		DeletedAt string `json:"deletedAt"`
	}{
		Item:      item,
		DeletedAt: item.DeletedAt.UTC().Format(time.RFC3339),
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(line), nil
}

// parseManifest decodes JSONL content, skipping malformed lines.
func parseManifest(data []byte) []Item {
	var items []Item
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var payload struct {
			Item
			DeletedAt string `json:"deletedAt"`
		}
		if err := json.Unmarshal(line, &payload); err != nil {
			continue
		}
		item := payload.Item
		if payload.DeletedAt != "" {
			if ts, err := time.Parse(time.RFC3339, payload.DeletedAt); err == nil {
				item.DeletedAt = ts
			}
		}
		if item.ID == "" || item.OriginalPath == "" {
			continue
		}
		items = append(items, item)
	}
	return items
}

// trashinfoPath percent-encodes an absolute path for a freedesktop
// trashinfo file.
func trashinfoPath(abs string) string {
	return url.PathEscape(filepath.Clean(abs))
}

// parseTrashInfoPath is exercised by tests to keep the round trip
// honest; production restore relies on the agentsview manifest.
func parseTrashInfoPath(encoded string) (string, error) {
	decoded, err := url.PathUnescape(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(decoded) {
		return "", fmt.Errorf("trashinfo path %q is not absolute", decoded)
	}
	return decoded, nil
}
