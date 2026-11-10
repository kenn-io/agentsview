//go:build chtest

package clickhouse

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseRetainedCommitLinksAfterNarrowRebuild(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, full := range []bool{false, true} {
			t.Run(string(policy)+map[bool]string{false: "/generation", true: "/full"}[full], func(t *testing.T) {
				a, target := seedFixture(t)
				b, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, b.Close()) })
				dbtest.SeedPricingMeta(t, b)
				dbtest.SeedRetainedCommitLink(t, a, "archive-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, b, "archive-b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
				sa := newTestSync(t, a, target, storage.PusherOptions{})
				_, err = sa.Push(t.Context(), true, nil)
				require.NoError(t, err)
				sb := newTestSync(t, b, target, storage.PusherOptions{})
				_, err = sb.Push(t.Context(), true, nil)
				require.NoError(t, err)
				rebuilt := dbtest.RebuildCommitArchive(t, a, policy)
				sr := newTestSync(t, rebuilt, target, storage.PusherOptions{})
				store, err := NewStore(t.Context(), target)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				for i := range 2 {
					result, err := sr.Push(t.Context(), full, nil)
					require.NoError(t, err)
					if i == 0 {
						require.True(t, result.Full)
					}
					dbtest.AssertRebuiltCommitLinks(t, store)

				}
			})
		}
	}
}

func TestClickHouseRetainedCommitLinksSharedPublishers(t *testing.T) {
	a, target := seedFixture(t)
	b, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	dbtest.SeedPricingMeta(t, b)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dbtest.SeedRetainedCommitLink(t, a, "shared", sha)
	dbtest.SeedRetainedCommitLink(t, b, "shared", sha)
	sa := newTestSync(t, a, target, storage.PusherOptions{})
	sb := newTestSync(t, b, target, storage.PusherOptions{})
	_, err = sa.Push(t.Context(), true, nil)
	require.NoError(t, err)
	_, err = sb.Push(t.Context(), true, nil)
	require.NoError(t, err)
	// A pre-upgrade copy is adopted on retry; unrelated legacy evidence stays.
	_, err = sa.conn.ExecContext(t.Context(), `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,push_version) SELECT session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,0 FROM retained_commit_links WHERE source_archive_id=? AND session_id='shared'`, sa.archiveID)
	require.NoError(t, err)
	_, err = sa.conn.ExecContext(t.Context(), `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,push_version) SELECT 'unknown','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,0 FROM retained_commit_links WHERE source_archive_id=? AND session_id='shared'`, sa.archiveID)
	require.NoError(t, err)
	_, err = sa.Push(t.Context(), true, nil)
	require.NoError(t, err)
	_, err = sa.Push(t.Context(), true, nil)
	require.NoError(t, err)
	var legacy uint64
	require.NoError(t, sa.conn.QueryRowContext(t.Context(), `SELECT count() FROM commit_links WHERE session_id='shared'`).Scan(&legacy))
	require.Zero(t, legacy)
	require.NoError(t, sa.conn.QueryRowContext(t.Context(), `SELECT count() FROM commit_links WHERE session_id='unknown'`).Scan(&legacy))
	require.EqualValues(t, 1, legacy)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	check := func(n int) {
		t.Helper()
		links, err := store.SessionCommitLinks(t.Context(), "shared")
		require.NoError(t, err)
		require.Len(t, links.Commits, n)
		matches, err := store.CommitSessionLinks(t.Context(), []string{sha})
		require.NoError(t, err)
		require.Len(t, matches.Commits[0].Links, n)
	}
	check(1)
	sa = newTestSync(t, dbtest.RebuildCommitArchive(t, a, config.ArchiveContentUsage), target, storage.PusherOptions{})
	_, err = sa.Push(t.Context(), true, nil)
	require.NoError(t, err)
	check(1)
	sb = newTestSync(t, dbtest.RebuildCommitArchive(t, b, config.ArchiveContentUsage), target, storage.PusherOptions{})
	_, err = sb.Push(t.Context(), true, nil)
	require.NoError(t, err)
	check(0)
}
