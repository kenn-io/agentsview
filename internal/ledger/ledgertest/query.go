// Package ledgertest holds storage conformance tests that run unchanged
// against every ledger backend (SQLite and PostgreSQL).
package ledgertest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/ledger"
)

// QueryStore is the storage surface the query conformance tests need.
type QueryStore interface {
	ledger.WriterStore
	QueryLedger(ctx context.Context, q ledger.Query) ([]ledger.ZoneEvents, error)
	LedgerZones(ctx context.Context) ([]string, error)
}

// dispatch writes one segment like jilog's write_dispatch_event
// (query.rs:376-409): a state_change with object_ref subsystem:<name> and
// a payload carrying kind, machine, subsystem and summary.
func dispatch(t *testing.T, st QueryStore, zone, source string, seq uint64, subsystem, summary string, ts time.Time) {
	t.Helper()
	actor := "machine:" + source
	object := "subsystem:" + subsystem
	seg := ledger.NewSegment(source, seq, ts)
	seg.Append(ledger.Event{
		EventID:     ledger.DeterministicEventID(source, fmt.Sprintf("%s/%d", zone, seq)),
		Zone:        zone,
		Source:      source,
		SourceSeq:   seq,
		Timestamp:   ts,
		ActorRef:    &actor,
		ObjectRef:   &object,
		EventClass:  ledger.ClassStateChange,
		PayloadTier: ledger.TierStructured,
		Payload: map[string]any{
			"kind": "machine-dispatch", "machine": source,
			"subsystem": subsystem, "summary": summary,
		},
	})
	require.NoError(t, seg.Seal())
	_, err := st.AppendLedgerSegment(t.Context(), zone, seg, ledger.OriginImport)
	require.NoError(t, err)
}

// buildTestLedger ports build_test_ledger (query.rs:420-439).
func buildTestLedger(t *testing.T, st QueryStore, now time.Time) {
	t.Helper()
	dispatch(t, st, "test-zone", "src1", 1, "hook-kata-logger", "logged 3 issues", now.Add(-1*time.Hour))
	dispatch(t, st, "test-zone", "src1", 2, "hook-feature-capture", "captured 2 features", now.Add(-2*time.Hour))
	dispatch(t, st, "test-zone", "src1", 3, "nightly-extractor", "109 signals, 1 P0", now.Add(-6*time.Hour))
	dispatch(t, st, "test-zone", "src1", 4, "opsctl", "phase 1 deployed", now.Add(-20*24*time.Hour))
}

func count(results []ledger.ZoneEvents) int {
	n := 0
	for _, z := range results {
		n += len(z.Events)
	}
	return n
}

// QueryConformance ports jilog's query tests (query.rs:496-570) and the
// ledger-sqlite read tests (db.rs:696-730) onto QueryLedger, plus the SQL
// filtering jilog does not do (D20).
func QueryConformance(t *testing.T, open func(t *testing.T) QueryStore) {
	now := time.Now().UTC()
	health := ledger.ClassHealth
	stateChange := ledger.ClassStateChange
	tests := []struct {
		name  string
		query ledger.Query
		want  int
	}{
		{"query_events_filters_by_subsystem_glob", ledger.Query{Zone: "test-zone", Since: now.Add(-7 * 24 * time.Hour), Subsystems: []string{"hook-*"}, Limit: 100}, 2},
		{"query_events_filters_by_exact_subsystem", ledger.Query{Zone: "test-zone", Since: now.Add(-7 * 24 * time.Hour), Subsystems: []string{"nightly-extractor"}, Limit: 100}, 1},
		{"query_events_filters_by_time_cutoff/7d", ledger.Query{Zone: "test-zone", Since: now.Add(-7 * 24 * time.Hour), Limit: 100}, 3},
		{"query_events_filters_by_time_cutoff/30d", ledger.Query{Zone: "test-zone", Since: now.Add(-30 * 24 * time.Hour), Limit: 100}, 4},
		{"query_events_multi_pattern_or_logic", ledger.Query{Zone: "test-zone", Since: now.Add(-30 * 24 * time.Hour), Subsystems: []string{"hook-*", "nightly-extractor"}, Limit: 100}, 3},
		{"test_recent_events/limit", ledger.Query{Zone: "test-zone", Limit: 2}, 2},
		{"test_events_by_class/state_change", ledger.Query{Zone: "test-zone", Class: &stateChange, Limit: 10}, 4},
		{"test_events_by_class/health", ledger.Query{Zone: "test-zone", Class: &health, Limit: 10}, 0},
		{"prefix_needs_its_prefix", ledger.Query{Zone: "test-zone", Subsystems: []string{"ops*"}, Limit: 10}, 1},
		{"unknown_zone_is_empty", ledger.Query{Zone: "nope", Limit: 10}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := open(t)
			buildTestLedger(t, st, now)
			results, err := st.QueryLedger(t.Context(), tt.query)
			require.NoError(t, err)
			assert.Equal(t, tt.want, count(results))
		})
	}

	t.Run("newest_first_with_event_id_tiebreak", func(t *testing.T) {
		st := open(t)
		buildTestLedger(t, st, now)
		results, err := st.QueryLedger(t.Context(), ledger.Query{Zone: "test-zone", Limit: 10})
		require.NoError(t, err)
		require.Len(t, results, 1)
		var summaries []string
		for _, e := range results[0].Events {
			summaries = append(summaries, ledger.Summary(e))
		}
		assert.Equal(t, []string{"logged 3 issues", "captured 2 features", "109 signals, 1 P0", "phase 1 deployed"}, summaries)
	})

	t.Run("filters_run_before_the_limit_unlike_jilog", func(t *testing.T) {
		// jilog reads the newest 5 x limit rows and filters afterwards
		// (query.rs:210), so with 60 newer non-matching events and limit 10
		// it returns nothing. SQL filtering finds the older match (D20).
		st := open(t)
		for i := range 60 {
			dispatch(t, st, "busy", "src1", uint64(i+2), "noise", "noise", now.Add(-time.Duration(i)*time.Minute))
		}
		dispatch(t, st, "busy", "src1", 1, "rare", "the one", now.Add(-5*time.Hour))
		results, err := st.QueryLedger(t.Context(), ledger.Query{Zone: "busy", Subsystems: []string{"rare"}, Limit: 10})
		require.NoError(t, err)
		require.Equal(t, 1, count(results))
		assert.Equal(t, "the one", ledger.Summary(results[0].Events[0]))
	})

	t.Run("all_zones_in_name_order_empty_omitted", func(t *testing.T) {
		st := open(t)
		dispatch(t, st, "zone-b", "src1", 1, "x", "b", now)
		dispatch(t, st, "zone-a", "src1", 1, "y", "a", now)
		results, err := st.QueryLedger(t.Context(), ledger.Query{Subsystems: []string{"x"}, Limit: 10})
		require.NoError(t, err)
		require.Len(t, results, 1, "zone-a has no match and is omitted")
		assert.Equal(t, "zone-b", results[0].Zone)
		zones, err := st.LedgerZones(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"zone-a", "zone-b"}, zones)
	})

	t.Run("events_read_back_exactly", func(t *testing.T) {
		st := open(t)
		ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
		dispatch(t, st, "exact", "src1", 1, "x", "nanoseconds survive", ts)
		results, err := st.QueryLedger(t.Context(), ledger.Query{Zone: "exact", Limit: 1})
		require.NoError(t, err)
		require.Equal(t, 1, count(results))
		assert.True(t, results[0].Events[0].Timestamp.Equal(ts))
		assert.Equal(t, "exact", results[0].Events[0].Zone)
	})
}
