package review

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/frictionevents"
	"go.kenn.io/agentsview/internal/ledger"
)

type recordingSink struct {
	mu     sync.Mutex
	events []ledger.Event
	err    error
}

func (s *recordingSink) Append(_ context.Context, _ string, events []ledger.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Payload.(map[string]any)["kind"].(string))
	}
	return out
}

func TestPatternTransitions(t *testing.T) {
	a := friction.Signal{Kind: friction.KindError, SubjectID: "s1", ToolName: "bash", Text: "exit 1"}
	b := friction.Signal{Kind: friction.KindError, SubjectID: "s2", ToolName: "grep", Text: "no match"}
	c := friction.Signal{Kind: friction.KindError, SubjectID: "s3", ToolName: "make", Text: "fail"}
	before := map[string]db.FrictionPattern{
		b.Fingerprint(): {Fingerprint: b.Fingerprint(), FirstSeenDate: "2026-09-10", OccurrenceCount: 3},
		c.Fingerprint(): {Fingerprint: c.Fingerprint(), FirstSeenDate: "2026-09-13", OccurrenceCount: 1},
	}
	first, recurred := patternTransitions([]friction.Signal{a, a, b, c}, before, "2026-09-13", nil)
	require.Len(t, first, 2)
	assert.Equal(t, a.Fingerprint(), first[0].Fingerprint)
	assert.Equal(t, "s1", first[0].SubjectID)
	assert.Equal(t, c.Fingerprint(), first[1].Fingerprint, "first seen on this date: a rebuild re-emits the same ID")
	require.Len(t, recurred, 1)
	assert.Equal(t, b.Fingerprint(), recurred[0].Fingerprint)
	assert.Equal(t, 4, recurred[0].OccurrenceCount)
}

func TestRunnerLedgerEvents(t *testing.T) {
	ctx := context.Background()
	seedSession := func(store *fakeReviewStore, date, id, tool string) {
		store.sessions[date] = append(store.sessions[date], db.FrictionSubject{
			SubjectID: id, SubjectKind: friction.SubjectSession,
			Machine: "m", RulesVersion: friction.RulesVersion,
		})
		store.findings[id] = append(store.findings[id], db.FrictionFinding{
			SessionID: id, Kind: "error", Detector: "error",
			ToolName: tool, Text: "exit 1", Title: "[friction/error] " + tool + ": exit 1",
			Fingerprint:  (friction.Signal{Kind: friction.KindError, ToolName: tool, Text: "exit 1"}).Fingerprint(),
			RulesVersion: friction.RulesVersion,
		})
	}

	t.Run("one_digest_built_per_build", func(t *testing.T) {
		store := newFakeReviewStore()
		seedSession(store, "2026-09-13", "s1", "bash")
		sink := &recordingSink{}
		r := newTestRunner(store, nil)
		r.Ledger, r.LedgerSource = sink, "av-test"
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		assert.Equal(t, []string{frictionevents.KindDigestBuilt, frictionevents.KindPatternFirstSeen}, sink.kinds())
		e := sink.events[0]
		require.NotNil(t, e.CorrelationID)
		assert.Equal(t, store.digests["2026-09-13"].RunID, e.CorrelationID.String())

		seedSession(store, "2026-09-14", "s2", "bash")
		_, err = r.BuildDate(ctx, "2026-09-14", BuildOptions{})
		require.NoError(t, err)
		assert.Equal(t, frictionevents.KindPatternRecurred, sink.kinds()[3])
	})

	t.Run("ledger_failure_never_fails_a_build", func(t *testing.T) {
		store := newFakeReviewStore()
		seedSession(store, "2026-09-13", "s1", "bash")
		r := newTestRunner(store, nil)
		r.Ledger, r.LedgerSource = &recordingSink{err: errors.New("ledger down")}, "av-test"
		rep, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		assert.True(t, rep.Written)
		assert.Contains(t, store.digests, "2026-09-13")
	})

	t.Run("dry_run_emits_nothing", func(t *testing.T) {
		store := newFakeReviewStore()
		seedSession(store, "2026-09-13", "s1", "bash")
		sink := &recordingSink{}
		r := newTestRunner(store, nil)
		r.Ledger, r.LedgerSource = sink, "av-test"
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{DryRun: true})
		require.NoError(t, err)
		assert.Empty(t, sink.events)
	})

	// Review Focus 5.
	t.Run("rebuild_emits_new_digest_event_and_reuses_pattern_ids", func(t *testing.T) {
		store := newFakeReviewStore()
		seedSession(store, "2026-09-13", "s1", "bash")
		sink := &recordingSink{}
		r := newTestRunner(store, nil)
		r.Ledger, r.LedgerSource = sink, "av-test"
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
		_, err = r.BuildDate(ctx, "2026-09-13", BuildOptions{Rebuild: true})
		require.NoError(t, err)
		require.Len(t, sink.events, 4)
		assert.NotEqual(t, sink.events[0].EventID, sink.events[2].EventID, "revision 2 is a new build")
		assert.Equal(t, sink.events[1].EventID, sink.events[3].EventID, "first_seen dedupes by ID")
	})

	t.Run("nil_ledger_is_a_no_op", func(t *testing.T) {
		store := newFakeReviewStore()
		seedSession(store, "2026-09-13", "s1", "bash")
		r := newTestRunner(store, nil)
		_, err := r.BuildDate(ctx, "2026-09-13", BuildOptions{})
		require.NoError(t, err)
	})
}
