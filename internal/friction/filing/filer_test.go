// internal/friction/filing/filer_test.go
package filing_test

import (
	"context"
	"encoding/json/v2"
	"errors"
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

var testNow = time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)

type harness struct {
	f         *filing.Filer
	kata      *katatest.Server
	db        *db.DB
	rerenders []string
	snapshots map[string]friction.DigestSnapshot
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := katatest.New(t)
	s.SetWebOrigin("https://kata.example.test")
	h := &harness{kata: s, db: dbtest.OpenTestDB(t), snapshots: map[string]friction.DigestSnapshot{}}
	now := testNow
	h.f = &filing.Filer{
		Kata:      kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview", Actor: "agentsview"}),
		Store:     h.db,
		Redact:    filing.DefaultRedact,
		Now:       func() time.Time { return now },
		Policy:    filing.Policy{Kinds: filing.DefaultKinds(), ReopenOnRecurrence: true, Actor: "agentsview"},
		Instance:  "inst-test",
		PublicURL: "https://av.example.test",
		Snapshot: func(_ context.Context, date string) (friction.DigestSnapshot, error) {
			snap, ok := h.snapshots[date]
			if !ok {
				return friction.DigestSnapshot{}, errors.New("no digest")
			}
			return snap, nil
		},
		Rerender: func(_ context.Context, date string) error { h.rerenders = append(h.rerenders, date); return nil },
	}
	require.True(t, h.f.Ready(t.Context()))
	s.ResetRequests()
	return h
}

func errSig(subject, tool, msg string) friction.Signal {
	return friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: subject, Detector: "error", ToolName: tool, Text: msg, Ordinal: new(3)}
}

func runCtx() filing.RunContext {
	return filing.RunContext{Date: "2026-09-20", PublicURL: "https://av.example.test", DigestURL: filing.DigestURL("https://av.example.test", "2026-09-20")}
}

