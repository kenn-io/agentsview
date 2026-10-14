package readbase

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

func TestMessageWindowSQL(t *testing.T) {
	anchor, from := 7, 3
	windows := []struct {
		name   string
		window db.MessageWindow
		args   []any
		suffix string
	}{
		{"linear_ascending", db.MessageWindow{Asc: true, Limit: 5}, []any{"session", 0, 5}, "ordinal >= ? ORDER BY ordinal ASC LIMIT ?"},
		{"linear_descending", db.MessageWindow{From: &from, Limit: 5}, []any{"session", 3, 5}, "ordinal <= ? ORDER BY ordinal DESC LIMIT ?"},
		{"roles", db.MessageWindow{Asc: true, Limit: 5, Roles: []string{"user", "assistant"}}, []any{"session", 0, "user", "assistant", 5}, "ordinal >= ? AND role IN (?,?) ORDER BY ordinal ASC LIMIT ?"},
		{"around", db.MessageWindow{Around: &anchor, Before: 2, After: 4}, []any{"session", 7, 2, "session", 7, "session", 7, 4}, "ORDER BY w.ordinal"},
		{"around_roles", db.MessageWindow{Around: &anchor, Before: 2, After: 4, Roles: []string{"user", "assistant"}}, []any{"session", 7, "user", "assistant", 2, "session", 7, "session", 7, "user", "assistant", 4}, "ORDER BY w.ordinal"},
		{"around_negative_limits", db.MessageWindow{Around: &anchor, Before: -2, After: -4}, []any{"session", 7, 0, "session", 7, "session", 7, 0}, "ORDER BY w.ordinal"},
	}
	revisions := []struct {
		name, sql string
		args      []any
	}{
		{"none", "", nil},
		{"literal", "'revision'", nil},
		{"empty_literal", "''", nil},
		{"bound", "(SELECT revision FROM sessions WHERE id = ?)", []any{"revision-session"}},
	}
	for _, window := range windows {
		for _, revision := range revisions {
			t.Run(window.name+"/"+revision.name, func(t *testing.T) {
				query, args, hasRevision := MessageWindowSQL("ordinal, role", revision.sql, revision.args, "session", window.window)
				expectedArgs := append(append([]any(nil), revision.args...), window.args...)
				assert.Equal(t, expectedArgs, args)
				assert.Equal(t, revision.sql != "", hasRevision)
				assert.Equal(t, len(args), strings.Count(query, "?"))
				projection := "ordinal, role"
				if window.window.Around != nil {
					projection = "w.*"
				}
				if revision.sql != "" {
					projection = revision.sql + ", " + projection
				}
				assert.True(t, strings.HasPrefix(query, "SELECT "+projection+" FROM "), query)
				assert.True(t, strings.HasSuffix(query, window.suffix), query)
				if window.window.Around != nil {
					assert.Equal(t, 3, strings.Count(query, "SELECT ordinal, role FROM messages"))
					assert.Contains(t, query, "ordinal < ?")
					assert.Contains(t, query, "ordinal = ?\n")
					assert.Contains(t, query, "ordinal > ?")
					expectedRoleClauses := 0
					if len(window.window.Roles) > 0 {
						expectedRoleClauses = 2
					}
					assert.Equal(t, expectedRoleClauses, strings.Count(query, "AND role IN"))
				}
			})
		}
	}
}
