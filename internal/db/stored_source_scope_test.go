package db

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListActiveSessionSourceOwnershipScopesPagePagesChildrenOfASeparatorRoot
// covers the root spelling that already ends in a separator. A drive root or
// share root is its own parent, so appending a second separator to build the
// child prefix produces a pattern no stored child matches, and the pass pages
// zero rows and tombstones nothing while still crediting coverage.
func TestListActiveSessionSourceOwnershipScopesPagePagesChildrenOfASeparatorRoot(
	t *testing.T,
) {
	if runtime.GOOS != "windows" {
		t.Skip("only a Windows drive root survives filepath.Clean with a trailing separator")
	}
	d := testDB(t)
	// A drive root is the one spelling filepath.Clean cannot shorten, so it
	// still carries its separator when the query is built. The paths are
	// stored strings; no file has to exist for the predicate to be exercised.
	const root = `C:\`
	const child = `C:\session.jsonl`
	seeds := []storedSourcePathSeed{
		{id: "child", agent: "claude", path: child},
	}
	insertSessionsWithSourcePaths(t, d, seeds)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, sourcePathsFromSeeds(seeds),
	))

	page, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "claude",
		[]StoredSourcePathHintScope{{Path: root}}, SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, page, 1,
		"a child of a separator-terminated root must page")
	assert.Equal(t, "child", page[0].ID)
}

// TestListActiveSessionSourceOwnershipScopesPageExcludesOutOfScopeRows bounds
// the query itself rather than what the engine does with its result. The
// engine skips an out-of-scope row before it ever stats the file, so a test
// that only records stat calls passes just as well against a widened query.
// The out-of-scope rows here sort between the in-scope rows and span a page
// boundary, so a query widened to the parent would surface them in the first
// page.
func TestListActiveSessionSourceOwnershipScopesPageExcludesOutOfScopeRows(
	t *testing.T,
) {
	d := testDB(t)
	root := t.TempDir()
	inScope := filepath.Join(root, "in")
	outOfScope := filepath.Join(root, "in-sibling")

	var seeds []storedSourcePathSeed
	wantIDs := make([]string, 0, WatchReconcileSourcePageSize+2)
	for i := range WatchReconcileSourcePageSize + 2 {
		id := fmt.Sprintf("in-%03d", i)
		wantIDs = append(wantIDs, id)
		seeds = append(seeds, storedSourcePathSeed{
			id:    id,
			agent: "claude",
			path:  filepath.Join(inScope, fmt.Sprintf("source-%03d.jsonl", i)),
		})
		seeds = append(seeds, storedSourcePathSeed{
			id:    fmt.Sprintf("out-%03d", i),
			agent: "claude",
			path: filepath.Join(
				outOfScope, fmt.Sprintf("source-%03d.jsonl", i),
			),
		})
	}
	insertSessionsWithSourcePaths(t, d, seeds)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, sourcePathsFromSeeds(seeds),
	))

	scopes := []StoredSourcePathHintScope{{Path: inScope}}
	var paged []SessionSourceOwnership
	cursor := SessionSourceCursor{}
	for range 8 {
		page, err := d.ListActiveSessionSourceOwnershipScopesPage(
			t.Context(), defaultMachine, "claude", scopes, cursor,
		)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		cursor = page[len(page)-1].Cursor()
	}

	gotIDs := make([]string, 0, len(paged))
	for _, ownership := range paged {
		assert.True(t,
			StoredSourcePathHintScopesContain(ownership.FilePath, scopes),
			"paged row %s at %s is outside the requested scope",
			ownership.ID, ownership.FilePath)
		gotIDs = append(gotIDs, ownership.ID)
	}
	assert.Equal(t, wantIDs, gotIDs,
		"every in-scope row pages exactly once and no sibling row joins it")
}

func TestStoredSourcePathHintScopesContainDirectoryAndMemberBranches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archive")
	memberScope := []StoredSourcePathHintScope{{
		Path: filepath.Join(root, "state.db"), IncludeVirtualMembers: true,
	}}
	directoryScope := []StoredSourcePathHintScope{{Path: root}}

	assert.True(t, StoredSourcePathHintScopesContain(root, directoryScope))
	assert.True(t, StoredSourcePathHintScopesContain(
		filepath.Join(root, "sessions", "a.jsonl"), directoryScope,
	))
	assert.False(t, StoredSourcePathHintScopesContain(
		filepath.Join(filepath.Dir(root), "sibling", "a.jsonl"), directoryScope,
	))
	assert.False(t, StoredSourcePathHintScopesContain(root+"suffix", directoryScope),
		"a sibling sharing the root as a string prefix is outside the scope")

	member := filepath.Join(root, "state.db") + "#session-1"
	assert.True(t, StoredSourcePathHintScopesContain(member, memberScope))
	assert.False(t, StoredSourcePathHintScopesContain(
		member,
		[]StoredSourcePathHintScope{{Path: filepath.Join(root, "state.db")}},
	), "virtual members of the scope path itself require the provider's declaration")
	assert.False(t, StoredSourcePathHintScopesContain(
		filepath.Join(root, "state.db")+"#nested/session.json", memberScope,
	), "nested member segments belong to other sources")
}

func TestStoredSourcePathHintScopesContainMatchesPlatformCase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Archive")
	variant := strings.ToUpper(root)
	if variant == root {
		t.Skip("path has no case variant")
	}
	scope := []StoredSourcePathHintScope{{Path: variant}}
	memberScope := []StoredSourcePathHintScope{{
		Path:                  strings.ToUpper(filepath.Join(root, "state.db")),
		IncludeVirtualMembers: true,
	}}
	inRoot := filepath.Join(root, "sessions", "a.jsonl")
	member := filepath.Join(root, "state.db") + "#session-1"

	if runtime.GOOS == "windows" {
		assert.True(t, StoredSourcePathHintScopesContain(inRoot, scope),
			"Windows containment folds case like the filesystem")
		assert.True(t, StoredSourcePathHintScopesContain(member, memberScope),
			"the member container prefix folds like the directory branch")
	} else {
		assert.False(t, StoredSourcePathHintScopesContain(inRoot, scope),
			"case-sensitive filesystems keep byte-exact containment")
		assert.False(t, StoredSourcePathHintScopesContain(member, memberScope))
	}
}
