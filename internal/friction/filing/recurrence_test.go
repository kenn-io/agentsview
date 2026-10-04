package filing_test

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func TestReopenAllowed(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"done", true},
		{"", false},
		{"wontfix", false},
		{"duplicate", false},
		{"superseded", false},
		{"audit-no-change", false},
		{"anything-else", false},
		{"Done", false},
	}
	for _, tt := range tests {
		t.Run("reopen_allowed_only_for_done/"+tt.reason, func(t *testing.T) {
			assert.Equal(t, tt.want, filing.ReopenAllowed(tt.reason))
		})
	}
}

func TestRecurrenceCommentAndKey(t *testing.T) {
	assert.Equal(t, "friction-recur-01J0ABCDEF0000000000000001-2026-09-03", filing.RecurrenceCommentKey("01J0ABCDEF0000000000000001", "2026-09-03"))
	tests := []struct {
		name, url, want string
	}{
		{name: "no_public_url", want: "Recurred on 2026-09-03 — closure may have been premature."},
		{
			name: "with_session_url", url: "https://av.example.test/sessions/claude/s1?msg=3",
			want: "Recurred on 2026-09-03 — closure may have been premature.\nSession: https://av.example.test/sessions/claude/s1?msg=3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, filing.RecurrenceComment("2026-09-03", tt.url)) })
	}
}

func closedMatch(h *harness, sig friction.Signal, reason string) katatest.Issue {
	return h.kata.AddIssue(katatest.Issue{
		Title: sig.Title(), Status: "closed", ClosedReason: reason,
		Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
	})
}

func TestReopenPathForDoneMatch(t *testing.T) {
	// create_takes_reopen_path_for_done_match (trackers/kata.rs:1223-1242).
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "done me")
	is := closedMatch(h, sig, "done")
	h.kata.ResetRequests()

	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Equal(t, is.UID, link.IssueUID)
	assert.Equal(t, "2026-09-20", link.LastRecurrenceDate)
	assert.Equal(t, []string{
		"GET /api/v1/projects/17/issues",
		"POST /api/v1/projects/17/issues/" + is.UID + "/actions/reopen",
		"POST /api/v1/projects/17/issues/" + is.UID + "/comments",
		"POST /api/v1/projects/17/issues/" + is.UID + "/labels",
	}, paths(h.kata.Requests()), "done path must never file a new issue")
	reopens := h.kata.RequestsMatching(http.MethodPost, "/actions/reopen")
	require.Len(t, reopens, 1)
	reopen := reopens[0]
	assert.JSONEq(t, `{"actor":"agentsview"}`, string(reopen.Body))
	comments := h.kata.RequestsMatching(http.MethodPost, "/comments")
	require.Len(t, comments, 1)
	comment := comments[0]
	assert.Equal(t, "friction-recur-"+is.UID+"-2026-09-20", comment.IdempotencyKey)
	assert.JSONEq(t, `{"actor":"agentsview","body":"Recurred on 2026-09-20 — closure may have been premature.\nSession: https://av.example.test/sessions/claude/s1?msg=3"}`, string(comment.Body))
	labels := h.kata.RequestsMatching(http.MethodPost, "/labels")
	require.Len(t, labels, 1)
	assert.JSONEq(t, `{"actor":"agentsview","label":"friction:recurred"}`, string(labels[0].Body))
	after, _ := h.kata.Issue(is.UID)
	assert.Equal(t, "open", after.Status)
}

func TestReopenReasonGate(t *testing.T) {
	// create_returns_wontfix_match_without_reopen: every non-done reason and an
	// absent reason link and mutate nothing.
	for _, reason := range []string{"wontfix", "duplicate", "superseded", "audit-no-change", ""} {
		name := reason
		if name == "" {
			name = "missing_reason_never_reopens"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			sig := errSig("claude:s1", "Bash", "decided")
			is := closedMatch(h, sig, reason)
			h.kata.ResetRequests()
			link, err := h.f.File(t.Context(), sig, runCtx())
			require.NoError(t, err)
			assert.Equal(t, db.FrictionLinkStateLinked, link.State)
			assert.Equal(t, is.UID, link.IssueUID)
			assert.Equal(t, []string{"GET /api/v1/projects/17/issues"}, paths(h.kata.Requests()))
		})
	}
}

