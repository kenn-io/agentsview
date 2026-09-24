// internal/friction/review/filing_integration_test.go
package review_test

import (
	"strings"
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

func seedErrorSession(t *testing.T, d *db.DB, id, ended string, sigs ...friction.Signal) {
	t.Helper()
	dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) {
		s.Machine = "laptop-a"
		s.StartedAt = new(ended)
		s.EndedAt = new(ended)
		s.MessageCount = 4
	})
	findings := make([]db.FrictionFinding, 0, len(sigs))
	for i, s := range sigs {
		findings = append(findings, db.FrictionFinding{SessionID: id, Kind: string(s.Kind), Detector: s.Detector, MessageOrdinal: s.Ordinal,
			ToolName: s.ToolName, Label: s.Label, Text: s.Text, Title: s.Title(), Fingerprint: s.Fingerprint(), Seq: i, RulesVersion: friction.RulesVersion})
	}
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), id, findings, nil, friction.RulesVersion, "h-"+id))
}

func TestBuildDateKataDownThenDrain(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	errS := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", Detector: "error", ToolName: "Bash", Text: "boom", Ordinal: new(2)}
	def := friction.Signal{Kind: friction.KindDeferral, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", Detector: "deferral", Label: "write the docs", Ordinal: new(3)}
	seedErrorSession(t, d, "claude:s1", "2026-09-20T15:00:00Z", errS, def)

	fake := katatest.New(t)
	fake.Close() // Kata is down for the build
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	runner := &review.Runner{Store: d, Loc: time.UTC, Now: func() time.Time { return now }, BackfillDays: 7, AutoFile: true, PublicURL: "https://av.example.test"}
	filer := &filing.Filer{
		Kata:  kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: fake.Endpoint(), Project: "agentsview"}),
		Store: d, Redact: filing.DefaultRedact, Now: func() time.Time { return now },
		Policy: filing.Policy{Kinds: filing.DefaultKinds(), ReopenOnRecurrence: true}, Instance: "inst",
		PublicURL: runner.PublicURL, Snapshot: runner.Snapshot, Rerender: runner.Rerender,
	}
	runner.Filer = filer

	rep, err := runner.BuildDate(t.Context(), "2026-09-20", review.BuildOptions{})
	require.NoError(t, err, "Kata down never fails a build")
	require.True(t, rep.Written)
	dg, err := d.GetFrictionDigest(t.Context(), "2026-09-20")
	require.NoError(t, err)
	assert.NotContains(t, string(dg.Markdown), "(→ kata#")
	assert.Equal(t, 1, dg.Revision)
	links, err := d.GetFrictionIssueLinks(t.Context(), []string{errS.Fingerprint(), def.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, db.FrictionLinkStatePending, links[errS.Fingerprint()].State)
	assert.Equal(t, db.FrictionLinkStatePending, links[def.Fingerprint()].State)

	// Kata comes back on the same address space: point the filer at a live fake.
	live := katatest.New(t)
	filer.Kata = kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: live.Endpoint(), Project: "agentsview"})
	_, err = runner.CatchUp(t.Context()) // runs the drain (§8.2 step 4)
	require.NoError(t, err)

	dg, err = d.GetFrictionDigest(t.Context(), "2026-09-20")
	require.NoError(t, err)
	md := string(dg.Markdown)
	linked, err := d.GetFrictionIssueLinks(t.Context(), []string{errS.Fingerprint()})
	require.NoError(t, err)
	errLink := linked[errS.Fingerprint()]
	assert.Equal(t, db.FrictionLinkStateLinked, errLink.State)
	assert.Contains(t, md, "(→ kata#"+strings.TrimPrefix(errLink.QualifiedID, "agentsview#")+")", "error line annotated after the drain")
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "write the docs") {
			assert.NotContains(t, line, "(→ kata#", "deferrals never get annotations")
		}
	}
	assert.Equal(t, 2, dg.Revision)
	dates, err := d.DigestDatesForFingerprints(t.Context(), []string{errS.Fingerprint()})
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-20"}, dates)
}

func TestBuildDateFilesInlineWhenReady(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	errS := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:s1", Detector: "error", ToolName: "Bash", Text: "boom", Ordinal: new(2)}
	seedErrorSession(t, d, "claude:s1", "2026-09-20T15:00:00Z", errS)
	fake := katatest.New(t)
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	runner := &review.Runner{Store: d, Loc: time.UTC, Now: func() time.Time { return now }, BackfillDays: 7, AutoFile: true}
	runner.Filer = &filing.Filer{Kata: kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: fake.Endpoint(), Project: "agentsview"}),
		Store: d, Now: func() time.Time { return now }, Policy: filing.Policy{Kinds: filing.DefaultKinds()}, Snapshot: runner.Snapshot, Rerender: runner.Rerender}

	rep, err := runner.BuildDate(t.Context(), "2026-09-20", review.BuildOptions{})
	require.NoError(t, err)
	assert.Len(t, rep.Meta.CreatedIssues, 1)
	assert.Equal(t, 0, rep.Meta.TrackerFailures)
	dg, err := d.GetFrictionDigest(t.Context(), "2026-09-20")
	require.NoError(t, err)
	assert.Contains(t, string(dg.Markdown), "(→ kata#f001)")
	assert.Equal(t, 1, dg.Revision, "inline filing needs no re-render")

	dry, err := runner.BuildDate(t.Context(), "2026-09-20", review.BuildOptions{Rebuild: true, DryRun: true})
	require.NoError(t, err)
	assert.False(t, dry.Written)
	assert.Len(t, fake.RequestsMatching("POST", "/issues"), 1, "dry run files nothing")
}

func TestCopyFrictionStateFromCopiesLinks(t *testing.T) {
	src := dbtest.OpenTestDB(t)
	require.NoError(t, src.UpsertFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: "fl1:aa", State: db.FrictionLinkStateLinked,
		IssueUID: "01J0ABCDEF0000000000000001", QualifiedID: "agentsview#f001", UpdatedAt: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)}))
	srcPath := src.Path()
	dst := dbtest.OpenTestDB(t)
	require.NoError(t, dst.CopyFrictionStateFrom(srcPath))
	got, err := dst.GetFrictionIssueLinks(t.Context(), []string{"fl1:aa"})
	require.NoError(t, err)
	assert.Equal(t, "agentsview#f001", got["fl1:aa"].QualifiedID)
}