func paths(reqs []katatest.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func TestFileRequestSequences(t *testing.T) {
	sig := errSig("claude:s1", "Bash", "boom")
	fp := sig.Fingerprint()
	tests := []struct {
		name      string
		setup     func(h *harness)
		run       func() filing.RunContext
		sig       friction.Signal
		wantState string
		wantSrc   string
		wantPaths []string
		check     func(t *testing.T, h *harness, link db.FrictionIssueLink)
	}{
		{
			name: "create_returns_open_match_first",
			setup: func(h *harness) {
				h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: "open", Metadata: map[string]any{"friction.fingerprint": fp}})
			},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceFound,
			wantPaths: []string{"GET /api/v1/projects/17/issues"},
		},
		{
			name: "create_returns_wontfix_match_without_reopen",
			setup: func(h *harness) {
				h.kata.AddIssue(katatest.Issue{Title: sig.Title(), Status: "closed", ClosedReason: "wontfix", Metadata: map[string]any{"friction.fingerprint": fp}})
			},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceFound,
			wantPaths: []string{"GET /api/v1/projects/17/issues"},
		},
		{
			name:      "new_create_with_idempotency_key",
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceCreated,
			wantPaths: []string{"GET /api/v1/projects/17/issues", "POST /api/v1/projects/17/issues", "GET /api/v1/issues/01J0ABCDEF0000000000000001"},
			check: func(t *testing.T, h *harness, link db.FrictionIssueLink) {
				t.Helper()
				post := h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues")[0]
				assert.Equal(t, filing.IdempotencyKey(filing.DefaultRedact(sig.Title())), post.IdempotencyKey)
				var body map[string]any
				require.NoError(t, json.Unmarshal(post.Body, &body))
				assert.Equal(t, "agentsview", body["actor"])
				assert.Equal(t, sig.Title(), body["title"])
				assert.InDelta(t, 3, body["priority"], 0)
				assert.Equal(t, []any{"friction", "friction:error"}, body["labels"])
				assert.NotContains(t, body, "force_new")
				meta := body["metadata"].(map[string]any)
				assert.Equal(t, fp, meta["friction.fingerprint"])
				assert.Equal(t, "https://av.example.test/sessions/claude/s1?msg=3", meta["agentsview.session_url"])
				assert.Equal(t, "inst-test", meta["agentsview.instance"])
				assert.Equal(t, "agentsview#f001", link.QualifiedID)
				assert.Equal(t, "https://kata.example.test/issues/01J0ABCDEF0000000000000001", link.WebURL)
				assert.Equal(t, "01J00000000000000000000002", link.KataInstanceUID)
				assert.Equal(t, "2026-09-20", link.LastRecurrenceDate)
			},
		},
		{
			name: "idempotency_mismatch_links_prior_uid",
			setup: func(h *harness) {
				prior := h.kata.AddIssue(katatest.Issue{Title: "older body", Status: "open"})
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{
					Status: 409, Code: "idempotency_mismatch", Message: "m",
					Data: map[string]any{"uid": prior.UID, "short_id": prior.ShortID, "qualified_id": "agentsview#" + prior.ShortID},
				})
			},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceIdempotentReuse,
			wantPaths: []string{"GET /api/v1/projects/17/issues", "POST /api/v1/projects/17/issues", "GET /api/v1/issues/01J0ABCDEF0000000000000001"},
		},
		{
			name: "idempotency_deleted_needs_human",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 409, Code: "idempotency_deleted", Message: "m"})
			},
			wantState: db.FrictionLinkStateNeedsHuman,
			wantPaths: []string{"GET /api/v1/projects/17/issues", "POST /api/v1/projects/17/issues"},
			check: func(t *testing.T, _ *harness, link db.FrictionIssueLink) {
				t.Helper()
				assert.Equal(t, "idempotency_deleted", link.LastErrorCode)
				assert.Nil(t, link.NextAttemptAt, "needs_human is never retried automatically")
			},
		},
		{
			name: "duplicate_candidates_needs_human_with_candidates",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{
					Status: 409, Code: "duplicate_candidates", Message: "m",
					Data: map[string]any{"candidates": []any{map[string]any{"uid": "01J0CAND000000000000000001", "short_id": "c1", "qualified_id": "agentsview#c1", "title": "near", "score": 0.8}}},
				})
			},
			wantState: db.FrictionLinkStateNeedsHuman,
			check: func(t *testing.T, _ *harness, link db.FrictionIssueLink) {
				t.Helper()
				assert.Contains(t, link.CandidatesJSON, "01J0CAND000000000000000001")
			},
		},
		{
			name: "federated_read_only_needs_human",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 409, Code: "federated_read_only", Message: "replica"})
			},
			wantState: db.FrictionLinkStateNeedsHuman,
		},
		{
			name: "bootstrap_token_write_forbidden_needs_human",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 403, Code: "bootstrap_token_write_forbidden", Message: "m"})
			},
			wantState: db.FrictionLinkStateNeedsHuman,
		},
		{
			name: "server_error_fails_with_backoff",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 503, Code: "unavailable", Message: "m"})
			},
			wantState: db.FrictionLinkStateFailed,
			check: func(t *testing.T, _ *harness, link db.FrictionIssueLink) {
				t.Helper()
				assert.Equal(t, 1, link.Attempts)
				require.NotNil(t, link.FirstFailedAt)
				require.NotNil(t, link.NextAttemptAt)
				assert.Equal(t, testNow.Add(filing.Backoff(1, fp)).Truncate(time.Second), *link.NextAttemptAt)
			},
		},
		{
			name: "drift_is_failed_not_no_match",
			setup: func(h *harness) {
				h.kata.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 200, RawBody: `{"items":[]}`})
			},
			wantState: db.FrictionLinkStateFailed,
			wantPaths: []string{"GET /api/v1/projects/17/issues"},
			check: func(t *testing.T, _ *harness, link db.FrictionIssueLink) {
				t.Helper()
				assert.Equal(t, "invalid_response", link.LastErrorCode)
			},
		},
		{
			name: "two_metadata_matches_need_human",
			setup: func(h *harness) {
				h.kata.AddIssue(katatest.Issue{Title: "a", Metadata: map[string]any{"friction.fingerprint": fp}})
				h.kata.AddIssue(katatest.Issue{Title: "b", Metadata: map[string]any{"friction.fingerprint": fp}})
			},
			wantState: db.FrictionLinkStateNeedsHuman,
			wantPaths: []string{"GET /api/v1/projects/17/issues"},
			check: func(t *testing.T, _ *harness, link db.FrictionIssueLink) {
				t.Helper()
				assert.Equal(t, "multiple_matches", link.LastErrorCode)
				assert.Contains(t, link.CandidatesJSON, "agentsview#f001")
				assert.Contains(t, link.CandidatesJSON, "agentsview#f002")
			},
		},
		{
			name: "jilog_title_is_not_matched",
			setup: func(h *harness) {
				h.kata.AddIssue(katatest.Issue{Title: "[jilog/error] Bash: boom", Status: "open"})
			},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceCreated,
		},
		{
			name:      "diagnostic_subject_sends_force_new",
			sig:       friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectDiagnostic, SubjectID: "ci:build-17", ToolName: "ci", Text: "ci: build 17 failed"},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceCreated,
			check: func(t *testing.T, h *harness, _ db.FrictionIssueLink) {
				t.Helper()
				var body map[string]any
				require.NoError(t, json.Unmarshal(h.kata.RequestsMatching(http.MethodPost, "/issues")[0].Body, &body))
				assert.Equal(t, true, body["force_new"])
			},
		},
		{
			name: "explicit_force_new_skips_lookup",
			run: func() filing.RunContext {
				r := runCtx()
				r.ForceNew = true
				return r
			},
			wantState: db.FrictionLinkStateLinked, wantSrc: db.FrictionLinkSourceCreated,
			wantPaths: []string{"POST /api/v1/projects/17/issues", "GET /api/v1/issues/01J0ABCDEF0000000000000001"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.setup != nil {
				tt.setup(h)
				h.kata.ResetRequests()
			}
			s := sig
			if tt.sig.Kind != "" {
				s = tt.sig
			}
			rc := runCtx()
			if tt.run != nil {
				rc = tt.run()
			}
			link, err := h.f.File(t.Context(), s, rc)
			if tt.wantState == db.FrictionLinkStateFailed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, link.State, link.LastError)
			assert.Equal(t, tt.wantSrc, link.LinkSource)
			if tt.wantPaths != nil {
				assert.Equal(t, tt.wantPaths, paths(h.kata.Requests()))
			}
			stored, err := h.db.GetFrictionIssueLinks(t.Context(), []string{s.Fingerprint()})
			require.NoError(t, err)
			assert.Equal(t, link, stored[s.Fingerprint()])
			if tt.check != nil {
				tt.check(t, h, link)
			}
		})
	}
}

