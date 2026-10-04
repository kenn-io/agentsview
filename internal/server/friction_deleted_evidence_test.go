package server_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func TestFrictionFileRouteTrashedDigestEvidence(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "preview", false: "create"}[dryRun], func(t *testing.T) {
			fake := katatest.New(t)
			sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:trashed", Text: "No, this is wrong.", Ordinal: new(1)}
			conn := kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: fake.Endpoint(), Project: "agentsview"})
			f := &filing.Filer{Kata: conn, Snapshot: func(context.Context, string) (friction.DigestSnapshot, error) {
				return friction.DigestSnapshot{Date: "2026-09-20", Signals: []friction.Signal{sig}}, nil
			}}
			te := setupWithServerOpts(t, []server.Option{server.WithKataConn(conn), server.WithFrictionFiler(f)})
			f.Store = fixedDates{LinkStore: te.db, dates: []string{"2026-09-20"}}
			f.Archive = te.db
			dbtest.SeedSession(t, te.db, sig.SubjectID, "example")
			require.NoError(t, te.db.SoftDeleteSession(t.Context(), sig.SubjectID))
			body := `{}`
			if dryRun {
				body = `{"dry_run":true}`
			}
			w := te.post(t, "/api/v1/friction/patterns/"+sig.Fingerprint()+"/file", body)
			if dryRun {
				assertStatus(t, w, http.StatusNotFound)
			} else {
				assertStatus(t, w, http.StatusOK)
				var response server.FrictionFileResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.NotNil(t, response.Link)
				assert.Equal(t, db.FrictionLinkStateNeedsHuman, response.Link.State)
				assert.Equal(t, "signal_not_found", response.Link.LastErrorCode)
				assert.Nil(t, response.Link.NextAttemptAt)
			}
			assert.Empty(t, fake.RequestsMatching(http.MethodPost, "/api/v1/projects/17/issues"))
		})
	}
}
