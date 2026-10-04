package filing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

type readyOverride struct {
	filing.KataAPI
	ready bool
}

func (k readyOverride) Ready(context.Context) bool { return k.ready }

type destinationOverride struct {
	filing.KataAPI
	instanceUID string
	projectUID  string
	matches     []kata.Issue
	findCalls   int
}

func (k *destinationOverride) InstanceUID() string { return k.instanceUID }

func (k *destinationOverride) ProjectUID(context.Context) (string, error) {
	return k.projectUID, nil
}

func (k *destinationOverride) FindByMetadata(context.Context, string, string) ([]kata.Issue, error) {
	k.findCalls++
	return k.matches, nil
}

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

func TestExplicitRetryOfAbandonedFilingStartsFreshRetryWindow(t *testing.T) {
	old := testNow.Add(-15 * 24 * time.Hour)
	t.Run("create keeps its idempotency key", func(t *testing.T) {
		h := newHarness(t)
		sig := errSig("claude:abandoned-create", "Bash", "write failed")
		const createKey = "friction-force-new-existing-operation"
		require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
			Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateAbandoned,
			Attempts: 9, FirstFailedAt: &old, CreateIdempotencyKey: createKey, UpdatedAt: old,
		}))
		h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 503, Code: "unavailable"})

		link, err := h.f.File(t.Context(), sig, runCtx())
		require.ErrorIs(t, err, filing.ErrRecordedKataFailure)
		assert.Equal(t, db.FrictionLinkStateFailed, link.State)
		assert.Equal(t, 1, link.Attempts)
		require.NotNil(t, link.FirstFailedAt)
		assert.Equal(t, testNow, *link.FirstFailedAt)
		assert.Equal(t, createKey, link.CreateIdempotencyKey)
		posts := h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues")
		require.Len(t, posts, 1)
		assert.Equal(t, createKey, posts[0].IdempotencyKey)
	})

	t.Run("recurrence keeps its linked issue", func(t *testing.T) {
		h := newHarness(t)
		sig := errSig("claude:abandoned-recurrence", "Bash", "write failed")
		issue := closedMatch(h, sig, "done")
		require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
			Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateAbandoned,
			IssueUID: issue.UID, QualifiedID: "agentsview#" + issue.ShortID,
			LinkSource: db.FrictionLinkSourceCreated, LastRecurrenceDate: "2026-09-19",
			Attempts: 9, FirstFailedAt: &old, UpdatedAt: old,
		}))
		h.kata.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})

		link, err := h.f.File(t.Context(), sig, runCtx())
		require.ErrorIs(t, err, filing.ErrRecordedKataFailure)
		assert.Equal(t, db.FrictionLinkStateFailed, link.State)
		assert.Equal(t, 1, link.Attempts)
		require.NotNil(t, link.FirstFailedAt)
		assert.Equal(t, testNow, *link.FirstFailedAt)
		assert.Equal(t, issue.UID, link.IssueUID)
		assert.Equal(t, "agentsview#"+issue.ShortID, link.QualifiedID)
	})
}

func TestExplicitRetryOfAbandonedManualLinkKeepsIssue(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:abandoned-manual", "Bash", "write failed")
	issue := h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: "open"})
	projectUID, err := h.f.Kata.ProjectUID(t.Context())
	require.NoError(t, err)
	old := testNow.Add(-15 * 24 * time.Hour)
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateAbandoned,
		IssueUID: issue.UID, QualifiedID: "agentsview#" + issue.ShortID,
		LinkSource: "manual", KataInstanceUID: h.f.Kata.InstanceUID(), KataProjectUID: projectUID,
		LastRecurrenceDate: "2026-09-19", Attempts: 9, FirstFailedAt: &old, UpdatedAt: old,
	}))
	h.kata.ResetRequests()

	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Equal(t, issue.UID, link.IssueUID)
	assert.Equal(t, "manual", link.LinkSource)
	assert.Zero(t, link.Attempts)
	require.Nil(t, link.FirstFailedAt)
	assert.Empty(t, h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"))
}

