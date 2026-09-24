package review

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
)

// memLedger is an in-memory LedgerQuerier. It applies Zone, Class, Since
// and Until, which is all LedgerDiagnostics asks of QueryLedger. failures
// makes that many calls fail first.
type memLedger struct {
	events   []ledger.Event
	failures int
}

func (m *memLedger) QueryLedger(_ context.Context, q ledger.Query) ([]ledger.ZoneEvents, error) {
	if m.failures > 0 {
		m.failures--
		return nil, assert.AnError
	}
	var out []ledger.Event
	for _, e := range m.events {
		if e.Zone != q.Zone || (q.Class != nil && e.EventClass != *q.Class) ||
			e.Timestamp.Before(q.Since) || (!q.Until.IsZero() && !e.Timestamp.Before(q.Until)) {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return []ledger.ZoneEvents{{Zone: q.Zone, Events: out}}, nil
}

func diagOn(date, identity, diagnostic, seat string) ledger.Event {
	ts, _ := time.Parse("2006-01-02", date)
	p := map[string]any{"kind": "diagnostic", "diagnostic": diagnostic, "identity": identity, "detail": "failed"}
	if seat != "" {
		p["seat"] = seat
	}
	return ledger.Event{
		EventID: ledger.NewEventID(), Zone: "default", Source: "host-a", SourceSeq: 1,
		Timestamp: ts.Add(time.Hour), EventClass: ledger.ClassHealth, PayloadTier: ledger.TierStructured, Payload: p,
	}
}

func fromSource(e ledger.Event, source string) ledger.Event {
	e.Source = source
	e.Timestamp = e.Timestamp.Add(time.Minute)
	return e
}

// ledgerSource is the shipped source over an in-memory ledger, deduping
// across dates through the same store the Runner saves into.
func ledgerSource(store *fakeReviewStore, l *memLedger) LedgerDiagnostics {
	return LedgerDiagnostics{Store: l, Digested: store, Zones: []string{"default"}}
}

func newTestRunner(store Store, diags DiagnosticSource) *Runner {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return &Runner{Store: store, Diagnostics: diags, Loc: time.UTC, Now: func() time.Time { return now }, BackfillDays: 7}
}

func errorsOf(r Report) []friction.Signal {
	var out []friction.Signal
	for _, s := range r.Snapshot.Signals {
		if s.Kind == friction.KindError {
			out = append(out, s)
		}
	}
	return out
}

func TestRunnerDiagnostics(t *testing.T) {
	ctx := t.Context()

	// Port of worker_evidence_flows_through_errors_and_dedup (flow only).
	t.Run("worker_evidence_flows_through_errors_and_dedup", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{
			diagOn("2026-09-13", "run-1:ci_trust", "ci_trust", "seat-01"),
			diagOn("2026-09-14", "run-1:ci_trust", "ci_trust", "seat-01"),
			diagOn("2026-09-14", "run-1:ci_review", "ci_review", "seat-01"),
		}}))
		first, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		errs := errorsOf(first)
		require.Len(t, errs, 1)
		assert.Equal(t, "ci_trust", errs[0].ToolName)
		assert.Equal(t, "seat-01", errs[0].Dims.Seat)
		assert.Equal(t, 1, first.Snapshot.SessionsScanned)

		second, err := r.BuildDate(ctx, "2026-09-14", BuildOptions{})
		require.NoError(t, err)
		errs = errorsOf(second)
		require.Len(t, errs, 1, "a new kind after the first scan must survive identity dedup")
		assert.Equal(t, "ci_review", errs[0].ToolName)
	})

	// Review Focus 3.
	t.Run("identity_dedupes_across_dates_and_sources", func(t *testing.T) {
		store := newFakeReviewStore()
		e := diagOn("2026-09-13", "run-9:x", "x", "")
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{
			e, fromSource(e, "host-b"), diagOn("2026-09-15", "run-9:x", "x", ""),
		}}))
		first, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		require.Len(t, errorsOf(first), 1, "two sources, one identity, one signal")
		assert.Equal(t, "host-a", errorsOf(first)[0].Dims.Machine, "earliest occurrence wins")
		later, err := r.BuildDate(ctx, "2026-09-15", BuildOptions{})
		require.NoError(t, err)
		assert.Empty(t, errorsOf(later))
		assert.Equal(t, "2026-09-13", store.digested["run-9:x"])
	})

	t.Run("rebuild_keeps_diagnostic_membership", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{diagOn("2026-09-13", "run-1:x", "x", "")}}))
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{Rebuild: true})
		require.NoError(t, err)
		require.Len(t, errorsOf(rep), 1, "the source keeps a same-date identity, and PR 5 does not duplicate it")
		assert.Equal(t, 1, rep.Snapshot.SessionsScanned)
	})

	t.Run("diagnostics_never_raise_p0", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{
			diagOn("2026-09-13", "a:x", "x", ""), diagOn("2026-09-13", "b:x", "x", ""), diagOn("2026-09-13", "c:x", "x", ""),
		}}))
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		assert.Len(t, errorsOf(rep), 3)
		assert.Empty(t, rep.Snapshot.P0Alerts, "three subjects, one tool, still no P0 (§6.5)")
	})

	t.Run("source_error_fails_the_build_and_next_run_succeeds", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{
			failures: 1,
			events:   []ledger.Event{diagOn("2026-09-13", "run-1:x", "x", "")},
		}))
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.ErrorIs(t, err, assert.AnError)
		assert.NotContains(t, store.digests, "2026-09-13", "no digest without its diagnostics")
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		assert.True(t, rep.Written)
		assert.Len(t, errorsOf(rep), 1)
	})

	t.Run("persists_diagnostic_subject_rows", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{diagOn("2026-09-13", "run-1:x", "x", "")}}))
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		require.Len(t, store.saves, 1)
		assert.Contains(t, store.saves[0].subjects,
			db.FrictionDigestSubject{SubjectID: "run-1:x", Date: "2026-09-13", SubjectKind: friction.SubjectDiagnostic})
	})

	t.Run("off_means_nothing_consumed", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, nil)
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		assert.Empty(t, errorsOf(rep))
		assert.Empty(t, store.digested)
	})

	t.Run("dry_run_records_no_subjects", func(t *testing.T) {
		store := newFakeReviewStore()
		r := newTestRunner(store, ledgerSource(store, &memLedger{events: []ledger.Event{diagOn("2026-09-13", "run-1:x", "x", "")}}))
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{DryRun: true})
		require.NoError(t, err)
		assert.Len(t, errorsOf(rep), 1)
		assert.Empty(t, store.saves)
	})
}