func TestFileIsIdempotentAcrossCalls(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "boom")
	first, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	h.kata.ResetRequests()
	second, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, first.IssueUID, second.IssueUID)
	assert.Empty(t, h.kata.Requests(), "a linked fingerprint makes no Kata calls on the same date")
}

func TestFileNeedsHumanIsSticky(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "boom")
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateNeedsHuman, UpdatedAt: testNow}))
	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateNeedsHuman, link.State)
	assert.Empty(t, h.kata.Requests())
}

func TestFileSecretInErrorIsRedactedEverywhere(t *testing.T) {
	h := newHarness(t)
	fixtureKey := "AKIA" + "7QHWN2DKR4FYPLJM"
	home := "/" + "Users" + "/example"
	sig := errSig("claude:s1", "Bash", "token "+fixtureKey+" rejected at "+home+"/app")
	h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 400, Code: "validation", Message: "bad " + fixtureKey})
	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateNeedsHuman, link.State, "4xx validation will not fix itself")
	post := h.kata.RequestsMatching(http.MethodPost, "/issues")[0]
	for _, s := range []string{string(post.Body), post.IdempotencyKey, link.LastError} {
		assert.NotContains(t, s, fixtureKey)
		assert.NotContains(t, s, home)
	}
}

func TestFileCandidatesAreRedacted(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "boom")
	fixtureKey := "AKIA" + "7QHWN2DKR4FYPLJM"
	home := "/" + "Users" + "/example"
	first := h.kata.AddIssue(katatest.Issue{
		Title:    "token " + fixtureKey + " at " + home + "/app",
		Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
	})
	h.kata.AddIssue(katatest.Issue{
		Title:    "another candidate",
		Metadata: map[string]any{"friction.fingerprint": sig.Fingerprint()},
	})
	link, err := h.f.File(t.Context(), sig, runCtx())
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateNeedsHuman, link.State)
	assert.Contains(t, link.CandidatesJSON, first.UID)
	assert.NotContains(t, link.CandidatesJSON, fixtureKey)
	assert.NotContains(t, link.CandidatesJSON, home)
}

