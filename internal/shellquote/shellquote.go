// Package shellquote quotes values for commands that users paste into a
// shell.
package shellquote

import "strings"

// posixSafe reports whether c can appear unquoted in a POSIX shell word.
func posixSafe(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.ContainsRune("@%+=:,./-_~", c)
}

// POSIX returns s as a single POSIX shell word, quoting only when needed
// so the common case stays readable.
func POSIX(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool { return !posixSafe(c) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PowerShell returns s as a PowerShell single-quoted string, in which
// only a doubled quote is special.
func PowerShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
