package main

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

var fileableCorrection = friction.Signal{
	Kind: friction.KindCorrection, SubjectID: "claude:s1", SubjectKind: friction.SubjectSession,
	Detector: "correction.coding", Text: "no, use the other flag", Ordinal: new(4),
}

// seedFileableDigest stores a digest holding one correction and its pattern.
func seedFileableDigest(t *testing.T, d *db.DB) string {
	t.Helper()
	const date = "2026-09-14"
	snap := review.Snapshot{
		Date: date, Signals: []friction.Signal{fileableCorrection}, SessionsScanned: 1,
		Timezone: "UTC", RulesVersion: friction.RulesVersion,
	}
	snapJSON, err := review.EncodeSnapshot(snap)
	require.NoError(t, err)
	sig := fileableCorrection
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 1,
		SnapshotJSON: snapJSON, SummaryJSON: friction.RenderSummaryJSON(snap.DigestSnapshot),
		Markdown: friction.RenderMarkdown(snap.DigestSnapshot), MarkdownSHA256: "s", RunID: "r",
	}, nil, []db.FrictionPatternUpdate{{
		Fingerprint: sig.Fingerprint(), Kind: string(sig.Kind), Title: sig.Title(),
		Date: date, SubjectID: sig.SubjectID, Ordinal: sig.Ordinal, Occurrences: 1,
	}}))
	return sig.Fingerprint()
}

func TestFrictionFileLinkUnlinkThroughDaemon(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	fp := seedFileableDigest(t, d)
	s := katatest.New(t)
	ts := localServerProxy(t, d, dataDir, server.WithKataConn(kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
	})))
	run := func(args ...string) (string, error) {
		stdout, _, err := runInsightCommand(t, append(args, "--server", ts.URL)...)
		return stdout, err
	}

	out, err := run("friction", "file", fp, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "Labels:   friction, friction:correction")
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))

	out, err = run("friction", "file", fp)
	require.NoError(t, err)
	assert.Regexp(t, `^Filed agentsview#\w+`, out)

	out, err = run("friction", "patterns")
	require.NoError(t, err)
	assert.Contains(t, out, "ISSUE")
	assert.Regexp(t, `agentsview#\w+`, out)

	out, err = run("friction", "file", fp)
	require.NoError(t, err)
	assert.Contains(t, out, "Linked the existing Kata issue")
	assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), 1)

	other := s.AddIssue(katatest.Issue{Title: "tracked by hand", Status: "open"})
	out, err = run("friction", "link", fp, katatest.DefaultProjectName+"#"+other.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "Linked "+fp+" to "+katatest.DefaultProjectName+"#"+other.ShortID+"\n", out)

	out, err = run("friction", "unlink", fp)
	require.NoError(t, err)
	assert.Contains(t, out, "Removed the Kata link")
	_, err = run("friction", "unlink", fp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

func TestFrictionFileLocalWithoutDaemon(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{name: "hub_files"},
		{
			name: "pusher_never_files", config: "[pg]\nurl = \"postgres://pg.example.test/agentsview\"\n",
			wantErr: "only the AgentsView hub files to Kata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := testDataDir(t)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			d := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
			fp := seedFileableDigest(t, d)
			require.NoError(t, d.Close())
			s := katatest.New(t)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
				tt.config+"[kata]\nenabled = true\nendpoint = \""+s.Endpoint()+"\"\n"), 0o600))

			stdout, _, err := runInsightCommand(t, "friction", "file", fp, "--json")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, s.Requests(), "a pusher never contacts Kata")
				return
			}
			require.NoError(t, err)
			var out struct {
				Created bool `json:"created"`
				Issue   struct {
					QualifiedID string `json:"qualified_id"`
				} `json:"issue"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &out))
			assert.True(t, out.Created)

			d = dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
			rows, _, err := d.ListFrictionPatterns(t.Context(), db.FrictionPatternFilter{Fingerprints: []string{fp}})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].Issue)
			assert.Equal(t, out.Issue.QualifiedID, rows[0].Issue.QualifiedID, "the local run stored the link it reported")
		})
	}
}
