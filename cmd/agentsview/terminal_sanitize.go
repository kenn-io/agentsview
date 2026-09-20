// ABOUTME: terminal-output hardening for session CLI commands.
// ABOUTME: Strips C0/C1 control bytes before printing so session
// ABOUTME: text cannot spoof terminal state via escape sequences.
package main

import (
	"strings"
	"unicode/utf8"
)

// sanitizeTerminal strips C0/C1 control bytes (including ESC and
// CR) from s so that session-derived text — message content,
// display names, project names, tool names, etc. — cannot drive
// terminal escape sequences when printed in --format human mode.
// Preserves only \n and \t so line breaks and tabs still work;
// carriage return is dropped because bare \r returns the cursor
// to column 0 and lets "safe\rEVIL" overwrite earlier output
// without any ANSI involved. CRLF input still renders correctly
// because terminals treat lone \n as a newline.
//
// Rationale: even though agentsview is a single-user tool and
// session files are generally trusted, content flows in from
// imported transcripts and remote machines via PG sync. Without
// this filter a malicious session could emit OSC 8 hyperlinks
// (phishing), OSC 52 clipboard writes, title-set sequences, or
// cursor-movement that overwrites prior output. JSON output is
// left untouched because consumers there handle their own escaping.
func sanitizeTerminal(s string) string {
	if !hasControlBytes(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		// Invalid UTF-8 byte: drop one byte and retry. This
		// prevents raw 0x80-0xBF bytes from surfacing as U+FFFD
		// and keeps the output valid UTF-8.
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// C0, DEL, C1 controls: dropped.
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// hasControlBytes is a fast-path check that avoids building a new
// string when s is already clean. It only looks at raw bytes — the
// UTF-8 range pass in sanitizeTerminal handles rune boundaries.
// Keep the preserved set here in sync with sanitizeTerminal.
func hasControlBytes(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '\n' || c == '\t':
			continue
		case c < 0x20, c == 0x7f, c >= 0x80 && c <= 0x9f:
			return true
		}
	}
	return false
}
