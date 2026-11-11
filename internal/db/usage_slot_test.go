package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestUsageSlot(t *testing.T) {
	base := db.UsageSlot(db.UsageFilter{}, true)
	require.NotEmpty(t, base)
	require.Equal(t, base, db.UsageSlot(db.UsageFilter{ModelAliases: map[string]string{"m": "g"}, SkipSessionCounts: true, PairwiseSides: true, TopSessionsSort: "tokens", TopSessionsByGroup: true, TopSessionsTokenTypes: db.UsageTokenTypeOutput}, true))
	require.NotEqual(t, base, db.UsageSlot(db.UsageFilter{ProjectLabels: []string{}}, true))
	require.NotEqual(t, base, db.UsageSlot(db.UsageFilter{Breakdowns: true}, true))
	require.Equal(t, base, db.UsageSlot(db.UsageFilter{Breakdowns: true}, false))
	require.NotEqual(t, base, db.UsageSlot(db.UsageFilter{From: "2026-01-01"}, true))
	for _, termination := range []string{"active", "stale", "unclean", "\xff"} {
		require.Empty(t, db.UsageSlot(db.UsageFilter{Termination: termination}, true))
	}
}
