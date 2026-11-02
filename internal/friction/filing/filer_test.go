package filing_test

import (
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
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

const digestDate = "2026-09-14"

var (
	correction = friction.Signal{
		Kind: friction.KindCorrection, SubjectID: "claude:s1", SubjectKind: friction.SubjectSession,
		Detector: "correction.coding", Ordinal: new(4),
		Text: "no, read the key from /home/alice/.config/app: AKIA7QHWN2DKR4FYPLJM",
	}
	diagnostic = friction.Signal{
		Kind: friction.KindError, SubjectID: "run-7:ci_check", SubjectKind: friction.SubjectDiagnostic,
		Detector: "error", ToolName: "ci_check", Text: "run-7:ci_check: nightly check failed",
	}
	frustration = friction.Signal{
		Kind: friction.KindFrustration, SubjectID: "claude:s1", SubjectKind: friction.SubjectSession,
		Detector: "frustration", Text: "this is still broken", Ordinal: new(6),
	}
)

// seedDigest stores one digest holding sigs, with a recurrence row for each.
func seedDigest(t *testing.T, d *db.DB, sigs ...friction.Signal) {
	t.Helper()
	snap := review.Snapshot{
		Date: digestDate, Signals: sigs, SessionsScanned: 1,
		Timezone: "UTC", RulesVersion: friction.RulesVersion,
	}
	snapJSON, err := review.EncodeSnapshot(snap)
	require.NoError(t, err)
	var patterns []db.FrictionPatternUpdate
	for _, sig := range sigs {
		patterns = append(patterns, db.FrictionPatternUpdate{
			Fingerprint: sig.Fingerprint(), Kind: string(sig.Kind), Title: sig.Title(),
			Date: digestDate, SubjectID: sig.SubjectID, Ordinal: sig.Ordinal, Occurrences: 1,
		})
	}
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: digestDate, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 1,
		SnapshotJSON: snapJSON, SummaryJSON: friction.RenderSummaryJSON(snap.DigestSnapshot),
		Markdown: friction.RenderMarkdown(snap.DigestSnapshot), MarkdownSHA256: "sha", RunID: "run",
	}, nil, patterns))
}

func newFiler(t *testing.T, s *katatest.Server, project string, sigs ...friction.Signal) (*filing.Filer, *db.DB) {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	seedDigest(t, d, sigs...)
	conn := kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: project})
	return &filing.Filer{
		Kata: conn, Store: d, PublicURL: "https://av.example.test/base",
		Now: func() time.Time { return time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC) },
	}, d
}

func patternIssue(t *testing.T, d *db.DB, fingerprint string) *db.FrictionIssueLink {
	t.Helper()
	rows, _, err := d.ListFrictionPatterns(t.Context(), db.FrictionPatternFilter{Fingerprints: []string{fingerprint}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].Issue
}

func TestFileCreatesTheIssueOnceAndLinksIt(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()

	res, err := f.File(t.Context(), fp)
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, db.FrictionIssueSourceCreated, res.Link.Source)
	assert.Equal(t, "agentsview#"+mustIssue(t, s, res.Link.IssueUID).ShortID, res.Link.QualifiedID)
	assert.Equal(t, res.Link, *patternIssue(t, d, fp), "pattern reads return the link")

	is := mustIssue(t, s, res.Link.IssueUID)
	assert.Equal(t, fp, is.Metadata[filing.MetadataFingerprint])
	assert.Equal(t, []string{"friction", "friction:correction"}, is.Labels)
	assert.Contains(t, is.Body, "https://av.example.test/base/sessions/claude/s1?msg=4")
	assert.Contains(t, is.Body, "https://av.example.test/base/friction?date=2026-09-14")
	assert.NotContains(t, is.Body, "/home/alice", "home directories are shortened")
	assert.NotContains(t, is.Body, "AKIA7QHWN2DKR4FYPLJM", "secrets are masked")
	assert.NotContains(t, is.Title, "AKIA7QHWN2DKR4FYPLJM")

	again, err := f.File(t.Context(), fp)
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, res.Link, again.Link)
	assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), 1, "a linked pattern is never filed twice")
}

func TestFileLinksTheIssueKataAlreadyHolds(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	existing := s.AddIssue(katatest.Issue{Title: "filed elsewhere", Status: "open", Metadata: map[string]any{filing.MetadataFingerprint: fp}})

	res, err := f.File(t.Context(), fp)
	require.NoError(t, err)
	assert.False(t, res.Created)
	assert.Equal(t, db.FrictionIssueSourceFound, res.Link.Source)
	assert.Equal(t, existing.UID, patternIssue(t, d, fp).IssueUID)
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
}

