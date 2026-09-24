package review

import (
	"bytes"
	"context"
	"log"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
)

func appendEvents(t *testing.T, store interface {
	AppendLedgerSegment(context.Context, string, ledger.Segment, string) (ledger.PublishOutcome, error)
}, zone, source string, seq uint64, events ...ledger.Event,
) {
	t.Helper()
	seg := ledger.NewSegment(source, seq, time.Now())
	for _, e := range events {
		e.Zone, e.Source, e.SourceSeq = zone, source, seq
		seg.Append(e)
	}
	require.NoError(t, seg.Seal())
	_, err := store.AppendLedgerSegment(t.Context(), zone, seg, "local")
	require.NoError(t, err)
}

func diagAtTime(ts time.Time, identity string, extra map[string]any) ledger.Event {
	p := map[string]any{
		"kind": "diagnostic", "subsystem": "ci", "diagnostic": "ci_nightly_failed",
		"identity": identity, "detail": "lint step exited 1",
	}
	maps.Copy(p, extra)
	return ledger.Event{
		EventID: ledger.NewEventID(), Timestamp: ts, EventClass: ledger.ClassHealth,
		PayloadTier: ledger.TierStructured, Payload: p,
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestLedgerDiagnostics(t *testing.T) {
	loc := time.UTC
	day := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)

	t.Run("reads_only_the_local_date_and_labels_local_source", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		appendEvents(t, d, "default", "av-local", 1,
			diagAtTime(day.Add(-time.Minute), "early:x", nil),
			diagAtTime(day.Add(time.Hour), "run-2:ci", map[string]any{"seat": "seat-02"}),
			diagAtTime(day.Add(time.Hour), "run-1:ci", nil),
			diagAtTime(day.Add(24*time.Hour), "late:x", nil))
		src := LedgerDiagnostics{Store: d, Zones: []string{"default"}, LocalSource: "av-local", LocalLabel: "laptop"}
		sigs, err := src.DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		require.Len(t, sigs, 2)
		assert.Equal(t, "run-1:ci", sigs[0].SubjectID, "sorted by identity")
		assert.Equal(t, "run-2:ci", sigs[1].SubjectID)
		assert.Equal(t, "laptop", sigs[1].Dims.Machine)
		assert.Equal(t, "seat-02", sigs[1].Dims.Seat)
	})

	t.Run("foreign_source_keeps_its_name_and_same_identity_dedupes", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		appendEvents(t, d, "default", "host-b", 1, diagAtTime(day.Add(2*time.Hour), "run-1:ci", nil))
		appendEvents(t, d, "default", "host-c", 1, diagAtTime(day.Add(3*time.Hour), "run-1:ci", nil))
		sigs, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}}.DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		require.Len(t, sigs, 1)
		assert.Equal(t, "host-b", sigs[0].Dims.Machine, "earliest occurrence wins")
	})

	t.Run("subsystem_filter_and_star_default", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		noSub := diagAtTime(day.Add(time.Hour), "nosub:x", nil)
		delete(noSub.Payload.(map[string]any), "subsystem")
		appendEvents(t, d, "default", "host-b", 1, diagAtTime(day.Add(time.Hour), "run-1:ci", nil), noSub)
		star, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}, Subsystems: []string{"*"}}.
			DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		assert.Len(t, star, 2, `["*"] means no filter, so an event without a subsystem still counts`)
		only, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}, Subsystems: []string{"other*"}}.
			DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		assert.Empty(t, only)
	})

	// Review Focus 2.
	t.Run("newer_events_do_not_crowd_out_the_date", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		appendEvents(t, d, "default", "host-b", 1, diagAtTime(day.Add(time.Hour), "run-1:ci", nil))
		newer := make([]ledger.Event, 0, 20)
		for i := range 20 {
			newer = append(newer, diagAtTime(day.Add(48*time.Hour+time.Duration(i)*time.Minute), "later:"+string(rune('a'+i)), nil))
		}
		appendEvents(t, d, "default", "host-b", 2, newer...)
		sigs, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}, Limit: 5}.
			DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		require.Len(t, sigs, 1)
		assert.Equal(t, "run-1:ci", sigs[0].SubjectID)
	})

	// Review Focus 4.
	t.Run("malformed_warns_once_per_run", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		bad1 := diagAtTime(day.Add(time.Hour), "bad-1", nil)
		delete(bad1.Payload.(map[string]any), "detail")
		bad2 := diagAtTime(day.Add(time.Hour), "bad-2", nil)
		delete(bad2.Payload.(map[string]any), "detail")
		appendEvents(t, d, "default", "host-b", 1, bad1, bad2, diagAtTime(day.Add(time.Hour), "run-1:ci", nil))
		logs := captureLog(t)
		sigs, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}}.DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		assert.Len(t, sigs, 1)
		assert.Equal(t, 1, strings.Count(logs.String(), "friction diagnostics: ignored 2 malformed diagnostic event(s)"))
	})

	t.Run("non_diagnostic_health_events_are_silent", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		other := ledger.Event{
			EventID: ledger.NewEventID(), Timestamp: day.Add(time.Hour), EventClass: ledger.ClassHealth,
			PayloadTier: ledger.TierStructured, Payload: map[string]any{"kind": "friction.pattern.first_seen", "subsystem": "friction"},
		}
		appendEvents(t, d, "default", "host-b", 1, other)
		logs := captureLog(t)
		sigs, err := LedgerDiagnostics{Store: d, Zones: []string{"default"}}.DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		assert.Empty(t, sigs)
		assert.Empty(t, logs.String())
	})

	// Review Focus 3, and PR 5's DiagnosticSource contract.
	t.Run("drops_identities_digested_on_another_date", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		for _, date := range []string{"2026-09-12", "2026-09-13"} {
			id := map[string]string{"2026-09-12": "old:ci", "2026-09-13": "same:ci"}[date]
			require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
				Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion, BuiltAt: day, Revision: 1,
				SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte(""), RunID: "run-" + date,
			}, []db.FrictionDigestSubject{{SubjectID: id, Date: date, SubjectKind: friction.SubjectDiagnostic}}, nil))
		}
		appendEvents(t, d, "default", "host-b", 1,
			diagAtTime(day.Add(time.Hour), "old:ci", nil),
			diagAtTime(day.Add(time.Hour), "same:ci", nil),
			diagAtTime(day.Add(time.Hour), "new:ci", nil))
		sigs, err := LedgerDiagnostics{Store: d, Digested: d, Zones: []string{"default"}}.
			DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.NoError(t, err)
		ids := []string{}
		for _, s := range sigs {
			ids = append(ids, s.SubjectID)
		}
		assert.Equal(t, []string{"new:ci", "same:ci"}, ids, "another date's identity is dropped; this date's is kept for rebuilds")
	})

	t.Run("lookup_error_is_returned", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		appendEvents(t, d, "default", "host-b", 1, diagAtTime(day.Add(time.Hour), "run-1:ci", nil))
		_, err := LedgerDiagnostics{Store: d, Digested: failingDigested{}, Zones: []string{"default"}}.
			DiagnosticSignalsForDate(t.Context(), "2026-09-13", loc)
		require.ErrorIs(t, err, assert.AnError, "PR 5 fails the build so the poller retries")
	})
}

type failingDigested struct{}

func (failingDigested) FrictionDigestedSubjects(context.Context, []string) (map[string]string, error) {
	return nil, assert.AnError
}
