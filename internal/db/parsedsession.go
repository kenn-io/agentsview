package db

import "go.kenn.io/agentsview/internal/parser"

// ParsedSessionName returns the session_name value to store when upserting
// a parsed session. Returns nil when the parser did not extract a name.
func ParsedSessionName(sess parser.ParsedSession) *string {
	if sess.SessionName == "" {
		return nil
	}
	n := sess.SessionName
	return &n
}

// ParsedSessionNameFields is a compatibility shim retained while callers
// are migrated to use ParsedSessionName. It will be removed in the
// session_name two-field refactor (Task 5).
func ParsedSessionNameFields(sess parser.ParsedSession) (displayName *string, nameSource *string) {
	n := ParsedSessionName(sess)
	if n == nil {
		return nil, nil
	}
	src := "agent"
	return n, &src
}