func TestFileStopsWhenSeveralIssuesMayTrackThePattern(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	for range 2 {
		s.AddIssue(katatest.Issue{Title: "dup", Status: "open", Metadata: map[string]any{filing.MetadataFingerprint: fp}})
	}

	_, err := f.File(t.Context(), fp)
	amb, ok := errors.AsType[*filing.AmbiguousError](err)
	require.True(t, ok, "got %v", err)
	assert.Len(t, amb.Candidates, 2)
	assert.Nil(t, patternIssue(t, d, fp))
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
}

func TestFileLinksTheIssueAnEarlierCreateMade(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	earlier := s.AddIssue(katatest.Issue{Title: "first attempt", Status: "open"})
	s.Fail(http.MethodPost, "/issues", katatest.Fault{
		Status: 409, Code: "idempotency_mismatch",
		Data: map[string]any{"uid": earlier.UID},
	})

	res, err := f.File(t.Context(), fp)
	require.NoError(t, err)
	assert.Equal(t, earlier.UID, res.Link.IssueUID)
	assert.Equal(t, db.FrictionIssueSourceFound, res.Link.Source)
	assert.Equal(t, earlier.UID, patternIssue(t, d, fp).IssueUID)
}

func TestFileRefusals(t *testing.T) {
	tests := []struct {
		name        string
		project     string
		fingerprint string
		want        error
	}{
		{name: "unknown_pattern", project: katatest.DefaultProjectName, fingerprint: "fl1:missing", want: filing.ErrPatternNotFound},
		{name: "frustration_is_never_filed", project: katatest.DefaultProjectName, fingerprint: frustration.Fingerprint(), want: filing.ErrNotFileable},
		{name: "kata_not_ready", project: "no-such-project", fingerprint: correction.Fingerprint(), want: filing.ErrKataNotReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, tt.project, correction, frustration)
			_, err := f.File(t.Context(), tt.fingerprint)
			require.ErrorIs(t, err, tt.want)
			assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
			if tt.fingerprint != "fl1:missing" {
				assert.Nil(t, patternIssue(t, d, tt.fingerprint))
			}
		})
	}
}

func TestPreviewDiagnosticSkipsLookAlikeCheckAndSessionLink(t *testing.T) {
	s := katatest.New(t)
	f, _ := newFiler(t, s, katatest.DefaultProjectName, diagnostic)

	req, err := f.Preview(t.Context(), diagnostic.Fingerprint())
	require.NoError(t, err)
	assert.True(t, req.ForceNew, "diagnostic identities are distinct by construction")
	assert.Contains(t, req.Body, "Source: run-7:ci_check")
	assert.NotContains(t, req.Body, "/sessions/")
	assert.NotContains(t, req.Metadata, "agentsview.session_url")
	assert.Empty(t, s.Requests(), "a preview never contacts Kata")
}

func TestLinkAndUnlink(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	is := s.AddIssue(katatest.Issue{Title: "tracked by hand", Status: "open"})

	_, err := f.Link(t.Context(), fp, "agentsview#nope")
	require.Error(t, err)
	assert.Nil(t, patternIssue(t, d, fp), "a failed lookup links nothing")

	link, err := f.Link(t.Context(), fp, "agentsview#"+is.ShortID)
	require.NoError(t, err)
	assert.Equal(t, db.FrictionIssueSourceManual, link.Source)
	assert.Equal(t, is.UID, patternIssue(t, d, fp).IssueUID)

	elsewhere := s.AddIssue(katatest.Issue{Title: "another project's", Status: "open", ProjectID: 99})
	_, err = f.Link(t.Context(), fp, elsewhere.UID)
	require.ErrorIs(t, err, kata.ErrIssueInOtherProject)
	assert.Equal(t, is.UID, patternIssue(t, d, fp).IssueUID, "a refused link keeps the current one")

	require.NoError(t, f.Unlink(t.Context(), fp))
	assert.Nil(t, patternIssue(t, d, fp))
	require.ErrorIs(t, f.Unlink(t.Context(), fp), filing.ErrNotLinked)
	_, stillThere := s.Issue(is.UID)
	assert.True(t, stillThere, "unlinking leaves the Kata issue alone")
}

func mustIssue(t *testing.T, s *katatest.Server, uid string) katatest.Issue {
	t.Helper()
	is, ok := s.Issue(uid)
	require.True(t, ok, "issue %s", uid)
	return is
}
