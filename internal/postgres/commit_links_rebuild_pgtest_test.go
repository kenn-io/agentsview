//go:build pgtest

package postgres

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"testing"
)

func TestPGRetainedCommitLinksAfterNarrowRebuild(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, full := range []bool{false, true} {
			t.Run(string(policy)+map[bool]string{false: "/generation", true: "/full"}[full], func(t *testing.T) {
				syncer, a, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_rebuild_test")
				b := dbtest.OpenTestDB(t)
				dbtest.SeedPricingMeta(t, b)
				dbtest.SeedRetainedCommitLink(t, a, "shared", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, b, "shared", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, a, "distinct", "cccccccccccccccccccccccccccccccccccccccc")
				dbtest.SeedRetainedCommitLink(t, b, "distinct", "dddddddddddddddddddddddddddddddddddddddd")
				_, err := syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				syncer.local = b
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				store, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				_, err = pg.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) SELECT session_id,sha,branch,'legacy',repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,true FROM retained_commit_links WHERE source_archive_id=$1 AND session_id='shared'`, syncer.archiveID)
				require.NoError(t, err)
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				links, err := store.SessionCommitLinks(ctx, "shared")
				require.NoError(t, err)
				require.Len(t, links.Commits, 1)
				require.Equal(t, "Save the change", links.Commits[0].Subject)
				_, err = pg.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) SELECT session_id,sha,branch,'orphan',repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,false FROM retained_commit_links WHERE source_archive_id=$1 AND session_id='distinct'`, syncer.archiveID)
				require.NoError(t, err)
				links, err = store.SessionCommitLinks(ctx, "distinct")
				require.NoError(t, err)
				require.Len(t, links.Commits, 2)
				for _, link := range links.Commits {
					require.Equal(t, "Save the change", link.Subject)
				}
				_, err = pg.ExecContext(ctx, `DELETE FROM commit_links WHERE session_id='distinct'`)
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent) VALUES ('shared','commit-project','test','claude')`)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 0, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				_, err = pg.ExecContext(ctx, `DELETE FROM sessions WHERE id='shared'`)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 1, "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd")
				syncer.local = dbtest.RebuildCommitArchive(t, a, policy)
				for i := range 2 {
					result, err := syncer.Push(ctx, full, nil)
					require.NoError(t, err)
					require.True(t, i > 0 || result.Full)
					dbtest.AssertSharedCommitLinks(t, store, 1, "dddddddddddddddddddddddddddddddddddddddd")
				}
				syncer.local = dbtest.RebuildCommitArchive(t, b, policy)
				_, err = syncer.Push(ctx, full, nil)
				require.NoError(t, err)
				dbtest.AssertSharedCommitLinks(t, store, 1)
				links, err = store.SessionCommitLinks(ctx, "shared")
				require.NoError(t, err)
				require.Equal(t, "legacy", links.Commits[0].Subject)
			})
		}
	}
}

func TestPGRetainedCommitLinksArchiveRetirement(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_retire_test")
	dbtest.SeedRetainedCommitLink(t, local, "shared", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	id := syncer.archiveID
	syncer.projects = []string{"commit-project"}
	require.NoError(t, syncer.retireSourceArchiveMetadata(ctx, id))
	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM retained_commit_links WHERE source_archive_id=$1`, id).Scan(&count))
	require.Equal(t, 1, count)
	syncer.projects = nil
	require.NoError(t, syncer.retireSourceArchiveMetadata(ctx, id))
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT count(*) FROM retained_commit_links WHERE source_archive_id=$1`, id).Scan(&count))
	require.Zero(t, count)
}