func TestFileAll(t *testing.T) {
	a := errSig("claude:s1", "Bash", "boom")
	aAgain := errSig("claude:s2", "Bash", "boom") // same title => same fingerprint
	b := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", Text: "no, the other file", Ordinal: new(1)}
	frus := friction.Signal{Kind: friction.KindFrustration, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", Text: "argh", Ordinal: new(2)}
	t.Run("repeated_fingerprint_files_once", func(t *testing.T) {
		h := newHarness(t)
		rep, err := h.f.FileAll(t.Context(), []friction.Signal{a, aAgain, b, frus}, runCtx())
		require.NoError(t, err)
		assert.Equal(t, 2, rep.Created, "frustration is not in the default kinds")
		assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), 2)
		require.Len(t, rep.Issues, 2)
		assert.Equal(t, filing.DefaultRedact(a.Title()), rep.Issues[0].Title)
		assert.Equal(t, "#f001", rep.Issues[0].ID)
		assert.Equal(t, "kata", rep.Issues[0].Backend)
	})
	t.Run("kata_down_writes_pending", func(t *testing.T) {
		h := newHarness(t)
		h.kata.Close()
		h.f.Kata = kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: h.kata.Endpoint(), Project: "agentsview"})
		rep, err := h.f.FileAll(t.Context(), []friction.Signal{a, b}, runCtx())
		require.NoError(t, err, "Kata down never fails a review")
		assert.Equal(t, filing.Report{}, rep)
		links, err := h.db.GetFrictionIssueLinks(t.Context(), []string{a.Fingerprint(), b.Fingerprint()})
		require.NoError(t, err)
		require.Len(t, links, 2)
		for _, l := range links {
			assert.Equal(t, db.FrictionLinkStatePending, l.State)
			require.NotNil(t, l.NextAttemptAt)
			assert.Equal(t, testNow, *l.NextAttemptAt)
		}
	})
	t.Run("failure_counts", func(t *testing.T) {
		h := newHarness(t)
		h.kata.Fail(http.MethodPost, "/api/v1/projects/17/issues", katatest.Fault{Status: 500, Code: "internal", Message: "m"})
		rep, err := h.f.FileAll(t.Context(), []friction.Signal{a, b}, runCtx())
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Failed)
		assert.Equal(t, 1, rep.Created)
	})
}

func TestLinkAndUnlink(t *testing.T) {
	h := newHarness(t)
	sig := errSig("claude:s1", "Bash", "boom")
	is := h.kata.AddIssue(katatest.Issue{Title: "hand filed", Status: "open"})
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStateNeedsHuman, UpdatedAt: testNow}))
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-19", "2026-09-20"}}

	link, err := h.f.Link(t.Context(), sig.Fingerprint(), "agentsview#"+is.ShortID)
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStateLinked, link.State)
	assert.Equal(t, db.FrictionLinkSourceManual, link.LinkSource)
	assert.Equal(t, is.UID, link.IssueUID)
	assert.Equal(t, []string{"2026-09-19", "2026-09-20"}, h.rerenders)

	h.rerenders = nil
	require.NoError(t, h.f.Unlink(t.Context(), sig.Fingerprint()))
	got, err := h.db.GetFrictionIssueLinks(t.Context(), []string{sig.Fingerprint()})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, []string{"2026-09-19", "2026-09-20"}, h.rerenders)
	_, ok := h.kata.Issue(is.UID)
	assert.True(t, ok, "unlink never touches Kata")
}

type datedStore struct {
	filing.LinkStore
	dates []string
}

func (d datedStore) DigestDatesForFingerprints(context.Context, []string) ([]string, error) {
	return d.dates, nil
}

