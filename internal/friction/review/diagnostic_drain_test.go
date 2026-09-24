package review_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

type oneDateDiagnostics struct {
	date string
	sigs []friction.Signal
}

func (o oneDateDiagnostics) DiagnosticSignalsForDate(_ context.Context, date string, _ *time.Location) ([]friction.Signal, error) {
	if date != o.date {
		return nil, nil
	}
	return o.sigs, nil
}

func TestPendingDiagnosticFilingDrains(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	diag := friction.Signal{
		Kind: friction.KindError, SubjectKind: friction.SubjectDiagnostic,
		SubjectID: "run-1:nightly_check", Detector: "error", ToolName: "nightly_check",
		Text: "run-1:nightly_check: failed", Dims: friction.Dims{Machine: "ci-runner"},
	}
	down := katatest.New(t)
	down.Close()
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	runner := &review.Runner{
		Store: d, Loc: time.UTC, Now: func() time.Time { return now }, BackfillDays: 7, AutoFile: true,
		Diagnostics: oneDateDiagnostics{date: "2026-09-20", sigs: []friction.Signal{diag}},
	}
	filer := &filing.Filer{
		Kata: kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: down.Endpoint(),
			Project: katatest.DefaultProjectName, Actor: "agentsview",
		}),
		Store: d, Now: func() time.Time { return now }, Policy: filing.Policy{Kinds: filing.DefaultKinds()},
		Snapshot: runner.Snapshot, Rerender: runner.Rerender,
	}
	runner.Filer = filer
	_, err := runner.BuildDate(t.Context(), "2026-09-20", review.BuildOptions{})
	require.NoError(t, err)
	links, err := d.GetFrictionIssueLinks(t.Context(), []string{diag.Fingerprint()})
	require.NoError(t, err)
	require.Contains(t, links, diag.Fingerprint())
	assert.Equal(t, db.FrictionLinkStatePending, links[diag.Fingerprint()].State)

	dates, err := d.DigestDatesForFingerprints(t.Context(), []string{diag.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-20"}, dates, "a diagnostic fingerprint resolves to its digest")

	live := katatest.New(t)
	filer.Kata = kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: live.Endpoint(),
		Project: katatest.DefaultProjectName, Actor: "agentsview",
	})
	apiSink, inlineSink := &filingEventSink{}, &filingEventSink{}
	filer.Ledger, filer.InlineLedger = apiSink, inlineSink
	_, err = filer.Drain(t.Context())
	require.NoError(t, err)
	links, err = d.GetFrictionIssueLinks(t.Context(), []string{diag.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, links[diag.Fingerprint()].State)
	creates := live.RequestsMatching("POST", "/issues")
	require.Len(t, creates, 1)
	assert.Contains(t, string(creates[0].Body), `"force_new":true`)
	require.NotNil(t, inlineSink.event("friction.issue.filed"))
	assert.Empty(t, apiSink.events, "drain must use the sink for work already under the review lock")
}
