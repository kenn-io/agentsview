package db

import (
	"testing"

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
	require.NoError(t, d.UpsertSession(session))
	require.NoError(t, d.SetSessionDataVersion(id, version))
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
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	const container = "/data/opencode.db"
	// Member "a" is stored twice: the newer row carries the hash the fold
	// must surface and the lower data version the fold must keep.
	seedVirtualMemberRow(t, d, "a-old", container+"#a", 100, 5, "old-hash")
	seedVirtualMemberRow(t, d, "a-new", container+"#a", 200, 2, "new-hash")
	seedVirtualMemberRow(t, d, "b", container+"#b", 150, 5, "")
	seedVirtualMemberRow(t, d, "c", container+"#c", 300, 5, "")
	seedVirtualMemberRow(t, d, "outside", "/data/other.db#x", 400, 5, "")
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET
			source_missing_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = 'b'`,
	)
	require.NoError(err, "mark member b source-missing")

	page, done, err := d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", 1,
	)
	require.NoError(err)
	assert.False(done, "a full page must not report exhaustion")
	require.Len(page, 1)
	assert.Equal(container+"#a", page[0].Path)
	assert.Equal(int64(200), page[0].MTimeNS,
		"the fold surfaces the newest row's mtime")
	assert.Equal("new-hash", page[0].Hash,
		"the hash rides the newest-mtime row")
	assert.Equal(2, page[0].DataVersion,
		"the minimum stored data version survives the fold")

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, page[0].Path, 1,
	)
	require.NoError(err)
	assert.False(done)
	require.Len(page, 1)
	assert.Equal(container+"#c", page[0].Path,
		"the source-missing member must never surface")

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, page[0].Path, 1,
	)
	require.NoError(err)
	assert.True(done, "an empty page reports exhaustion")
	assert.Empty(page)

	page, done, err = d.ListVirtualContainerMemberFreshnessPage(
		t.Context(), container, "", 10,
	)
	require.NoError(err)
	assert.True(done, "a short page reports exhaustion")
	require.Len(page, 2,
		"one call over a large limit folds the whole membership")
	assert.Equal(container+"#a", page[0].Path)
	assert.Equal(container+"#c", page[1].Path)
}