func TestLinksAndSnapshotOpen(t *testing.T) {
	h := newHarness(t)
	open := h.kata.AddIssue(katatest.Issue{Title: "o", Status: "open"})
	closed := h.kata.AddIssue(katatest.Issue{Title: "c", Status: "closed", ClosedReason: "done"})
	for fp, is := range map[string]katatest.Issue{"fl1:open": open, "fl1:closed": closed} {
		require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{
			Fingerprint: fp, State: db.FrictionLinkStateLinked,
			IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, WebURL: "https://kata.example.test/issues/" + is.UID, UpdatedAt: testNow,
		}))
	}
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: "fl1:pending", State: db.FrictionLinkStatePending, UpdatedAt: testNow}))

	links, err := h.f.Links(t.Context(), []string{"fl1:open", "fl1:closed", "fl1:pending", "fl1:none"})
	require.NoError(t, err)
	assert.Equal(t, map[string]friction.IssueRef{
		"fl1:open":   {ID: "#" + open.ShortID, Backend: "kata", URL: "https://kata.example.test/issues/" + open.UID},
		"fl1:closed": {ID: "#" + closed.ShortID, Backend: "kata", URL: "https://kata.example.test/issues/" + closed.UID},
	}, links, "only linked rows annotate")

	snap, err := h.f.SnapshotOpen(t.Context(), []string{"fl1:open", "fl1:closed", "fl1:pending"})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"fl1:open": true}, snap)
}

func TestDrain(t *testing.T) {
	sig := errSig("claude:s1", "Bash", "boom")
	fp := sig.Fingerprint()
	old := testNow.Add(-15 * 24 * time.Hour)
	recent := testNow.Add(-time.Hour)
	tests := []struct {
		name         string
		row          db.FrictionIssueLink
		ready        bool
		wantState    string
		wantCreates  int
		wantRerender []string
	}{
		{
			name: "pending_links_and_rerenders", row: db.FrictionIssueLink{Fingerprint: fp, State: db.FrictionLinkStatePending, NextAttemptAt: &recent},
			ready: true, wantState: db.FrictionLinkStateLinked, wantCreates: 1, wantRerender: []string{"2026-09-20"},
		},
		{
			name: "failed_past_cap_is_abandoned", row: db.FrictionIssueLink{Fingerprint: fp, State: db.FrictionLinkStateFailed, Attempts: 9, FirstFailedAt: &old, NextAttemptAt: &recent},
			ready: true, wantState: db.FrictionLinkStateAbandoned,
		},
		{
			name: "pending_not_consumed_while_kata_unready", row: db.FrictionIssueLink{Fingerprint: fp, State: db.FrictionLinkStatePending, NextAttemptAt: &recent},
			ready: false, wantState: db.FrictionLinkStatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}
			h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20"}}
			tt.row.UpdatedAt = testNow
			require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), tt.row))
			if !tt.ready {
				h.kata.Close()
				h.f.Kata = kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: h.kata.Endpoint(), Project: "agentsview"})
			}
			_, err := h.f.Drain(t.Context())
			require.NoError(t, err)
			got, err := h.db.GetFrictionIssueLinks(t.Context(), []string{fp})
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, got[fp].State)
			if tt.ready {
				assert.Len(t, h.kata.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"), tt.wantCreates)
			}
			assert.Equal(t, tt.wantRerender, h.rerenders)
			if tt.wantState == db.FrictionLinkStatePending {
				assert.Equal(t, 0, got[fp].Attempts, "an unready drain changes nothing")
			}
		})
	}
}

func TestDrainBodyMatchesInlineFiling(t *testing.T) {
	// The drained create must send the byte-identical body an inline filing
	// would have, so a retry under the same idempotency key reuses the issue.
	sig := errSig("claude:s1", "Bash", "boom")
	sig.Dims = friction.Dims{Agent: "claude", Machine: "laptop-a"}
	h := newHarness(t)
	h.snapshots["2026-09-20"] = friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}
	h.f.Store = datedStore{LinkStore: h.db, dates: []string{"2026-09-20"}}
	inline := h.f.Plan(sig, runCtx())
	require.NoError(t, h.db.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), State: db.FrictionLinkStatePending, UpdatedAt: testNow}))
	_, err := h.f.Drain(t.Context())
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(h.kata.RequestsMatching(http.MethodPost, "/issues")[0].Body, &body))
	assert.Equal(t, inline.Body, body["body"])
}