func TestFilingResolvesChangedDestinationBeforeReusingIssue(t *testing.T) {
	sig := errSig("claude:destination-change", "Bash", "configuration not found")
	old := testNow.Add(-time.Hour)
	newIssue := kata.Issue{UID: "new-destination-issue", QualifiedID: "new-project#new", Status: "open"}
	tests := []struct {
		name         string
		instanceUID  string
		projectUID   string
		matches      []kata.Issue
		wantState    string
		wantIssue    string
		wantCode     string
		wantInstance string
		wantProject  string
	}{
		{name: "project change resolves metadata match", instanceUID: "old-instance", projectUID: "new-project", matches: []kata.Issue{newIssue}, wantState: db.FrictionLinkStateLinked, wantIssue: newIssue.UID, wantInstance: "old-instance", wantProject: "new-project"},
		{name: "instance change without a match asks for human", instanceUID: "new-instance", projectUID: "old-project", wantState: db.FrictionLinkStateNeedsHuman, wantIssue: "old-destination-issue", wantCode: "destination_changed", wantInstance: "old-instance", wantProject: "old-project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
				Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateLinked,
				IssueUID: "old-destination-issue", QualifiedID: "old-project#old",
				KataInstanceUID: "old-instance", KataProjectUID: "old-project",
				LinkSource: "manual", LastRecurrenceDate: "2026-09-20", UpdatedAt: old,
			}))
			api := &destinationOverride{
				KataAPI: h.f.Kata, instanceUID: tt.instanceUID, projectUID: tt.projectUID, matches: tt.matches,
			}
			h.f.Kata = api

			link, err := h.f.File(t.Context(), sig, runCtx())
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, link.State)
			assert.Equal(t, tt.wantIssue, link.IssueUID)
			assert.Equal(t, 1, api.findCalls)
			assert.Equal(t, tt.wantInstance, link.KataInstanceUID)
			assert.Equal(t, tt.wantProject, link.KataProjectUID)
			if tt.wantState != db.FrictionLinkStateLinked {
				assert.Equal(t, tt.wantCode, link.LastErrorCode)
			}
			assert.Empty(t, h.kata.Requests(), "a changed destination must not query the stale issue or create a replacement")
		})
	}
}

func TestRepeatedUnavailableRecurrencePreservesRetryWindow(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:recurrence-outage", "Bash", "configuration not found")
	firstFailed := testNow.Add(-15 * 24 * time.Hour)
	nextAttempt := testNow.Add(time.Hour)
	row := db.FrictionIssueLink{
		Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateFailed,
		IssueUID: "linked-issue", QualifiedID: "agentsview#linked",
		LastRecurrenceDate: "2026-09-20", Attempts: 4,
		FirstFailedAt: &firstFailed, NextAttemptAt: &nextAttempt,
		LastErrorCode: "transport", LastError: "stored outage detail", UpdatedAt: testNow,
	}
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), row))
	baseKata := h.f.Kata
	h.f.Kata = readyOverride{KataAPI: baseKata, ready: false}
	for _, date := range []string{"2026-09-21", "2026-09-22"} {
		if date == "2026-09-22" {
			h.f.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		}
		_, err := h.f.FileAll(t.Context(), []friction.Signal{sig}, filing.RunContext{Date: date})
		require.NoError(t, err)
	}
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	queued := links[sig.Fingerprint()]
	assert.Equal(t, row.Attempts, queued.Attempts)
	assert.Equal(t, row.FirstFailedAt, queued.FirstFailedAt)
	assert.Equal(t, row.NextAttemptAt, queued.NextAttemptAt)
	assert.Equal(t, row.LastErrorCode, queued.LastErrorCode)
	assert.Equal(t, row.LastError, queued.LastError)

	h.f.Kata = readyOverride{KataAPI: baseKata, ready: true}
	_, err = h.f.Drain(t.Context())
	require.NoError(t, err)
	links, err = h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateAbandoned, links[sig.Fingerprint()].State,
		"the original failure window must still reach the retry cap during a sustained outage")
}

