//go:build darwin

package capture

import (
	"path/filepath"
	"strings"
)

func pathWithinVolume(parent, candidate string) (bool, error) {
	parent = strings.ToLower(filepath.Clean(parent))
	candidate = strings.ToLower(filepath.Clean(candidate))
	return candidate == parent || strings.HasPrefix(
		candidate, parent+string(filepath.Separator),
	), nil
}
