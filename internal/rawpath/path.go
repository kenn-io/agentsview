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

// PlatformKey maps a validated logical path to its cross-platform
// filesystem-equivalence key: two paths with the same key denote the same
// location wherever a manifest may materialize. Windows (NTFS, ReFS) and
// macOS (default case-insensitive APFS volumes) compare file names
// case-insensitively using simple Unicode case mapping, so the key lowercases
// every rune. The mapping deliberately stops at case: those filesystems do
// not normalize Unicode, so NFC and NFD spellings stay distinct keys, and
// NTFS short-name aliases depend on per-volume state that cannot be
// predicted here. The result depends only on the input, never on locale,
// time, or the host running the check.
func PlatformKey(value string) string {
	return strings.ToLower(value)
}

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