func TestDrainRetriesFailedLinkedIssueOnSameDigestDate(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:failed-linked-retry", "Bash", "configuration not found")
	date := runCtx().Date
	issue := h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: "open"})
	projectUID, err := h.f.Kata.ProjectUID(t.Context())
	require.NoError(t, err)
	firstFailed := testNow.Add(-time.Hour)
	nextAttempt := testNow.Add(-time.Second)
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateFailed,
		IssueUID: issue.UID, QualifiedID: "agentsview#" + issue.ShortID,
		LinkSource: db.FrictionLinkSourceCreated, KataInstanceUID: h.f.Kata.InstanceUID(),
		KataProjectUID: projectUID, LastRecurrenceDate: date, Attempts: 1,
		FirstFailedAt: &firstFailed, NextAttemptAt: &nextAttempt,
		LastErrorCode: "transport", LastError: "temporary destination lookup failure", UpdatedAt: nextAttempt,
	}))
	h.kata.ResetRequests()
	h.snapshots[date] = friction.DigestSnapshot{Date: date, Signals: []friction.Signal{sig}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{date}}

	_, err = h.f.Drain(t.Context())
	require.NoError(t, err)
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	link := links[sig.Fingerprint()]
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Nil(t, link.NextAttemptAt)
	assert.Empty(t, link.LastErrorCode)
	assert.Len(t, h.kata.RequestsMatching(http.MethodGet, "/api/v1/issues/"+issue.UID), 1)
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

func TestManualFingerprintSelectionSkipsUnavailableFrozenOccurrences(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	live := errSig("claude:live", "Bash", "same finding")
	trashed := errSig("claude:trashed-frozen", "Bash", "same finding")
	missing := errSig("claude:missing-frozen", "Bash", "same finding")
	require.Equal(t, live.Fingerprint(), trashed.Fingerprint())
	require.Equal(t, live.Fingerprint(), missing.Fingerprint())
	dbtest.SeedSession(t, h.db, live.SubjectID, "example")
	dbtest.SeedSession(t, h.db, trashed.SubjectID, "example")
	require.NoError(t, h.db.SoftDeleteSession(t.Context(), trashed.SubjectID))
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{live}}
	h.snapshots["2026-09-21"] = friction.DigestSnapshot{Date: "2026-09-21", Signals: []friction.Signal{trashed, missing}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20", "2026-09-21"}}

	got, run, err := h.f.SignalForFingerprint(t.Context(), live.Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, live.SubjectID, got.SubjectID)
	assert.Equal(t, "2026-09-20", run.Date)
}

func TestManualLatestStoredFindingKeepsNewestDigestLink(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	sig := errSig("claude:current", "Bash", "same finding")
	sig.OccurredAt = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	dbtest.SeedSession(t, h.db, sig.SubjectID, "example")
	require.NoError(t, h.db.ReplaceSessionFriction(t.Context(), sig.SubjectID,
		[]db.FrictionFinding{{
			SessionID: sig.SubjectID, Kind: string(sig.Kind), Detector: sig.Detector,
			ToolName: sig.ToolName, Text: sig.Text, MessageOrdinal: sig.Ordinal,
			OccurredAt: &sig.OccurredAt, Fingerprint: sig.Fingerprint(),
		}}, nil, friction.RulesVersion, "synthetic"))
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{
		Date: "2026-09-20", Signals: []friction.Signal{sig},
	}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20"}}

	got, run, err := h.f.SignalForFingerprint(t.Context(), sig.Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, sig.Fingerprint(), got.Fingerprint())
	assert.Equal(t, "2026-09-21", run.Date,
		"manual filing must use the latest stored occurrence")
	assert.Equal(t, filing.DigestURL(h.f.PublicURL, "2026-09-20"), run.DigestURL,
		"the source link should point to the newest frozen digest containing this fingerprint")
	assert.Contains(t, filing.Body(got, run), "- Digest: "+run.DigestURL)
}

