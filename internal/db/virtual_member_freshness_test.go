package db

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedVirtualMemberRow(
	t *testing.T, d *DB, id, path string, mtimeNS int64, version int,
	hash string,
) {
	t.Helper()
	session := Session{
		ID: id, Project: "proj", Machine: defaultMachine, Agent: "opencode",
		FilePath: &path, FileMtime: &mtimeNS,
	}
	if hash != "" {
		session.FileHash = &hash
	}
	require.NoError(t, d.UpsertSession(t.Context(), session))
	require.NoError(t, d.SetSessionDataVersion(t.Context(), id, version))
}

// TestListVirtualContainerMemberFreshnessPagePagesCompleteFolds pins the
// paged freshness read: pages arrive in ascending path order honoring the
// limit, duplicate session rows for one member fold completely even when the
// page boundary lands on that member (the MAX-mtime row carries its hash
// while the MIN data version survives), source-missing rows and paths
// outside the container never surface, and the done flag reports exhaustion.
func TestListVirtualContainerMemberFreshnessPagePagesCompleteFolds(
	t *testing.T,
) {
	d := testDB(t)
	const container = "/data/opencode.db"
	// Member "a" has three rows. The first newest row supplies the hash,
	// and the lower data version survives the fold.
	seedVirtualMemberRow(t, d, "a-old", container+"#a", 100, 5, "old-hash")
	seedVirtualMemberRow(t, d, "a-new", container+"#a", 200, 5, "new-hash")
	seedVirtualMemberRow(t, d, "a-tie", container+"#a", 200, 2, "tie-hash")
	seedVirtualMemberRow(t, d, "b", container+"#b", 150, 5, "")
	seedVirtualMemberRow(t, d, "c", container+"#c", 300, 5, "")
	seedVirtualMemberRow(t, d, "outside", "/data/other.db#x", 400, 5, "")
	_, err := d.getWriter().Exec(t.Context(),
		`UPDATE sessions SET
			source_missing_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = 'b'`,
	)
	require.NoError(t, err, "mark member b source-missing")

	page, done, err := d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", "", 1,
	)
	require.NoError(t, err)
	assert.False(t, done, "a full page must not report exhaustion")
	require.Len(t, page, 1)
	assert.Equal(t, container+"#a", page[0].Path)
	assert.Equal(t, int64(200), page[0].MTimeNS,
		"the fold surfaces the newest row's mtime")
	assert.Equal(t, "opencode", page[0].Agent)
	assert.Equal(t, "proj", page[0].Project)
	assert.Equal(t, "new-hash", page[0].Hash,
		"the first rowid wins when newest mtimes tie")
	assert.Equal(t, 2, page[0].DataVersion,
		"the minimum stored data version survives the fold")

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", page[0].Path, 1,
	)
	require.NoError(t, err)
	assert.False(t, done)
	require.Len(t, page, 1)
	assert.Equal(t, container+"#c", page[0].Path,
		"the source-missing member must never surface")

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", page[0].Path, 1,
	)
	require.NoError(t, err)
	assert.True(t, done, "an empty page reports exhaustion")
	assert.Empty(t, page)

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", "", 10,
	)
	require.NoError(t, err)
	assert.True(t, done, "a short page reports exhaustion")
	require.Len(t, page, 2,
		"one call over a large limit folds the whole membership")
	assert.Equal(t, container+"#a", page[0].Path)
	assert.Equal(t, container+"#c", page[1].Path)
}

func TestVirtualContainerMemberFreshnessStreamingPlan(t *testing.T) {
	d := testDB(t)
	for _, op := range []string{">=", ">"} {
		t.Run(op, func(t *testing.T) {
			plan := queryPlanOf(t, d, fmt.Sprintf(virtualContainerMemberFreshnessQuery, op), "/data/opencode.db#", "/data/opencode.db")
			assert.NotContains(t, plan, "TEMP B-TREE", "pages must stream without sorting the container")
		})
	}
}

func TestVirtualContainerMemberFreshnessGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("measures full container scans at two sizes")
	}
	var medians []time.Duration
	for _, count := range []int{5000, 50000} {
		d := testDB(t)
		const container = "/data/opencode.db"
		_, err := d.getWriter().Exec(t.Context(), `
			WITH RECURSIVE members(n) AS (
				SELECT 1 UNION ALL SELECT n + 1 FROM members WHERE n < ?
			)
			INSERT INTO sessions (id, project, agent, file_path, file_mtime, data_version, file_hash)
			SELECT printf('member-%05d', n), 'proj', 'opencode',
				? || printf('#%05d', n), 100, 5, 'hash' FROM members`, count, container)
		require.NoError(t, err)
		var elapsed []time.Duration
		for pass := range 8 {
			start := time.Now()
			after, total := "", 0
			for {
				page, done, err := d.ListVirtualContainerMemberFreshnessPage(t.Context(), container, "opencode:", after, 256)
				require.NoError(t, err)
				total += len(page)
				if done {
					break
				}
				require.NotEmpty(t, page)
				after = page[len(page)-1].Path
			}
			duration := time.Since(start)
			require.Equal(t, count, total)
			if pass > 0 {
				elapsed = append(elapsed, duration)
			}
		}
		slices.Sort(elapsed)
		medians = append(medians, elapsed[len(elapsed)/2])
	}
	ratio := float64(medians[1]) / float64(medians[0])
	t.Logf("5000 members: %s; 50000 members: %s; growth ratio: %.2fx", medians[0], medians[1], ratio)
	// Leave room for CI noise around linear growth while rejecting the
	// roughly 100x growth of rescanning the remaining container on each page.
	assert.LessOrEqual(t, ratio, 30.0, "10x membership must remain near linear")
}

// TestListVirtualContainerMemberFreshnessPageMarksTrashedAndExcluded pins the
// suppression rows: a trashed member carries Trashed, a permanently deleted
// member surfaces from excluded_sessions at its virtual path marked Excluded,
// both merge into ascending path order across page boundaries, and an empty
// ID prefix leaves excluded IDs out.
func TestListVirtualContainerMemberFreshnessPageMarksTrashedAndExcluded(
	t *testing.T,
) {
	d := testDB(t)
	const container = "/data/state.vscdb"
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		seedVirtualMemberRow(t, d, "cursor-ide:"+id, container+"#"+id, 100, 5, "h-"+id)
	}
	seedVirtualMemberRow(t, d, "other:z", "/data/other.db#z", 100, 5, "")
	require.NoError(t, d.SoftDeleteSession(t.Context(), "cursor-ide:c"))
	require.NoError(t, d.DeleteSession(t.Context(), "cursor-ide:b"))
	require.NoError(t, d.DeleteSession(t.Context(), "cursor-ide:d"))
	require.NoError(t, d.DeleteSession(t.Context(), "other:z"))

	type got struct {
		path              string
		trashed, excluded bool
	}
	var pages [][]got
	var dones []bool
	after := ""
	for range 4 {
		page, done, err := d.ListVirtualContainerMemberFreshnessPage(
			t.Context(), container, "cursor-ide:", after, 2,
		)
		require.NoError(t, err)
		var rows []got
		for _, row := range page {
			rows = append(rows, got{row.Path, row.Trashed, row.Excluded})
		}
		pages = append(pages, rows)
		dones = append(dones, done)
		if done {
			break
		}
		after = page[len(page)-1].Path
	}
	assert.Equal(t, [][]got{
		{{container + "#a", false, false}, {container + "#b", false, true}},
		{{container + "#c", true, false}, {container + "#d", false, true}},
		{{container + "#e", false, false}},
	}, pages)
	assert.Equal(t, []bool{false, false, true}, dones)

	page, done, err := d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", "", 10,
	)
	require.NoError(t, err)
	assert.True(t, done)
	var paths []string
	for _, row := range page {
		paths = append(paths, row.Path)
		assert.False(t, row.Excluded, row.Path)
	}
	assert.Equal(t, []string{container + "#a", container + "#c", container + "#e"}, paths)
}
