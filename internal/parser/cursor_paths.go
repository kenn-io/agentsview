package parser

import (
	"path/filepath"
	"strings"
)

// ParseCursorTranscriptRelPath validates a path relative to a
// Cursor projects dir and returns the encoded project directory
// name for recognized transcript layouts.
func ParseCursorTranscriptRelPath(rel string) (string, bool) {
	rel = filepath.Clean(rel)
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || parts[1] != "agent-transcripts" {
		return "", false
	}

	switch len(parts) {
	case 3:
		if !IsCursorTranscriptExt(parts[2]) {
			return "", false
		}
		return parts[0], true
	case 4:
		if !IsCursorTranscriptExt(parts[3]) {
			return "", false
		}
		stem := strings.TrimSuffix(parts[3], filepath.Ext(parts[3]))
		if stem != parts[2] {
			return "", false
		}
		return parts[0], true
	default:
		return "", false
	}
}
