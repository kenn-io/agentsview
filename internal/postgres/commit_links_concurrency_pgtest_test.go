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
	if strings.HasPrefix(data.SQL, "DELETE FROM retained_commit_links") {
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
		t.Run(map[bool]string{false: "empty", true: "prompt"}[withPrompt], func(t *testing.T) {
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
			require.NoError(t, localA.DeleteSession(ctx, "commit-live"))
			var err error
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
			require.NoError(t, <-done)
			store, err := NewStore(ctx, testPGURL(t), a.schema, true)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			for range 2 {
				links, err := store.SessionCommitLinks(ctx, "commit-live")
				require.NoError(t, err)
				matches, err := store.CommitSessionLinks(ctx, []string{"abc1234567890123456789012345678901234567"})
				require.NoError(t, err)
				got := links.Commits
				for _, link := range matches.Commits[0].Links {
					if link.SessionID == "commit-live" {
						got = append(got, link)
					}
				}
				require.Len(t, got, map[bool]int{false: 0, true: 2}[withPrompt])
				for _, link := range got {
					require.False(t, link.Missing)
					require.NotNil(t, link.Turn)
					require.Equal(t, "Record the parser fix", link.Turn.Text)
				}
				_, err = b.Push(ctx, false, nil)
				require.NoError(t, err)
			}
		})
	}
}
