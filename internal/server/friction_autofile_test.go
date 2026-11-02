package server_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func TestFrictionJobFilesAutomaticallyOnTheHub(t *testing.T) {
	tests := []struct {
		name     string
		autoFile bool
		hub      bool
		filed    bool
	}{
		{name: "auto_file_on_hub", autoFile: true, hub: true, filed: true},
		{name: "auto_file_off", hub: true},
		{name: "not_the_hub", autoFile: true},
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
			})
			runner.Store = te.db
			const date = "2026-09-14"
			seedFrictionDigest(t, te.db, date)
			fp := frictionTestSnapshot(date).Signals[0].Fingerprint()

			require.NoError(t, review.Job(runner, nil).Run(t.Context()))
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
