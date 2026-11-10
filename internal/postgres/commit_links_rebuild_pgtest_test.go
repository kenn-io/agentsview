//go:build pgtest

package postgres

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"testing"
)

func TestPGRetainedCommitLinksAfterNarrowRebuild(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts} {
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
				_, err = pg.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) SELECT session_id,sha,branch,'legacy',repository,message_ordinal,call_index,tool_use_id,committed_at,NULL,'','',true FROM retained_commit_links WHERE source_archive_id=$1 AND session_id='shared'`, syncer.archiveID)
				require.NoError(t, err)
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
				links, err := store.SessionCommitLinks(ctx, "shared")
				require.NoError(t, err)
				require.Equal(t, "legacy", links.Commits[0].Subject)
			})
		}
	}
}
