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