func TestManualFingerprintOnDateSkipsUnavailableFrozenOccurrences(t *testing.T) {
	h := newHarness(t)
	h.f.Archive = h.db
	live := errSig("claude:live-on-date", "Bash", "same finding")
	trashed := errSig("claude:trashed-on-date", "Bash", "same finding")
	missing := errSig("claude:missing-on-date", "Bash", "same finding")
	require.Equal(t, live.Fingerprint(), trashed.Fingerprint())
	require.Equal(t, live.Fingerprint(), missing.Fingerprint())
	dbtest.SeedSession(t, h.db, live.SubjectID, "example")
	dbtest.SeedSession(t, h.db, trashed.SubjectID, "example")
	require.NoError(t, h.db.SoftDeleteSession(t.Context(), trashed.SubjectID))
	h.snapshots["2026-09-21"] = friction.DigestSnapshot{Date: "2026-09-21", Signals: []friction.Signal{trashed, missing, live}}

	got, run, err := h.f.SignalForFingerprintOnDate(t.Context(), live.Fingerprint(), "2026-09-21")
	require.NoError(t, err)
	assert.Equal(t, live.SubjectID, got.SubjectID)
	assert.Equal(t, "2026-09-21", run.Date)
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

func TestLinkedRecurrenceQueuesWhileKataIsUnavailable(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:recurrence", "Bash", "configuration not found")
	issue := closedMatch(h, sig, "done")
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateLinked,
		IssueUID: issue.UID, QualifiedID: "agentsview#" + issue.ShortID,
		LinkSource: db.FrictionLinkSourceCreated, LastRecurrenceDate: "2026-09-20", UpdatedAt: testNow,
	}))
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}
	h.snapshots["2026-09-21"] = friction.DigestSnapshot{Date: "2026-09-21", Signals: []friction.Signal{sig}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20", "2026-09-21"}}
	kataAPI := h.f.Kata
	h.f.Kata = readyOverride{KataAPI: kataAPI, ready: false}
	h.kata.ResetRequests()

	_, err := h.f.FileAll(t.Context(), []friction.Signal{sig}, filing.RunContext{Date: "2026-09-21"})
	require.NoError(t, err)
	links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	queued := links[sig.Fingerprint()]
	assert.Equal(t, db.FrictionLinkStateFailed, queued.State)
	assert.Equal(t, issue.UID, queued.IssueUID, "queueing must preserve the linked issue")
	require.NotNil(t, queued.NextAttemptAt)
	assert.Equal(t, testNow, *queued.NextAttemptAt)
	due, err := h.db.DueFrictionFilings(t.Context(), testNow, 50)
	require.NoError(t, err)
	assert.Len(t, due, 1)
	refs, err := h.f.Links(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, "#"+issue.ShortID, refs[sig.Fingerprint()].ID, "the existing annotation remains available during the retry")
	assert.Empty(t, h.kata.Requests(), "an unavailable Kata must not receive requests")

	h.f.Kata = readyOverride{KataAPI: kataAPI, ready: true}
	report, err := h.f.Drain(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, report.Reopened)
	links, err = h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, links[sig.Fingerprint()].State)
	assert.Equal(t, issue.UID, links[sig.Fingerprint()].IssueUID)
	assert.Equal(t, "2026-09-21", links[sig.Fingerprint()].LastRecurrenceDate)
	assert.Nil(t, links[sig.Fingerprint()].NextAttemptAt)
	updated, _ := h.kata.Issue(issue.UID)
	assert.Equal(t, "open", updated.Status)
	assert.Equal(t, []string{
		"GET /api/v1/issues/" + issue.UID,
		"POST /api/v1/projects/17/issues/" + issue.UID + "/actions/reopen",
		"POST /api/v1/projects/17/issues/" + issue.UID + "/comments",
		"POST /api/v1/projects/17/issues/" + issue.UID + "/labels",
	}, paths(h.kata.Requests()))
}
