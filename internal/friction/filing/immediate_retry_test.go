package filing_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func TestImmediateFilingRetryWithoutDigest(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	sig := errSig("claude:today", "Bash", "configuration not found")
	dbtest.SeedSession(t, h.db, sig.SubjectID, "example")
	require.NoError(t, h.db.ReplaceSessionFriction(t.Context(), sig.SubjectID, []db.FrictionFinding{{
		SessionID: sig.SubjectID, Kind: string(sig.Kind), Detector: sig.Detector,
		ToolName: sig.ToolName, Text: sig.Text, MessageOrdinal: sig.Ordinal, Fingerprint: sig.Fingerprint(),
	}}, nil, friction.RulesVersion, "synthetic"))
	h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 503, Code: "unavailable"})
	row, err := h.f.File(t.Context(), sig, filing.RunContext{Date: "2026-09-20"})
	require.Error(t, err)
	assert.Equal(t, db.FrictionLinkStateFailed, row.State)
	h.f.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	report, err := h.f.Drain(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, report.Created)
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, links[sig.Fingerprint()].State)
}

func TestOutboxStopsRetryingTrashedDigestSession(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	sig := errSig("claude:trashed", "Bash", "configuration not found")
	dbtest.SeedSession(t, h.db, sig.SubjectID, "example")
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20"}}
	h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 503, Code: "unavailable"})
	_, err := h.f.File(t.Context(), sig, runCtx())
	require.Error(t, err)
	require.NoError(t, h.db.SoftDeleteSession(t.Context(), sig.SubjectID))
	h.f.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.kata.ResetRequests()
	report, err := h.f.Drain(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, report.NeedsHuman)
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	row := links[sig.Fingerprint()]
	assert.Equal(t, db.FrictionLinkStateNeedsHuman, row.State)
	assert.Equal(t, "signal_not_found", row.LastErrorCode)
	assert.Nil(t, row.NextAttemptAt)
	assert.Empty(t, h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"))
	h.f.Now = func() time.Time { return testNow.Add(24 * time.Hour) }
	h.kata.ResetRequests()
	report, err = h.f.Drain(t.Context())
	require.NoError(t, err)
	assert.Zero(t, report.Failed)
	assert.Empty(t, h.kata.RequestsMatching(http.MethodGet, "/api/v1/projects/17/issues"), "a permanent failure is excluded from later drains")
}

func TestImmediateRecurrenceRetryUsesNewestStoredOccurrence(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	seed := func(id, date string) friction.Signal {
		sig := errSig(id, "Bash", "configuration not found")
		sig.OccurredAt = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
		if date == "2026-09-21" {
			sig.OccurredAt = sig.OccurredAt.Add(24 * time.Hour)
		}
		dbtest.SeedSession(t, h.db, id, "example")
		require.NoError(t, h.db.ReplaceSessionFriction(t.Context(), id, []db.FrictionFinding{{
			SessionID: id, Kind: string(sig.Kind), Detector: sig.Detector, ToolName: sig.ToolName,
			Text: sig.Text, MessageOrdinal: sig.Ordinal, OccurredAt: &sig.OccurredAt, Fingerprint: sig.Fingerprint(),
		}}, nil, friction.RulesVersion, "synthetic"))
		return sig
	}
	old := seed("claude:a", "2026-09-20")
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{old}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20"}}
	issue := closedMatch(h, old, "done")
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: old.Fingerprint(), State: db.FrictionLinkStateLinked, IssueUID: issue.UID,
		QualifiedID: "agentsview#" + issue.ShortID, LinkSource: db.FrictionLinkSourceCreated,
		LastRecurrenceDate: "2026-09-20", UpdatedAt: testNow,
	}))
	latest := seed("claude:z", "2026-09-21")
	h.kata.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})
	_, err := h.f.File(t.Context(), latest, filing.RunContext{Date: "2026-09-21"})
	require.Error(t, err)
	h.f.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	_, err = h.f.Drain(t.Context())
	require.NoError(t, err)
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{old.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, links[old.Fingerprint()].State)
	assert.Equal(t, "2026-09-21", links[old.Fingerprint()].LastRecurrenceDate)
	after, _ := h.kata.Issue(issue.UID)
	assert.Equal(t, "open", after.Status)
}
