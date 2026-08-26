// Package rawpath validates logical paths transported by hosted raw sync.
package rawpath

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DefaultMaxBytes = 4096

var ErrInvalid = errors.New("invalid raw logical path")

// Validate enforces the cross-platform relative-path contract used by capture
// plans and raw manifests.
func Validate(value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		path.IsAbs(value) || path.Clean(value) != value || value == "." ||
		value == ".." || strings.HasPrefix(value, "../") ||
		strings.ContainsRune(value, '\\') || platformUnsafe(value) {
		return fmt.Errorf("%w: path is not canonical and relative", ErrInvalid)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: path contains a control character", ErrInvalid)
		}
	}
	return nil
}

func platformUnsafe(value string) bool {
	if strings.ContainsRune(value, ':') {
		return true
	}
	for component := range strings.SplitSeq(value, "/") {
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return true
		}
		base, _, _ := strings.Cut(component, ".")
		upper := strings.ToUpper(base)
		switch upper {
		case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
			return true
		}
		if len(upper) == 4 && upper[3] >= '1' && upper[3] <= '9' &&
			(upper[:3] == "COM" || upper[:3] == "LPT") {
			return true
		}
	}
	return false
}
