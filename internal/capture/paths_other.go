//go:build !darwin

package capture

import (
	"fmt"
	"path/filepath"
	"strings"
)

func pathWithinVolume(parent, candidate string) (bool, error) {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false, fmt.Errorf("comparing paths: %w", err)
	}
	return relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}
