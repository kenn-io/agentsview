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
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts} {
		for _, full := range []bool{false, true} {
			t.Run(string(policy)+map[bool]string{false: "/generation", true: "/full"}[full], func(t *testing.T) {
				a, target := seedFixture(t)
				b, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "b.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, b.Close()) })
				dbtest.SeedPricingMeta(t, b)
				dbtest.SeedRetainedCommitLink(t, a, "shared", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, b, "shared", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, a, "distinct", "cccccccccccccccccccccccccccccccccccccccc")
				dbtest.SeedRetainedCommitLink(t, b, "distinct", "dddddddddddddddddddddddddddddddddddddddd")
				sa := newTestSync(t, a, target, storage.PusherOptions{})
				sb := newTestSync(t, b, target, storage.PusherOptions{})
				_, err = sa.Push(t.Context(), true, nil)
				require.NoError(t, err)
				_, err = sb.Push(t.Context(), true, nil)
				require.NoError(t, err)
				resident, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "resident.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, resident.Close()) })
				dbtest.SeedPricingMeta(t, resident)
				dbtest.SeedSession(t, resident, "shared", "commit-project")
				live := newTestSync(t, resident, target, storage.PusherOptions{})
				_, err = live.Push(t.Context(), true, nil)
				require.NoError(t, err)
				_, err = sa.conn.ExecContext(t.Context(), `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,push_version) SELECT session_id,sha,branch,'legacy',repository,message_ordinal,call_index,tool_use_id,committed_at,NULL,'','',true,0 FROM retained_commit_links WHERE source_archive_id=? AND session_id='shared'`, sa.archiveID)
				require.NoError(t, err)
				for range 2 {
					_, err = sa.Push(t.Context(), true, nil)
					require.NoError(t, err)
				}
				store, err := NewStore(t.Context(), target)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				dbtest.AssertSharedCommitLinks(t, store, 0, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				_, err = live.conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='shared' SETTINGS mutations_sync=2`)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 1, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				sa = newTestSync(t, dbtest.RebuildCommitArchive(t, a, policy), target, storage.PusherOptions{})
				for i := range 2 {
					result, err := sa.Push(t.Context(), full, nil)
					require.NoError(t, err)
					require.True(t, i > 0 || result.Full)
					dbtest.AssertSharedCommitLinks(t, store, 1, "dddddddddddddddddddddddddddddddddddddddd")
				}
				sb = newTestSync(t, dbtest.RebuildCommitArchive(t, b, policy), target, storage.PusherOptions{})
				_, err = sb.Push(t.Context(), full, nil)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 1)
				links, err := store.SessionCommitLinks(t.Context(), "shared")
				require.NoError(t, err)
				require.Equal(t, "legacy", links.Commits[0].Subject)
			})
		}
	}
}
