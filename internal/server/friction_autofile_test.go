package server_test

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func seedClosedFrictionIssue(t *testing.T, d *db.DB, s *katatest.Server, fp string) string {
	t.Helper()
	occurred := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	digest, err := d.GetFrictionDigest(t.Context(), "2026-09-14")
	require.NoError(t, err)
	require.NotNil(t, digest)
	snapshot, err := review.DecodeSnapshot(digest.SnapshotJSON)
	require.NoError(t, err)
	for i := range snapshot.Signals {
		if snapshot.Signals[i].Fingerprint() == fp {
			snapshot.Signals[i].OccurredAt = occurred
		}
	}
	digest.SnapshotJSON, err = review.EncodeSnapshot(snapshot)
	require.NoError(t, err)
	digest.Revision++
	require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, nil, nil))
	closed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	require.NoError(t, d.SaveFrictionIssueLink(t.Context(), db.FrictionIssueLink{
		Fingerprint: fp, KataInstanceUID: "instance", KataProjectUID: "project", IssueUID: is.UID,
		QualifiedID: "agentsview#" + is.ShortID, Source: db.FrictionIssueSourceManual, LinkedAt: closed,
	}))
	return is.UID
}

func TestFrictionJobFilesAutomaticallyOnTheHub(t *testing.T) {
	tests := []struct {
		name     string
		autoFile bool
		hub      bool
		// minSessions overrides the default of 2; the fixture's patterns
		// were each seen in one session.
		minSessions int
		filed       bool
	}{
		{name: "auto_file_on_hub", autoFile: true, hub: true, minSessions: 1, filed: true},
		{name: "single_session_patterns_wait_by_default", autoFile: true, hub: true},
		{name: "auto_file_off", hub: true, minSessions: 1},
		{name: "not_the_hub", autoFile: true, minSessions: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
			runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
			te := setupWithServerOpts(t, []server.Option{
				server.WithFriction(runner, nil),
				server.WithKataConn(kata.NewConn(kata.Config{
					Enabled: true, Hub: tt.hub, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
				})),
			}, func(c *config.Config) {
				c.Friction.BackfillDays = 7
				c.Friction.Kata.AutoFile = tt.autoFile
				c.Friction.Kata.MinSessions = tt.minSessions
			})
			runner.Store = te.db
			const date = "2026-09-14"
			seedFrictionDigest(t, te.db, date)
			fp := frictionTestSnapshot(date).Signals[0].Fingerprint()

			linkedUID := seedClosedFrictionIssue(t, te.db, s, frictionTestSnapshot(date).Signals[1].Fingerprint())

			w := te.get(t, "/api/v1/version")
			assertStatus(t, w, http.StatusOK)
			version := decode[struct {
				FrictionAutoFile bool `json:"friction_auto_file"`
			}](t, w)
			assert.Equal(t, tt.autoFile && tt.hub, version.FrictionAutoFile, "the page learns whether a pause matters")

			require.NoError(t, review.Job(runner, nil).Run(t.Context()))
			got, ok := s.Issue(linkedUID)
			require.True(t, ok)
			if tt.autoFile && tt.hub {
				assert.Equal(t, "open", got.Status)
			} else {
				assert.Equal(t, "closed", got.Status)
			}
			if !tt.filed {
				assert.Nil(t, patternIssue(t, te, fp))
				assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
				return
			}
			issue := patternIssue(t, te, fp)
			require.NotNil(t, issue, "the hourly job filed the recent pattern")
			assert.Equal(t, "created", issue.Source)
		})
	}
}

func TestFrictionJobDoesNotFileWhereLinksCannotBeSaved(t *testing.T) {
	s := katatest.New(t)
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host: "127.0.0.1", DataDir: dir, DBPath: filepath.Join(dir, "test.db"), WriteTimeout: 30 * time.Second,
	}
	cfg.Friction.BackfillDays = 7
	cfg.Friction.Kata.AutoFile = true
	cfg.Friction.Kata.MinSessions = 1
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	seedFrictionDigest(t, database, "2026-09-14")
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Store: database, Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	server.New(cfg, linksUnwritableStore{database}, nil,
		server.WithFriction(runner, nil),
		server.WithKataConn(kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
		})))

	require.NoError(t, review.Job(runner, nil).Run(t.Context()))
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"), "no issue is created without a place for its link")
}

// The local daemon starts the friction job after the server exists and
// installs it with WithFriction then; automatic filing must still run.
func TestFrictionJobFilesWhenInstalledAfterTheServer(t *testing.T) {
	s := katatest.New(t)
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	te := setupWithServerOpts(t, []server.Option{
		server.WithKataConn(kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
		})),
	}, func(c *config.Config) {
		c.Friction.BackfillDays = 7
		c.Friction.Kata.AutoFile = true
		c.Friction.Kata.MinSessions = 1
	})
	runner.Store = te.db
	const date = "2026-09-14"
	seedFrictionDigest(t, te.db, date)
	server.WithFriction(runner, nil)(te.srv)

	require.NoError(t, review.Job(runner, nil).Run(t.Context()))
	require.NotNil(t, patternIssue(t, te, frictionTestSnapshot(date).Signals[0].Fingerprint()))
}
