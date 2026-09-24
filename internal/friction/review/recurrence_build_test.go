package review_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
)

type memStore struct {
	subjects []db.FrictionSubject
	findings []db.FrictionFinding
	usage    map[string]friction.SessionUsage
	saved    *db.FrictionDigest
}

func (m *memStore) FrictionSubjectsForDate(context.Context, string, *time.Location, bool) ([]db.FrictionSubject, error) {
	return m.subjects, nil
}

func (m *memStore) FrictionFindingsForSubjects(context.Context, []string) ([]db.FrictionFinding, error) {
	return m.findings, nil
}

func (m *memStore) FrictionUsageForSessions(context.Context, []string) (map[string]friction.SessionUsage, error) {
	return m.usage, nil
}

func (m *memStore) FrictionArchiveSpend(context.Context, string, string, *time.Location) (*friction.ArchiveSpend, error) {
	return nil, nil
}

func (m *memStore) SaveFrictionDigest(_ context.Context, d db.FrictionDigest, _ []db.FrictionDigestSubject, _ []db.FrictionPatternUpdate) error {
	m.saved = &d
	return nil
}
func (m *memStore) GetFrictionDigest(context.Context, string) (*db.FrictionDigest, error) {
	return m.saved, nil
}
func (m *memStore) LatestFrictionDigestDate(context.Context) (string, error) { return "", nil }
func (m *memStore) EarliestSessionDate(context.Context, *time.Location) (string, error) {
	return "2026-07-01", nil
}
func (m *memStore) UpdateFrictionDigestRender(context.Context, string, []byte, []byte, int) error {
	return nil
}

// openFingerprints is jilog's OpenTitlesTracker: these fingerprints are
// already linked to open issues, and it files nothing.
type openFingerprints struct {
	open     map[string]bool
	snapshot int
	filedAt  int
}

func (o *openFingerprints) Ready(context.Context) bool { return true }
func (o *openFingerprints) SnapshotOpen(_ context.Context, fps []string) (map[string]bool, error) {
	o.snapshot++
	out := map[string]bool{}
	for _, fp := range fps {
		if o.open[fp] {
			out[fp] = true
		}
	}
	return out, nil
}

func (o *openFingerprints) FileAll(context.Context, []friction.Signal, filing.RunContext) (filing.Report, error) {
	o.filedAt = o.snapshot
	return filing.Report{}, nil
}

func (o *openFingerprints) Links(context.Context, []string) (map[string]friction.IssueRef, error) {
	return map[string]friction.IssueRef{}, nil
}
func (o *openFingerprints) Drain(context.Context) (filing.Report, error) { return filing.Report{}, nil }

func correctionFixture(t *testing.T, subject, context, cost, role string) *memStore {
	t.Helper()
	sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: subject, Detector: "correction.coding", Text: context, Ordinal: new(1)}
	c, err := friction.ParseUSD(cost)
	require.NoError(t, err)
	return &memStore{
		subjects: []db.FrictionSubject{{
			SubjectID: subject, SubjectKind: friction.SubjectSession, Machine: "laptop-a", Agent: "claude",
			LastActivity: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC), RulesVersion: friction.RulesVersion,
		}},
		findings: []db.FrictionFinding{{
			SessionID: subject, Kind: "correction", Detector: "correction.coding", MessageOrdinal: new(1),
			Text: context, Title: sig.Title(), Fingerprint: sig.Fingerprint(), Seq: 0, RulesVersion: friction.RulesVersion,
		}},
		usage: map[string]friction.SessionUsage{subject: {SubjectID: subject, Role: role, InputTokens: 100, OutputTokens: 10, CostUSD: &c}},
	}
}

func TestRunReviewRecurrenceAnnotations(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 6, 3, 0, 0, 0, time.UTC) }
	context := "no, use the calendar cli for calendar"
	t.Run("run_review_annotates_recurring_signal_with_session_cost", func(t *testing.T) {
		store := correctionFixture(t, "sess-r_explore", context, "4.2", "(root)")
		sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "sess-r_explore", Text: context}
		filer := &openFingerprints{open: map[string]bool{sig.Fingerprint(): true}}
		r := &review.Runner{Store: store, Filer: filer, Loc: time.UTC, Now: now, BackfillDays: 7}
		rep, err := r.BuildDate(t.Context(), "2026-07-05", review.BuildOptions{})
		require.NoError(t, err)
		require.Len(t, rep.Snapshot.Signals, 1)
		require.NotNil(t, store.saved)
		md := string(store.saved.Markdown)
		assert.Contains(t, md, "(recurred in sessions totaling $4.20)")
		assert.Contains(t, md, "## Spend")
	})
	t.Run("run_review_no_recurrence_annotation_for_new_signals", func(t *testing.T) {
		store := correctionFixture(t, "sess-n", "please stop doing that thing", "1.0", "(root)")
		filer := &openFingerprints{open: map[string]bool{"fl1:some-other-title": true}}
		r := &review.Runner{Store: store, Filer: filer, Loc: time.UTC, Now: now, BackfillDays: 7}
		_, err := r.BuildDate(t.Context(), "2026-07-05", review.BuildOptions{})
		require.NoError(t, err)
		md := string(store.saved.Markdown)
		assert.NotContains(t, md, "recurred in sessions totaling")
		assert.Contains(t, md, "- `(root)`: $1.00")
	})
	t.Run("new_issue_in_this_build_is_not_a_recurrence", func(t *testing.T) {
		store := correctionFixture(t, "sess-x", context, "2.0", "(root)")
		filer := &openFingerprints{open: map[string]bool{}}
		r := &review.Runner{Store: store, Filer: filer, Loc: time.UTC, Now: now, BackfillDays: 7, AutoFile: true}
		_, err := r.BuildDate(t.Context(), "2026-07-05", review.BuildOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, filer.filedAt, "the snapshot ran once, before FileAll")
		assert.NotContains(t, string(store.saved.Markdown), "recurred in sessions totaling")
	})
	t.Run("dry_run_skips_the_snapshot", func(t *testing.T) {
		store := correctionFixture(t, "sess-d", context, "2.0", "(root)")
		filer := &openFingerprints{open: map[string]bool{}}
		r := &review.Runner{Store: store, Filer: filer, Loc: time.UTC, Now: now, BackfillDays: 7}
		_, err := r.BuildDate(t.Context(), "2026-07-05", review.BuildOptions{DryRun: true})
		require.NoError(t, err)
		assert.Equal(t, 0, filer.snapshot)
	})
	t.Run("non_hub_has_no_annotations", func(t *testing.T) {
		store := correctionFixture(t, "sess-p", context, "2.0", "(root)")
		r := &review.Runner{Store: store, Loc: time.UTC, Now: now, BackfillDays: 7} // Filer nil on a pusher
		_, err := r.BuildDate(t.Context(), "2026-07-05", review.BuildOptions{})
		require.NoError(t, err)
		assert.NotContains(t, string(store.saved.Markdown), "recurred in sessions totaling")
	})
}
