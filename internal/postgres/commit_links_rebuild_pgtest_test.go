//go:build pgtest

package postgres

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestPGRetainedCommitLinksAfterNarrowRebuild(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, full := range []bool{false, true} {
			t.Run(string(policy)+map[bool]string{false: "/generation", true: "/full"}[full], func(t *testing.T) {
				syncer, a, _, ctx := newSessionProvenancePushSync(t, "agentsview_commit_rebuild_test")
				b := dbtest.OpenTestDB(t)
				dbtest.SeedPricingMeta(t, b)
				dbtest.SeedRetainedCommitLink(t, a, "archive-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				dbtest.SeedRetainedCommitLink(t, b, "archive-b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
				_, err := syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				syncer.local = b
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				syncer.local = dbtest.RebuildCommitArchive(t, a, policy)
				store, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				for i := range 2 {
					result, err := syncer.Push(ctx, full, nil)
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

func TestPGRetainedCommitLinksSharedPublishers(t *testing.T) {
	syncer, a, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_shared_test")
	b := dbtest.OpenTestDB(t)
	dbtest.SeedPricingMeta(t, b)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dbtest.SeedRetainedCommitLink(t, a, "shared", sha)
	dbtest.SeedRetainedCommitLink(t, b, "shared", sha)
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
	check := func(n int) {
		t.Helper()
		links, err := store.SessionCommitLinks(ctx, "shared")
		require.NoError(t, err)
		require.Len(t, links.Commits, n)
		matches, err := store.CommitSessionLinks(ctx, []string{sha})
		require.NoError(t, err)
		require.Len(t, matches.Commits[0].Links, n)
	}
	checkDistinct := func(want []string) {
		t.Helper()
		links, err := store.SessionCommitLinks(t.Context(), "distinct")
		require.NoError(t, err)
		var shas []string
		for _, link := range links.Commits {
			shas = append(shas, link.SHA)
		}
		require.ElementsMatch(t, want, shas)
		matches, err := store.CommitSessionLinks(t.Context(), []string{"cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd"})
		require.NoError(t, err)
		for i, sha := range []string{"cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd"} {
			if slices.Contains(want, sha) {
				require.Len(t, matches.Commits[i].Links, 1)
				require.Equal(t, "distinct", matches.Commits[i].Links[0].SessionID)
			} else {
				require.Empty(t, matches.Commits[i].Links)
			}
		}
	}
	checkDistinct([]string{"cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd"})
	check(1)
	_, err = pg.ExecContext(ctx, `INSERT INTO commit_links (session_id,sha,branch,subject,repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,retained) SELECT session_id,sha,branch,'orphan',repository,message_ordinal,call_index,tool_use_id,committed_at,turn_ordinal,turn_text,project,false FROM retained_commit_links WHERE source_archive_id=$1`, syncer.archiveID)
	require.NoError(t, err)
	links, err := store.SessionCommitLinks(ctx, "shared")
	require.NoError(t, err)
	require.Len(t, links.Commits, 1)
	require.Equal(t, "Save the change", links.Commits[0].Subject)
	_, err = pg.ExecContext(ctx, `DELETE FROM commit_links WHERE session_id='shared'`)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent) VALUES ('shared','commit-project','test','claude')`)
	require.NoError(t, err)
	check(0)
	_, err = pg.ExecContext(ctx, `DELETE FROM sessions WHERE id='shared'`)
	require.NoError(t, err)
	check(1)
	syncer.local = dbtest.RebuildCommitArchive(t, a, config.ArchiveContentUsage)
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	check(1)
	checkDistinct([]string{"dddddddddddddddddddddddddddddddddddddddd"})
	syncer.local = dbtest.RebuildCommitArchive(t, b, config.ArchiveContentUsage)
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	check(0)
	checkDistinct(nil)
}
