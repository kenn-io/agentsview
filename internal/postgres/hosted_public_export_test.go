//go:build pgtest

package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// HostedPublicFixture is shared only with the external service/HTTP tests.
func HostedPublicFixture(t *testing.T) (db.Store, func(), func()) {
	t.Helper()
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("readable transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	split := func() {
		b, _ := f.accept(t, "device-b", "capture-b", "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("other transcript")))
	}
	pin := func() {
		_, err := h.PinMessage(t.Context(), "codex:portable", 0, new("remember this"))
		require.NoError(t, err)
	}
	return h, split, pin
}

// HostedClaudeUsageFixture projects distinct transcripts with the same native ID.
func HostedClaudeUsageFixture(t *testing.T, rawID string, branches int) db.Store {
	t.Helper()
	f := newProjectionFixture(t)
	for i := range branches {
		device := fmt.Sprintf("device-%d", i)
		m, _ := f.accept(t, device, "capture-"+device, "", parser.AgentClaude)
		outcome := projectionOutcome("transcript-" + device)
		session := &outcome.Outcome.Results[0].Result.Session
		session.ID = rawID
		session.SourceSessionID = rawID
		session.Agent = parser.AgentClaude
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	}
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}
