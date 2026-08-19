// ABOUTME: terminal-output hardening for session CLI commands.
// ABOUTME: Strips C0/C1 control bytes before printing so session
// ABOUTME: text cannot spoof terminal state via escape sequences.
package main

import "go.kenn.io/agentsview/internal/terminaltext"

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
	return terminaltext.Sanitize(s)
}
