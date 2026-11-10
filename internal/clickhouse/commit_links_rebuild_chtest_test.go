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
				idA, err := a.GetArchiveID(t.Context())
				require.NoError(t, err)
				idB, err := b.GetArchiveID(t.Context())
				require.NoError(t, err)
				idLive, err := resident.GetArchiveID(t.Context())
				require.NoError(t, err)
				require.NotEqual(t, idA, idB)
				require.NotEqual(t, idB, idLive)
				live := newTestSync(t, resident, target, storage.PusherOptions{})
				_, err = live.Push(t.Context(), true, nil)
				require.NoError(t, err)
				_, err = sa.conn.ExecContext(t.Context(), `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,push_version) SELECT session_id,sha,branch,'legacy',repository,message_ordinal,call_index,tool_use_id,committed_at,NULL,'','',true,0 FROM retained_commit_links WHERE source_archive_id=? AND session_id='shared'`, sa.archiveID)
				require.NoError(t, err)
				_, err = sa.conn.ExecContext(t.Context(), `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained,push_version) SELECT 'unknown','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,NULL,'','',true,0 FROM retained_commit_links WHERE source_archive_id=? AND session_id='shared'`, sa.archiveID)
				require.NoError(t, err)
				for range 2 {
					_, err = sa.Push(t.Context(), true, nil)
					require.NoError(t, err)
				}
				var count uint64
				require.NoError(t, sa.conn.QueryRowContext(t.Context(), `SELECT count() FROM commit_links WHERE session_id IN ('shared','unknown')`).Scan(&count))
				require.EqualValues(t, 2, count)
				store, err := NewStore(t.Context(), target)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				dbtest.AssertSharedCommitLinks(t, store, 0, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				_, err = live.conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='shared' SETTINGS mutations_sync=2`)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 1, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				links, err := store.SessionCommitLinks(t.Context(), "shared")
				require.NoError(t, err)
				require.Len(t, links.Commits, 1)
				require.Equal(t, "Save the change", links.Commits[0].Subject)
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
				links, err = store.SessionCommitLinks(t.Context(), "shared")
				require.NoError(t, err)
				require.Equal(t, "legacy", links.Commits[0].Subject)
			})
		}
	}
}

func TestClickHouseRetainedPublicationKeepsLiveCommitLinks(t *testing.T) {
	for _, withPrompt := range []bool{true, false} {
		name := "empty"
		if withPrompt {
			name = "prompt"
		}
		t.Run(name, func(t *testing.T) {
			retained, target := seedFixture(t)
			live, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "live.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, live.Close()) })
			dbtest.SeedPricingMeta(t, live)
			dbtest.SeedCommitLinks(t, retained)
			dbtest.SeedCommitLinks(t, live)
			require.NoError(t, retained.DeleteSession(t.Context(), "commit-live"))
			if !withPrompt {
				require.NoError(t, live.ReplaceSessionMessages(t.Context(), "commit-live", nil))
			}
			publisher := newTestSync(t, live, target, storage.PusherOptions{})
			_, err = publisher.Push(t.Context(), true, nil)
			require.NoError(t, err)
			retainedPublisher := newTestSync(t, retained, target, storage.PusherOptions{})
			store, err := NewStore(t.Context(), target)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			for range 2 {
				_, err = retainedPublisher.Push(t.Context(), true, nil)
				require.NoError(t, err)
				links, err := store.SessionCommitLinks(t.Context(), "commit-live")
				require.NoError(t, err)
				if withPrompt {
					require.Len(t, links.Commits, 1)
					require.False(t, links.Commits[0].Missing)
					require.NotNil(t, links.Commits[0].Turn)
					require.Equal(t, "Record the parser fix", links.Commits[0].Turn.Text)
				} else {
					require.Empty(t, links.Commits)
				}
				matches, err := store.CommitSessionLinks(t.Context(), []string{"abc1234567890123456789012345678901234567"})
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
			}
		})
	}
}