func TestReopenDisabledByPolicy(t *testing.T) {
	h := newHarness(t)
	h.f.Policy.ReopenOnRecurrence = false
	sig := errSig("claude:s1", "Bash", "done me")
	closedMatch(h, sig, "done")
	h.kata.ResetRequests()
	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Empty(t, h.kata.RequestsMatching(http.MethodPost, "/actions/reopen"))
}

func TestReopenFailureIsLoud(t *testing.T) {
	// create_reopen_failure_is_loud_for_done_match (trackers/kata.rs:1244-1254).
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "done but daemon down")
	is := closedMatch(h, sig, "done")
	h.kata.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable", Message: "down"})
	h.kata.ResetRequests()

	link, err := h.f.File(t.Context(), sig, runCtx())
	require.Error(t, err, "a failed reopen must surface")
	assert.Equal(t, db.FrictionLinkStateFailed, link.State)
	assert.Equal(t, is.UID, link.IssueUID, "the issue stays known so the retry reopens it")
	assert.Empty(t, link.LastRecurrenceDate, "not handled for this date yet")
	assert.Equal(t, []string{
		"GET /api/v1/projects/17/issues",
		"POST /api/v1/projects/17/issues/" + is.UID + "/actions/reopen",
	}, paths(h.kata.Requests()), "stops at the failed reopen")
}

func TestReopenAnnotationFailuresWarnAndKeepLink(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "done me")
	closedMatch(h, sig, "done")
	h.kata.Fail(http.MethodPost, "/comments", katatest.Fault{Status: 500, Code: "internal", Message: "m"})
	h.kata.Fail(http.MethodPost, "/labels", katatest.Fault{Status: 500, Code: "internal", Message: "m"})

	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Equal(t, "2026-09-20", link.LastRecurrenceDate)
	assert.Contains(t, buf.String(), "recurrence comment")
	assert.Contains(t, buf.String(), "issue reopened anyway")
}

func TestRecurrenceOnLinkedFingerprint(t *testing.T) {
	sig := errSig("claude:s9", "Bash", "again")
	tests := []struct {
		name       string
		status     string
		reason     string
		lrd        string
		wantPaths  func(uid string) []string
		wantLRD    string
		wantReopen bool
	}{
		{
			name: "closed_done_on_new_date_reopens", status: "closed", reason: "done", lrd: "2026-09-19", wantReopen: true, wantLRD: "2026-09-20",
			wantPaths: func(uid string) []string {
				return []string{
					"GET /api/v1/issues/" + uid, "POST /api/v1/projects/17/issues/" + uid + "/actions/reopen",
					"POST /api/v1/projects/17/issues/" + uid + "/comments", "POST /api/v1/projects/17/issues/" + uid + "/labels",
				}
			},
		},
		{
			name: "closed_wontfix_on_new_date_only_records_the_check", status: "closed", reason: "wontfix", lrd: "2026-09-19", wantLRD: "2026-09-20",
			wantPaths: func(uid string) []string { return []string{"GET /api/v1/issues/" + uid} },
		},
		{
			name: "open_on_new_date_only_records_the_check", status: "open", lrd: "2026-09-19", wantLRD: "2026-09-20",
			wantPaths: func(uid string) []string { return []string{"GET /api/v1/issues/" + uid} },
		},
		{
			name: "same_date_makes_no_calls", status: "closed", reason: "done", lrd: "2026-09-20", wantLRD: "2026-09-20",
			wantPaths: func(string) []string { return nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			is := h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: tt.status, ClosedReason: tt.reason})
			require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
				Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateLinked,
				IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, LinkSource: db.FrictionLinkSourceCreated, LastRecurrenceDate: tt.lrd, UpdatedAt: testNow,
			}))
			h.kata.ResetRequests()
			rep, err := h.f.FileAll(t.Context(), []friction.Signal{sig}, runCtx())
			require.NoError(t, err)
			assert.Equal(t, tt.wantPaths(is.UID), pathsOrNil(h.kata.Requests()))
			got, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
			require.NoError(t, err)
			assert.Equal(t, tt.wantLRD, got[sig.Fingerprint()].LastRecurrenceDate)
			assert.Equal(t, db.FrictionLinkSourceCreated, got[sig.Fingerprint()].LinkSource, "recurrence keeps the original link source")
			if tt.wantReopen {
				assert.Equal(t, 1, rep.Reopened)
			} else {
				assert.Equal(t, 0, rep.Reopened)
			}
		})
	}
}

