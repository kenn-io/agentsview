//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"hegel.dev/go/hegel"
)

func TestLoadRawParityReadinessRejectsUnsafeTransactions(t *testing.T) {
	f := newHostedFixture(t, "tenant-parity")
	for _, options := range []sql.TxOptions{{}, {Isolation: sql.LevelRepeatableRead}, {ReadOnly: true}} {
		tx, err := f.runtime.BeginTx(t.Context(), &options)
		require.NoError(t, err)
		_, err = LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "parser-1")
		assert.ErrorContains(t, err, "REPEATABLE READ READ ONLY")
		require.NoError(t, tx.Rollback())
	}
	tx := parityTx(t, f.runtime)
	require.NoError(t, tx.Rollback())
	_, err := LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "parser-1")
	assert.ErrorIs(t, err, sql.ErrTxDone)
}

func parityReadinessFixture(t *testing.T) projectionFixture {
	t.Helper()
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "complete-"+device, "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("shared")))
	}
	m, _ := f.acceptScoped(t, "device-a", "failed", "", parser.AgentCodex, "failed-root", "")
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), f.lease(t, m), "parse", "synthetic failure"))
	// An older successful projection is pending under the requested version.
	m, _ = f.acceptScoped(t, "device-a", "stale", "", parser.AgentClaude, "stale-root", "")
	out := projectionOutcome("stale")
	out.Outcome.Results[0].Result.Session.Agent = parser.AgentClaude
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	stale := m
	// Partial publication is still pending, despite retaining useful rows.
	m, _ = f.acceptScoped(t, "device-a", "incomplete", "", parser.AgentCodex, "incomplete-root", "")
	out = projectionOutcome("partial")
	out.Outcome.ResultSetComplete = false
	require.ErrorIs(t, f.sink.Project(t.Context(), f.lease(t, m), m, out), rawderive.ErrProjectionRetrying)
	_, err := f.sink.SelectSourceGeneration(t.Context(), stale, "parser-2")
	require.NoError(t, err)
	// No projection selection, and an accepted provider without raw capture.
	f.acceptScoped(t, "device-a", "pending", "", parser.AgentCodex, "pending-root", "")
	f.acceptScoped(t, "device-a", "unsupported", "", parser.AgentGemini, "unsupported-root", "")
	return f
}

// Counts concern sources, so two devices sharing one physical session count
// separately. Historical success and partial output must never inflate ready.
func TestLoadRawParityReadinessCurrentHeads(t *testing.T) {
	f := parityReadinessFixture(t)
	tx := parityTx(t, f.runtime)
	got, err := LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "parser-1")
	require.NoError(t, err)
	assert.Equal(t, []RawParityReadiness{
		{Device: "device-a", Provider: "claude", Pending: 1},
		{Device: "device-a", Provider: "codex", Projected: 1, Pending: 2, Failed: 1},
		{Device: "device-a", Provider: "gemini", Unsupported: 1},
		{Device: "device-b", Provider: "codex", Projected: 1},
	}, got)
	got, err = LoadRawParityReadiness(t.Context(), tx, RawParityScope{Device: "device-b", Provider: "codex"}, "parser-2")
	require.NoError(t, err)
	assert.Equal(t, []RawParityReadiness{{Device: "device-b", Provider: "codex", Pending: 1}}, got)
	_, err = LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "")
	assert.Error(t, err)
	_, err = LoadRawParityReadiness(t.Context(), nil, RawParityScope{}, "parser-1")
	assert.Error(t, err)
}

func TestLoadRawParityReadinessUsesCallerTransaction(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "before", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("before")))
	tx := parityTx(t, f.runtime)
	before, err := LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "parser-1")
	require.NoError(t, err)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	after, err := LoadRawParityReadiness(t.Context(), tx, RawParityScope{}, "parser-1")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, tx.Commit())
	after, err = LoadRawParityReadiness(t.Context(), parityTx(t, f.runtime), RawParityScope{}, "parser-1")
	require.NoError(t, err)
	assert.Equal(t, []RawParityReadiness{{Device: "device-a", Provider: "codex", Pending: 1}}, after)
}

func TestLoadRawParityReadinessIgnoresHistoricalFailure(t *testing.T) {
	f := newProjectionFixture(t)
	m, receipt := f.accept(t, "device-a", "failed", "")
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), f.lease(t, m), "parse", "synthetic failure"))
	f.accept(t, "device-a", "new-head", receipt.Receipt)
	got, err := LoadRawParityReadiness(t.Context(), parityTx(t, f.runtime), RawParityScope{}, "parser-1")
	require.NoError(t, err)
	assert.Equal(t, []RawParityReadiness{{Device: "device-a", Provider: "codex", Pending: 1}}, got)
}

// An independent source list is the oracle for complete, filtered counts.
func TestLoadRawParityReadinessScopeProperty(t *testing.T) {
	requireHegel(t)
	f := parityReadinessFixture(t)
	tx := parityTx(t, f.runtime)
	sources := []struct{ device, provider, state string }{
		{"device-a", "codex", "projected"}, {"device-b", "codex", "projected"},
		{"device-a", "codex", "failed"}, {"device-a", "claude", "pending"},
		{"device-a", "codex", "pending"}, {"device-a", "codex", "pending"},
		{"device-a", "gemini", "unsupported"},
	}
	hegel.Test(t, func(ht *hegel.T) {
		unknown := "unknown-" + hex.EncodeToString([]byte(hegel.Draw(ht, hegel.Text())))
		device := []string{"", "device-a", "device-b", unknown}[hegel.Draw(ht, hegel.Integers(0, 3))]
		provider := []string{"", "codex", "claude", "gemini", unknown}[hegel.Draw(ht, hegel.Integers(0, 4))]
		// Materialize a canonical version within its documented 128-byte limit.
		version := []string{"parser-1", "parser-2", unknown[:min(len(unknown), 128)]}[hegel.Draw(ht, hegel.Integers(0, 2))]
		got, err := LoadRawParityReadiness(t.Context(), tx, RawParityScope{Device: device, Provider: provider}, version)
		require.NoError(ht, err)
		want := map[[2]string]map[string]int{}
		for _, s := range sources {
			if device != "" && device != s.device || provider != "" && provider != s.provider {
				continue
			}
			key := [2]string{s.device, s.provider}
			if want[key] == nil {
				want[key] = map[string]int{}
			}
			state := s.state
			if version != "parser-1" && state != "unsupported" {
				state = "pending"
			}
			want[key][state]++
		}
		actual := map[[2]string]map[string]int{}
		for _, r := range got {
			counts := map[string]int{}
			for state, count := range map[string]int{"projected": r.Projected, "pending": r.Pending, "failed": r.Failed, "unsupported": r.Unsupported} {
				if count > 0 {
					counts[state] = count
				}
			}
			actual[[2]string{r.Device, r.Provider}] = counts
		}
		assert.Equal(ht, want, actual)
	}, hegel.WithTestCases(1000))
}
