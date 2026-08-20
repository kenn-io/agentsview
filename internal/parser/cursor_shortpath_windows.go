//go:build windows

package parser

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type cursorPathAlias struct {
	tokens []string
	path   string
}

// cursorComponentTokenAliases returns the token spellings one directory entry
// can match under. The 8.3 short-path alias costs a GetShortPathName syscall
// per entry, so callers enable it only when the encoded directory name
// contains a '~': Windows always mangles a shortened component with a '~N'
// tail, and a name already valid as 8.3 matches through its long form.
func cursorComponentTokenAliases(
	path string, includeShort bool,
) []cursorPathAlias {
	longName := filepath.Base(path)
	aliases := []cursorPathAlias{{
		tokens: cursorComponentTokens(longName),
		path:   path,
	}}
	if !includeShort {
		return aliases
	}
	shortPath, ok := cursorShortPath(path)
	if !ok || strings.EqualFold(filepath.Clean(shortPath), filepath.Clean(path)) {
		return aliases
	}
	return append(aliases, cursorPathAlias{
		tokens: cursorComponentTokens(filepath.Base(shortPath)),
		path:   shortPath,
	})
}

func cursorShortPath(path string) (string, bool) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	for size := uint32(260); size <= 32768; {
		buffer := make([]uint16, size)
		length, err := windows.GetShortPathName(pathPtr, &buffer[0], size)
		if err != nil || length == 0 {
			return "", false
		}
		if length < size {
			return filepath.Clean(windows.UTF16ToString(buffer[:length])), true
		}
		size = length + 1
	}
	return "", false
}
