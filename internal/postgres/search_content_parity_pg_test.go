//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// classifySearchErr buckets a SearchContent failure into the two error
// classes the HTTP layer maps to distinct status codes: "input" is a
// *db.SearchInputError (400) and "unavailable" is db.ErrSemanticUnavailable
// (501). An input error must never also satisfy errors.Is ErrSemanticUnavailable.
func classifySearchErr(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	var inputErr *db.SearchInputError
	if errors.As(err, &inputErr) {
		assert.False(t, errors.Is(err, db.ErrSemanticUnavailable),
			"input error must not also be ErrSemanticUnavailable: %v", err)
		return "input"
	}
	if errors.Is(err, db.ErrSemanticUnavailable) {
		return "unavailable"
	}
	require.Failf(t, "unexpected error class", "%T: %v", err, err)
	return ""
}

// TestSearchContentSemanticErrorParity runs the same semantic and hybrid
// error-classification table through SQLite and PostgreSQL. The PostgreSQL
// calls use setupContentSearch, so this parity proof executes against the
// actual backend whenever TEST_PG_URL is available.
func TestSearchContentSemanticErrorParity(t *testing.T) {
	sqlite, err := db.Open(filepath.Join(t.TempDir(), "parity.db"))
	require.NoError(t, err, "open sqlite")
	t.Cleanup(func() { sqlite.Close() })

	pg := setupContentSearch(t)

	cases := []struct {
		name   string
		filter db.ContentSearchFilter
		want   string
	}{
		{name: "semantic cursor rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "semantic", Cursor: 1}, want: "input"},
		{name: "hybrid cursor rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "hybrid", Cursor: 1}, want: "input"},
		{name: "semantic non-messages source rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "semantic", Sources: []string{"tool_input"}}, want: "input"},
		{name: "hybrid non-messages source rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "hybrid", Sources: []string{"tool_result"}}, want: "input"},
		{name: "semantic bad scope rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "semantic", Scope: "bogus"}, want: "input"},
		{name: "hybrid bad scope rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "hybrid", Scope: "bogus"}, want: "input"},
		{name: "unknown mode rejected", filter: db.ContentSearchFilter{Pattern: "x", Mode: "bogus"}, want: "input"},
		{name: "valid semantic hits capability gate", filter: db.ContentSearchFilter{Pattern: "x", Mode: "semantic"}, want: "unavailable"},
		{name: "valid hybrid hits capability gate", filter: db.ContentSearchFilter{Pattern: "x", Mode: "hybrid"}, want: "unavailable"},
		{name: "valid semantic messages-only hits capability gate", filter: db.ContentSearchFilter{Pattern: "x", Mode: "semantic", Sources: []string{"messages"}}, want: "unavailable"},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sqliteErr := sqlite.SearchContent(ctx, tc.filter)
			_, pgErr := pg.SearchContent(ctx, tc.filter)
			assert.Equal(t, tc.want, classifySearchErr(t, sqliteErr), "sqlite class")
			assert.Equal(t, tc.want, classifySearchErr(t, pgErr), "postgres class")
		})
	}
}
