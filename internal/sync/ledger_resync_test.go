package sync_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestResyncAllPreservesLedger(t *testing.T) {
	env := setupTestEnv(t)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello").
		AddClaudeAssistant(tsEarlyS5, "Hi there!").
		String()
	env.writeClaudeSession(t, "test-proj", "ledger-test.jsonl", content)
	env.engine.SyncAll(t.Context(), nil)

	w := ledger.NewWriter(env.db, "default", "av-test", nil)
	seg, err := w.Append(t.Context(), []ledger.Event{{
		EventClass:  ledger.ClassDecision,
		PayloadTier: ledger.TierStructured,
		Timestamp:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Payload:     map[string]any{"subsystem": "test", "summary": "survives resync"},
	}})
	require.NoError(t, err)

	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "resync aborted: %v", stats.Warnings)

	segs, err := env.db.ListLedgerSegments(t.Context(), "default", "av-test", 0, 0)
	require.NoError(t, err)
	require.Len(t, segs, 1)
	assert.True(t, segs[0].ContentMatches(seg))
	st, err := env.db.LedgerStatus(t.Context(), "default")
	require.NoError(t, err)
	assert.Equal(t, 1, st.Events)
}
