//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

type retainedDeleteBarrier struct{ paused, release chan struct{} }

func (b *retainedDeleteBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "DELETE FROM retained_commit_links") || strings.HasPrefix(data.SQL, "DELETE FROM commit_links WHERE session_id=") {
		close(b.paused)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*retainedDeleteBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPGRetainedCommitLinkPublicationRace(t *testing.T) {
	for _, withPrompt := range []bool{true, false} {
		name := "empty"
		if withPrompt {
			name = "prompt"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			a, localA, pg, _ := newSessionProvenancePushSync(t, "agentsview_commit_race_test")
			localB := testDB(t)
			dbtest.SeedCommitLinks(t, localA)
			dbtest.SeedCommitLinks(t, localB)
			if !withPrompt {
				require.NoError(t, localB.ReplaceSessionMessages(ctx, "commit-live", nil))
			}
			b := &Sync{pg: pg, local: localB, machine: "workstation", schema: a.schema, schemaDone: true}
			_, err := b.Push(ctx, true, nil)
			require.NoError(t, err)
			_, err = pg.ExecContext(ctx, `DELETE FROM sessions WHERE id='commit-live'`)
			require.NoError(t, err)
			require.NoError(t, localA.DeleteSession(ctx, "commit-live"))
			a.archiveID, err = localA.GetArchiveID(ctx)
			require.NoError(t, err)
			connStr, err := appendConnParams(testPGURL(t), map[string]string{"search_path": a.schema, "TimeZone": "UTC"})
			require.NoError(t, err)
			cfg, err := pgx.ParseConfig(connStr)
			require.NoError(t, err)
			barrier := &retainedDeleteBarrier{make(chan struct{}), make(chan struct{})}
			cfg.Tracer = barrier
			traced := stdlib.OpenDB(*cfg)
			defer func() { cancel(); _ = traced.Close() }()
			a.pg = traced
			done := make(chan error, 1)
			go func() { done <- a.replaceRetainedCommitLinks(ctx) }()
			select {
			case <-barrier.paused:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, err = b.Push(ctx, true, nil)
			close(barrier.release)
			require.NoError(t, err)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			store, err := NewStore(ctx, testPGURL(t), a.schema, true)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			for range 2 {
				links, err := store.SessionCommitLinks(ctx, "commit-live")
				require.NoError(t, err)
				if withPrompt {
					require.Len(t, links.Commits, 1)
					require.False(t, links.Commits[0].Missing)
					require.NotNil(t, links.Commits[0].Turn)
					require.Equal(t, "Record the parser fix", links.Commits[0].Turn.Text)
				} else {
					require.Empty(t, links.Commits)
				}
				matches, err := store.CommitSessionLinks(ctx, []string{"abc1234567890123456789012345678901234567"})
				require.NoError(t, err)
				found := false
				for _, link := range matches.Commits[0].Links {
					if link.SessionID == "commit-live" {
						found = true
						require.True(t, withPrompt)
						require.NotNil(t, link.Turn)
						require.Equal(t, "Record the parser fix", link.Turn.Text)
					}
				}
				require.Equal(t, withPrompt, found)
				_, err = b.Push(ctx, false, nil)
				require.NoError(t, err)
			}
		})
	}
}