func pathsOrNil(reqs []katatest.Request) []string {
	if len(reqs) == 0 {
		return nil
	}
	return paths(reqs)
}

func TestReopenOncePerDateAcrossRetry(t *testing.T) {
	t.Run("reopen_once_per_date_across_retry", func(t *testing.T) {
		h := newHarness(t)
		sig := errSig("claude:s1", "Bash", "done me")
		is := closedMatch(h, sig, "done")
		h.kata.Fail(http.MethodPost, "/comments", katatest.Fault{Status: 504, Code: "timeout", Message: "m"})
		_, err := h.f.File(t.Context(), sig, runCtx())
		require.NoError(t, err)
		_, err = h.f.File(t.Context(), sig, runCtx())
		require.NoError(t, err)
		assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/actions/reopen"), 1)
		assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/comments"), 1, "no second comment on the same date")
		after, _ := h.kata.Issue(is.UID)
		assert.Equal(t, "open", after.Status)
	})
	t.Run("failed_reopen_retries_then_succeeds_once", func(t *testing.T) {
		h := newHarness(t)
		sig := errSig("claude:s1", "Bash", "done me")
		is := closedMatch(h, sig, "done")
		h.kata.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable", Message: "m"})
		_, err := h.f.File(t.Context(), sig, runCtx())
		require.Error(t, err)
		link, err := h.f.File(t.Context(), sig, runCtx())
		require.NoError(t, err)
		assert.Equal(t, db.FrictionLinkStateLinked, link.State)
		assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/actions/reopen"), 2, "one failed, one succeeded")
		assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/comments"), 1)
		after, _ := h.kata.Issue(is.UID)
		assert.Equal(t, []string{"Recurred on 2026-09-20 — closure may have been premature.\nSession: https://av.example.test/sessions/claude/s1?msg=3"}, after.Comments)
	})
}

func TestDrainRetriesFailedReopenForLatestDate(t *testing.T) {
	t.Run("drain_retries_failed_reopen_for_latest_date", func(t *testing.T) {
		h := newHarness(t)
		sig := errSig("claude:s1", "Bash", "done me")
		is := h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: "closed", ClosedReason: "done"})
		h.snapshots["2026-09-19"] = friction.DigestSnapshot{Date: "2026-09-19", Signals: []friction.Signal{sig}}
		h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}
		h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-19", "2026-09-20"}}
		past := testNow.Add(-time.Minute)
		require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
			Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateFailed,
			IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, LinkSource: db.FrictionLinkSourceCreated, Attempts: 1,
			FirstFailedAt: &past, NextAttemptAt: &past, LastRecurrenceDate: "2026-09-19", UpdatedAt: testNow,
		}))

		rep, err := h.f.Drain(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Reopened)
		got, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
		require.NoError(t, err)
		assert.Equal(t, db.FrictionLinkStateLinked, got[sig.Fingerprint()].State)
		assert.Equal(t, "2026-09-20", got[sig.Fingerprint()].LastRecurrenceDate)
		comments := h.kata.RequestsMatching(http.MethodPost, "/comments")
		require.Len(t, comments, 1)
		assert.Equal(t, "friction-recur-"+is.UID+"-2026-09-20", comments[0].IdempotencyKey)
	})
}
