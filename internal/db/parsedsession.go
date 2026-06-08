package db

import "go.kenn.io/agentsview/internal/parser"

// ParsedSessionNameFields returns the display_name and name_source values
// to store when upserting a parsed session. Both are nil when the parser
// did not extract a name. This is the single place that couples the two
// fields — callers must not set them independently.
func ParsedSessionNameFields(sess parser.ParsedSession) (displayName *string, nameSource *string) {
	if sess.DisplayName == "" {
		return nil, nil
	}
	name := sess.DisplayName
	src := "agent"
	return &name, &src
}
