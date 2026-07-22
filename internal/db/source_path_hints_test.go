package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListStoredSourcePathHintsScopesByAgentAndRoot(t *testing.T) {

	d := testDB(t)
	root := t.TempDir()
	watchRoot := filepath.Join(root, "db")
	childPath := filepath.Join(watchRoot, "sessions", "a.jsonl")
	virtualPath := filepath.Join(watchRoot, "state.sqlite3") + "#session-a"
	uncleanPath := filepath.Join(watchRoot, "nested", "..", "nested", "b.jsonl")
	cleanPath := filepath.Join(watchRoot, "nested", "b.jsonl")
	siblingPath := filepath.Join(root, "db2", "sessions", "other.jsonl")
	otherAgentPath := filepath.Join(watchRoot, "sessions", "other-agent.jsonl")
	deletedPath := filepath.Join(watchRoot, "sessions", "deleted.jsonl")

	insertSessionWithSourcePath(t, d, "claude:child", "claude", childPath)
	insertSessionWithSourcePath(t, d, "claude:child-dup", "claude", childPath)
	insertSessionWithSourcePath(t, d, "claude:virtual", "claude", virtualPath)
	insertSessionWithSourcePath(t, d, "claude:unclean", "claude", uncleanPath)
	insertSessionWithSourcePath(t, d, "claude:sibling", "claude", siblingPath)
	insertSessionWithSourcePath(t, d, "codex:other-agent", "codex", otherAgentPath)
	insertSessionWithSourcePath(t, d, "claude:deleted", "claude", deletedPath)
	require.NoError(t, d.SoftDeleteSession("claude:deleted"))

	got, err := d.ListStoredSourcePathHints("claude", storedSourcePathHintScopes(
		filepath.Join(watchRoot, "."),
		filepath.Join(root, "db2", "..", "db"),
	))

	require.NoError(t, err)
	assert.Equal(t, []string{
		cleanPath,
		childPath,
		virtualPath,
	}, got)
}

func TestListStoredSourcePathHintsHandlesHashPathsAndVirtualSuffixes(t *testing.T) {

	d := testDB(t)
	base := t.TempDir()

	hashRoot := filepath.Join(base, "db#prod")
	hashChild := filepath.Join(hashRoot, "sessions", "a.jsonl")
	insertSessionWithSourcePath(t, d, "claude:hash-child", "claude", hashChild)

	dbRoot := filepath.Join(base, "state.sqlite3")
	virtualPath := dbRoot + "#session-a"
	insertSessionWithSourcePath(t, d, "claude:virtual", "claude", virtualPath)

	plainRoot := filepath.Join(base, "db")
	hashSibling := filepath.Join(base, "db#backup", "sessions", "b.jsonl")
	hashVirtualSibling := plainRoot + "#session-b"
	insertSessionWithSourcePath(t, d, "claude:hash-sibling", "claude", hashSibling)
	insertSessionWithSourcePath(
		t, d, "claude:hash-virtual-sibling", "claude", hashVirtualSibling,
	)

	got, err := d.ListStoredSourcePathHints("claude", []StoredSourcePathHintScope{
		{Path: hashRoot},
		{Path: dbRoot, IncludeVirtualMembers: true},
		{Path: plainRoot},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		hashChild,
		virtualPath,
	}, got)
}

func TestListStoredSourcePathHintsIncludesDeclaredExtensionlessVirtualMembers(t *testing.T) {
	d := testDB(t)
	base := t.TempDir()
	container := filepath.Join(base, "conversation")
	member := container + "#conversation-a"
	sibling := container + "-sibling#conversation-b"
	child := filepath.Join(container, "child.jsonl")
	insertSessionWithSourcePath(t, d, "visualstudio-copilot:member", "visualstudio-copilot", member)
	insertSessionWithSourcePath(t, d, "visualstudio-copilot:sibling", "visualstudio-copilot", sibling)
	insertSessionWithSourcePath(t, d, "visualstudio-copilot:child", "visualstudio-copilot", child)

	got, err := d.ListStoredSourcePathHints("visualstudio-copilot", []StoredSourcePathHintScope{{
		Path: container, IncludeVirtualMembers: true,
	}})

	require.NoError(t, err)
	assert.Equal(t, []string{member, child}, got)

	ordinary, err := d.ListStoredSourcePathHints("visualstudio-copilot", []StoredSourcePathHintScope{{
		Path: container,
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{child}, ordinary,
		"ordinary prefixes must not claim hash-delimited siblings")
}

// TestListStoredSourcePathHintsLimitVirtualMembersToSingleSegments pins the
// Go re-filter (storedSourcePathHintInRoot) that trims the deliberately wide
// SQL virtual range [root+"#", root+"$") in storedSourcePathHintQuery: rows
// with an empty or nested container suffix are ordinary paths owned by other
// sources and must never surface as hints, or the changed-path force-replace
// path (providerSourceSessionOwnershipsForForceReplace) could tombstone them.
func TestListStoredSourcePathHintsLimitVirtualMembersToSingleSegments(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	stateDB := filepath.Join(root, "state.db")
	member := stateDB + "#member1"
	insertSessionWithSourcePath(t, d, "hermes:member", "hermes", member)
	insertSessionWithSourcePath(
		t, d, "hermes:nested-slash", "hermes", stateDB+"#backup/session.json",
	)
	insertSessionWithSourcePath(
		t, d, "hermes:nested-backslash", "hermes", stateDB+`#a\b`,
	)
	insertSessionWithSourcePath(t, d, "hermes:empty-suffix", "hermes", stateDB+"#")

	got, err := d.ListStoredSourcePathHints("hermes", []StoredSourcePathHintScope{{
		Path: stateDB, IncludeVirtualMembers: true,
	}})

	require.NoError(t, err)
	assert.Equal(t, []string{member}, got,
		"nested and empty container suffixes are ordinary paths, not virtual members")
}

func TestListStoredSourcePathHintsEscapesLikeWildcards(t *testing.T) {

	d := testDB(t)
	base := t.TempDir()
	root := filepath.Join(base, "db%!_root")
	childPath := filepath.Join(root, "session.jsonl")
	insertSessionWithSourcePath(t, d, "claude:wildcard-child", "claude", childPath)

	siblingPath := filepath.Join(base, "dbX!Yroot", "session.jsonl")
	insertSessionWithSourcePath(t, d, "claude:wildcard-sibling", "claude", siblingPath)

	got, err := d.ListStoredSourcePathHints("claude", storedSourcePathHintScopes(root))

	require.NoError(t, err)
	assert.Equal(t, []string{childPath}, got)
}

func TestListStoredSourcePathHintsBatchesRootsWithoutTruncating(t *testing.T) {

	d := testDB(t)
	base := t.TempDir()
	var roots []string
	var seeds []storedSourcePathSeed
	var want []string
	for i := range storedSourcePathHintRootBatchSize + 17 {
		root := filepath.Join(base, fmt.Sprintf("root-%03d", i))
		roots = append(roots, root)
		if i == 0 || i == storedSourcePathHintRootBatchSize+16 {
			path := filepath.Join(root, "session.jsonl")
			seeds = append(seeds, storedSourcePathSeed{
				id:    fmt.Sprintf("claude:match-%03d", i),
				agent: "claude",
				path:  path,
			})
			want = append(want, path)
		}
	}
	for i := range 250 {
		path := filepath.Join(base, "unrelated", fmt.Sprintf("%03d.jsonl", i))
		seeds = append(seeds, storedSourcePathSeed{
			id:    fmt.Sprintf("claude:unrelated-%03d", i),
			agent: "claude",
			path:  path,
		})
	}
	insertSessionsWithSourcePaths(t, d, seeds)

	got, err := d.ListStoredSourcePathHints("claude", storedSourcePathHintScopes(roots...))

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestStoredSourcePathHintsLookupUsesAgentFilePathRangeSeek(t *testing.T) {

	d := testDB(t)
	root := t.TempDir()
	explainSQL, args := storedSourcePathHintQuery("claude", storedSourcePathHintScopes(root))
	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+explainSQL, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())

	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "idx_sessions_agent_file_path_active")
	assert.Contains(t, plan, "file_path>? AND file_path<?",
		"hint lookup must seek affected path ranges, not scan every source for the agent")
}

func storedSourcePathHintScopes(paths ...string) []StoredSourcePathHintScope {
	scopes := make([]StoredSourcePathHintScope, 0, len(paths))
	for _, path := range paths {
		scopes = append(scopes, StoredSourcePathHintScope{Path: path})
	}
	return scopes
}

func TestStoredSourcePathHintVirtualMemberLookupUsesBoundedRangeSeek(t *testing.T) {
	d := testDB(t)
	root := filepath.Join(t.TempDir(), "conversation")
	explainSQL, args := storedSourcePathHintQuery("visualstudio-copilot", []StoredSourcePathHintScope{{
		Path: root, IncludeVirtualMembers: true,
	}})
	rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+explainSQL, args...)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())

	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "idx_sessions_agent_file_path_active")
	assert.GreaterOrEqual(t, strings.Count(plan, "file_path>? AND file_path<?"), 2,
		"directory children and virtual members must each use bounded seeks")
}

func TestListActiveSessionSourceOwnershipScopesPageUsesStableBoundedKeyset(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	seeds := make([]storedSourcePathSeed, 0, WatchReconcileSourcePageSize+3)
	for i := range WatchReconcileSourcePageSize + 3 {
		path := filepath.Join(root, fmt.Sprintf("source-%03d.jsonl", i/2))
		seeds = append(seeds, storedSourcePathSeed{
			id: fmt.Sprintf("session-%03d", i), agent: "claude", path: path,
		})
	}
	insertSessionsWithSourcePaths(t, d, seeds)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, sourcePathsFromSeeds(seeds),
	))

	first, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "claude",
		[]StoredSourcePathHintScope{{Path: root}}, SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, first, WatchReconcileSourcePageSize)
	second, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "claude",
		[]StoredSourcePathHintScope{{Path: root}}, first[len(first)-1].Cursor(),
	)
	require.NoError(t, err)
	require.Len(t, second, 3)

	got := append(first, second...)
	require.Len(t, got, len(seeds))
	for i, ownership := range got {
		assert.Equal(t, seeds[i].id, ownership.ID)
		assert.Equal(t, seeds[i].path, ownership.FilePath)
		assert.Equal(t, "claude", ownership.Agent)
	}
}

func TestListActiveSessionSourceOwnershipScopesPagePagesVirtualMembersOnce(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	ownedRoot := filepath.Join(root, "z-owned")
	stateDB := filepath.Join(ownedRoot, "state.db")
	sessionsDir := filepath.Join(ownedRoot, "sessions")
	seeds := make([]storedSourcePathSeed, 0, WatchReconcileSourcePageSize+4)
	for i := range WatchReconcileSourcePageSize + 3 {
		seeds = append(seeds, storedSourcePathSeed{
			id:    fmt.Sprintf("hermes:%03d", i),
			agent: "hermes",
			path:  fmt.Sprintf("%s#member-%03d", stateDB, i),
		})
	}
	seeds = append(seeds, storedSourcePathSeed{
		id: "hermes:transcript", agent: "hermes",
		path: filepath.Join(sessionsDir, "transcript.jsonl"),
	})
	unrelated := storedSourcePathSeed{
		id: "hermes:unrelated", agent: "hermes",
		path: filepath.Join(root, "other.db") + "#member",
	}
	insertSessionsWithSourcePaths(t, d, append(seeds, unrelated))
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine,
		sourcePathsFromSeeds(append(seeds, unrelated)),
	))

	scopes := make([]StoredSourcePathHintScope, 0,
		watchReconcileOwnershipScopeBatchSize+2)
	for i := range watchReconcileOwnershipScopeBatchSize {
		scopes = append(scopes, StoredSourcePathHintScope{
			Path: filepath.Join(root, fmt.Sprintf("a-empty-scope-%02d", i)),
		})
	}
	scopes = append(scopes,
		StoredSourcePathHintScope{Path: stateDB, IncludeVirtualMembers: true},
		StoredSourcePathHintScope{Path: sessionsDir},
	)
	normalizedScopes := normalizeStoredSourcePathHintScopes(scopes)
	firstMatchingScope := len(normalizedScopes)
	for i, scope := range normalizedScopes {
		if scope.Path == stateDB || scope.Path == sessionsDir {
			firstMatchingScope = i
			break
		}
	}
	require.GreaterOrEqual(t, firstMatchingScope,
		watchReconcileOwnershipScopeBatchSize,
		"all matching scopes must be outside the first SQL batch")
	var got []SessionSourceOwnership
	var cursor SessionSourceCursor
	pageCount := 0
	for {
		page, err := d.ListActiveSessionSourceOwnershipScopesPage(
			t.Context(), defaultMachine, "hermes", scopes, cursor,
		)
		require.NoError(t, err)
		pageCount++
		got = append(got, page...)
		if len(page) < WatchReconcileSourcePageSize {
			break
		}
		cursor = page[len(page)-1].Cursor()
	}

	require.Len(t, got, len(seeds),
		"later batches must page each row once and keep sibling containers excluded")
	assert.GreaterOrEqual(t, pageCount, 2,
		"later-batch ownership must cross the global keyset page boundary")
	gotIDs := make(map[string]struct{}, len(got))
	for _, ownership := range got {
		gotIDs[ownership.ID] = struct{}{}
	}
	for _, seed := range seeds {
		assert.Contains(t, gotIDs, seed.id)
	}
	assert.NotContains(t, gotIDs, unrelated.id)
}

func TestListActiveSessionSourceOwnershipScopesPageBoundsSQLParameters(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	stateDB := filepath.Join(root, "zz-state.db")
	seed := storedSourcePathSeed{
		id: "hermes:member", agent: "hermes", path: stateDB + "#member",
	}
	insertSessionsWithSourcePaths(t, d, []storedSourcePathSeed{seed})
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, sourcePathsFromSeeds([]storedSourcePathSeed{seed}),
	))

	const scopeCount = 8300 // More than 32,766 parameters in the unbatched query.
	scopes := make([]StoredSourcePathHintScope, 0, scopeCount)
	for i := range scopeCount - 1 {
		scopes = append(scopes, StoredSourcePathHintScope{
			Path:                  filepath.Join(root, fmt.Sprintf("aa-unrelated-%05d.db", i)),
			IncludeVirtualMembers: true,
		})
	}
	scopes = append(scopes, StoredSourcePathHintScope{
		Path: stateDB, IncludeVirtualMembers: true,
	})
	normalizedScopes := normalizeStoredSourcePathHintScopes(scopes)
	require.Equal(t, stateDB, normalizedScopes[len(normalizedScopes)-1].Path,
		"the sole matching scope must remain in the final SQL batch")

	page, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "hermes", scopes, SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, seed.id, page[0].ID)
}

func TestListActiveSessionSourceOwnershipScopesPageLimitsVirtualMembersToSingleSegments(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	stateDB := filepath.Join(root, "state.db")
	member := storedSourcePathSeed{
		id: "hermes:member", agent: "hermes", path: stateDB + "#member1",
	}
	seeds := []storedSourcePathSeed{
		member,
		{id: "hermes:nested-slash", agent: "hermes", path: stateDB + "#backup/session.json"},
		{id: "hermes:nested-backslash", agent: "hermes", path: stateDB + `#a\b`},
		{id: "hermes:empty-suffix", agent: "hermes", path: stateDB + "#"},
	}
	insertSessionsWithSourcePaths(t, d, seeds)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, sourcePathsFromSeeds(seeds),
	))

	page, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "hermes",
		[]StoredSourcePathHintScope{{Path: stateDB, IncludeVirtualMembers: true}},
		SessionSourceCursor{},
	)

	require.NoError(t, err)
	require.Len(t, page, 1,
		"nested and empty container suffixes are ordinary paths, not virtual members")
	assert.Equal(t, member.id, page[0].ID)
	assert.Equal(t, member.path, page[0].FilePath)
}

func TestSourceBaselineRequiresObservedExactSameMachineOwnership(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	observed := filepath.Join(root, "observed.jsonl")
	unobserved := filepath.Join(root, "historical.jsonl")
	foreign := filepath.Join(root, "foreign.jsonl")
	insertSessionWithSourcePath(t, d, "observed", "claude", observed)
	insertSessionWithSourcePath(t, d, "historical", "claude", unobserved)
	insertSessionWithSourcePath(t, d, "foreign", "claude", foreign, func(s *Session) {
		s.Machine = "other-machine"
	})

	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, []SessionSourcePath{
			{Agent: "claude", FilePath: observed},
			{Agent: "claude", FilePath: foreign},
		},
	))
	page, err := d.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), defaultMachine, "claude",
		[]StoredSourcePathHintScope{{Path: root}}, SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "observed", page[0].ID)
	assert.Equal(t, defaultMachine, page[0].Machine)

	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "historical", unobserved,
	)
	require.NoError(t, err)
	assert.False(t, changed, "unobserved historical ownership must remain active")
	changed, err = d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "foreign", foreign,
	)
	require.NoError(t, err)
	assert.False(t, changed, "another machine cannot borrow the local baseline")
	changed, err = d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "observed", observed,
	)
	require.NoError(t, err)
	assert.True(t, changed)
}

func TestSourceBaselineDoesNotAuthorizeReassignedPath(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.jsonl")
	newPath := filepath.Join(root, "new.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", oldPath)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine,
		[]SessionSourcePath{{Agent: "claude", FilePath: oldPath}},
	))
	insertSessionWithSourcePath(t, d, "session", "claude", newPath)

	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "session", newPath,
	)
	require.NoError(t, err)
	assert.False(t, changed, "the old exact-path baseline must not authorize a new path")
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine,
		[]SessionSourcePath{{Agent: "claude", FilePath: newPath}},
	))
	changed, err = d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "session", newPath,
	)
	require.NoError(t, err)
	assert.True(t, changed)
}

func TestSourceBaselineTreatsVirtualAndLiteralHashPathsAsExact(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	literal := filepath.Join(root, "project#1", "session.jsonl")
	virtual := filepath.Join(root, "container") + "#member"
	insertSessionWithSourcePath(t, d, "literal", "claude", literal)
	insertSessionWithSourcePath(t, d, "virtual", "aider", virtual)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, []SessionSourcePath{
			{Agent: "claude", FilePath: literal},
			{Agent: "aider", FilePath: virtual},
		},
	))

	for id, want := range map[string]string{
		"literal": literal,
		"virtual": virtual,
	} {
		var got string
		require.NoError(t, d.getReader().QueryRow(
			"SELECT file_path FROM local_session_source_baselines WHERE session_id = ?", id,
		).Scan(&got))
		assert.Equal(t, want, got)
	}
}

func TestSourceBaselineDoesNotRewriteAlreadyObservedOwnership(t *testing.T) {
	d := testDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", path)
	_, err := d.getWriter().Exec(`
		CREATE TABLE source_baseline_update_observer (updates INTEGER NOT NULL);
		INSERT INTO source_baseline_update_observer VALUES (0);
		CREATE TRIGGER observe_source_baseline_insert
		AFTER INSERT ON local_session_source_baselines
		BEGIN
			UPDATE source_baseline_update_observer SET updates = updates + 1;
		END;
		CREATE TRIGGER observe_source_baseline_update
		AFTER UPDATE ON local_session_source_baselines
		BEGIN
			UPDATE source_baseline_update_observer SET updates = updates + 1;
		END`)
	require.NoError(t, err)
	source := []SessionSourcePath{{Agent: "claude", FilePath: path}}
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, source,
	))
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine, source,
	))

	var updates int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT updates FROM source_baseline_update_observer",
	).Scan(&updates))
	assert.Equal(t, 1, updates,
		"stable full syncs must not rewrite baseline rows or grow the WAL")
}

func TestRestoreSessionClearsSourceBaseline(t *testing.T) {
	d := testDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", path)
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), defaultMachine,
		[]SessionSourcePath{{Agent: "claude", FilePath: path}},
	))
	require.NoError(t, d.SoftDeleteSession("session"))
	restored, err := d.RestoreSession("session")
	require.NoError(t, err)
	require.EqualValues(t, 1, restored)

	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "session", path,
	)
	require.NoError(t, err)
	assert.False(t, changed,
		"a user-restored archive row must remain visible until its source is observed again")
}

func TestSoftDeleteSessionSourceOwnershipRequiresExactCurrentOwner(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.jsonl")
	newPath := filepath.Join(root, "new.jsonl")
	insertSessionWithSourcePath(t, d, "same-id", "claude", newPath)
	insertSessionWithSourcePath(t, d, "other-agent", "codex", oldPath)
	baselineSessionSource(t, d, defaultMachine, "claude", newPath)
	baselineSessionSource(t, d, defaultMachine, "codex", oldPath)

	deleted, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "same-id", oldPath,
	)
	require.NoError(t, err)
	assert.False(t, deleted, "a same-ID replacement at a new path is not the missing owner")
	deleted, err = d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "other-agent", oldPath,
	)
	require.NoError(t, err)
	assert.False(t, deleted, "another agent cannot be tombstoned through shared path ownership")
	deleted, err = d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "same-id", newPath,
	)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestSourceMissingOwnershipDoesNotSatisfyFreshnessLookups(t *testing.T) {
	d := testDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	userDeletedPath := filepath.Join(t.TempDir(), "user-deleted.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", path)
	insertSessionWithSourcePath(t, d, "user-deleted", "claude", userDeletedPath)
	_, err := d.getWriter().Exec(
		`UPDATE sessions
		 SET file_size = 4096, file_mtime = 1234,
		     file_hash = 'unchanged', data_version = ?
		 WHERE id IN ('session', 'user-deleted')`,
		CurrentDataVersion(),
	)
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession("user-deleted"))
	baselineSessionSource(t, d, defaultMachine, "claude", path)

	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "session", path,
	)
	require.NoError(t, err)
	require.True(t, changed)

	_, _, ok := d.GetFileInfoByPath(path)
	assert.False(t, ok)
	_, ok = d.GetFileHashByPath(path)
	assert.False(t, ok)
	assert.Zero(t, d.GetDataVersionByPath(path))
	_, _, _, _, ok = d.GetSourceRepairStateByPath(path)
	assert.False(t, ok)
	_, _, ok = d.GetSessionFileInfo("session")
	assert.False(t, ok)
	_, ok = d.GetSessionFileHash("session")
	assert.False(t, ok)

	storedSize, storedMtime, ok := d.GetFileInfoByPath(userDeletedPath)
	assert.True(t, ok, "ordinary user trash still suppresses redundant source parsing")
	assert.EqualValues(t, 4096, storedSize)
	assert.EqualValues(t, 1234, storedMtime)
	storedSize, storedMtime, ok = d.GetSessionFileInfo("user-deleted")
	assert.True(t, ok, "ordinary user trash retains legacy ID freshness semantics")
	assert.EqualValues(t, 4096, storedSize)
	assert.EqualValues(t, 1234, storedMtime)
	storedHash, ok := d.GetSessionFileHash("user-deleted")
	assert.True(t, ok)
	assert.Equal(t, "unchanged", storedHash)
}

func TestSharedPathSourceOwnershipPageAllocationsStayBoundedByPage(t *testing.T) {
	seed := func(t *testing.T, count int) (*DB, string) {
		t.Helper()
		d := testDB(t)
		root := t.TempDir()
		sharedPath := filepath.Join(root, "shared.jsonl")
		seeds := make([]storedSourcePathSeed, 0, count)
		for i := range count {
			seeds = append(seeds, storedSourcePathSeed{
				id: fmt.Sprintf("session-%05d", i), agent: "claude",
				path: sharedPath,
			})
		}
		insertSessionsWithSourcePaths(t, d, seeds)
		require.NoError(t, d.BaselineActiveSessionSourcePaths(
			t.Context(), defaultMachine, sourcePathsFromSeeds(seeds),
		))
		return d, root
	}
	smallDB, smallRoot := seed(t, WatchReconcileSourcePageSize)
	largeDB, largeRoot := seed(t, WatchReconcileSourcePageSize*20)

	measure := func(database *DB, root string) float64 {
		return testing.AllocsPerRun(10, func() {
			page, err := database.ListActiveSessionSourceOwnershipScopesPage(
				t.Context(), defaultMachine, "claude",
				[]StoredSourcePathHintScope{{Path: root}}, SessionSourceCursor{},
			)
			require.NoError(t, err)
			require.Len(t, page, WatchReconcileSourcePageSize)
		})
	}
	smallAllocs := measure(smallDB, smallRoot)
	largeAllocs := measure(largeDB, largeRoot)

	assert.LessOrEqual(t, largeAllocs, smallAllocs+16,
		"LIMIT must stop inside a large equal-path group without materializing it")
}

func TestActiveSessionSourceOwnershipPageUsesOrderedCoveringIndex(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	likeRoot := sqliteLikeEscape(root)
	rows, err := d.getReader().Query(`
		EXPLAIN QUERY PLAN
		SELECT s.machine, s.agent, s.id, s.file_path
		FROM local_session_source_baselines AS b
		JOIN sessions AS s
		  ON s.id = b.session_id
		 AND s.machine = b.machine
		 AND s.agent = b.agent
		 AND s.file_path = b.file_path
		WHERE b.machine = ?
		  AND b.agent = ?
		  AND s.deleted_at IS NULL
		  AND (b.file_path = ? OR b.file_path LIKE ? ESCAPE '!')
		  AND (b.file_path > ? OR (b.file_path = ? AND b.session_id > ?))
		ORDER BY b.file_path, b.session_id
		LIMIT ?`,
		defaultMachine, "claude", root, likeRoot+string(filepath.Separator)+"%",
		"", "", "", WatchReconcileSourcePageSize,
	)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "idx_local_source_baselines_ownership")
	assert.NotContains(t, plan, "USE TEMP B-TREE",
		"equal-path ownership rows must stream in id order before LIMIT")
}

func TestSessionSourceOwnershipAPIsHonorCanceledContext(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	insertSessionWithSourcePath(t, d, "session", "claude", path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := d.ListActiveSessionSourceOwnershipScopesPage(
		ctx, defaultMachine, "claude",
		[]StoredSourcePathHintScope{{Path: root}}, SessionSourceCursor{},
	)
	require.ErrorIs(t, err, context.Canceled)
	_, err = d.SoftDeleteSessionSourceOwnership(
		ctx, defaultMachine, "claude", "session", path,
	)
	require.ErrorIs(t, err, context.Canceled)
	active, err := d.GetSession(t.Context(), "session")
	require.NoError(t, err)
	assert.NotNil(t, active, "a canceled tombstone must not mutate the row")
}

func TestSourceMissingTombstoneRevivesThroughEverySessionUpsert(t *testing.T) {
	tests := []struct {
		name   string
		upsert func(*testing.T, *DB, Session) error
	}{
		{
			name: "single upsert",
			upsert: func(t *testing.T, d *DB, session Session) error {
				t.Helper()
				return d.UpsertSession(session)
			},
		},
		{
			name: "atomic batch upsert",
			upsert: func(t *testing.T, d *DB, session Session) error {
				t.Helper()
				result, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{{
					Session: session, DataVersion: CurrentDataVersion(),
				}})
				require.Equal(t, 1, result.WrittenSessions)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			root := t.TempDir()
			path := filepath.Join(root, "session.jsonl")
			insertSessionWithSourcePath(t, d, "session", "claude", path)
			baselineSessionSource(t, d, defaultMachine, "claude", path)
			changed, err := d.SoftDeleteSessionSourceOwnership(
				t.Context(), defaultMachine, "claude", "session", path,
			)
			require.NoError(t, err)
			require.True(t, changed)

			var deletedAt, cause *string
			require.NoError(t, d.getReader().QueryRow(
				"SELECT deleted_at, deletion_cause FROM sessions WHERE id = ?",
				"session",
			).Scan(&deletedAt, &cause))
			require.NotNil(t, deletedAt)
			require.NotNil(t, cause)
			assert.Equal(t, "source_missing", *cause)

			err = tt.upsert(t, d, Session{
				ID: "session", Project: "reappeared", Machine: defaultMachine,
				Agent: "claude", FilePath: &path,
			})
			require.NoError(t, err)
			require.NoError(t, d.getReader().QueryRow(
				"SELECT deleted_at, deletion_cause FROM sessions WHERE id = ?",
				"session",
			).Scan(&deletedAt, &cause))
			assert.Nil(t, deletedAt)
			assert.Nil(t, cause)
			active, err := d.GetSession(t.Context(), "session")
			require.NoError(t, err)
			require.NotNil(t, active)
			assert.Equal(t, "reappeared", active.Project)
		})
	}
}

func TestWriteSessionBatchSourceMissingRevivalReplacesRetainedMessages(
	t *testing.T,
) {
	d := testDB(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	session := Session{
		ID: "session", Project: "project", Machine: defaultMachine,
		Agent: "claude", FilePath: &path, MessageCount: 1,
	}
	write := func(content string) {
		t.Helper()
		result, err := d.WriteSessionBatch([]SessionBatchWrite{{
			Session: session,
			Messages: []Message{{
				SessionID: "session", Ordinal: 0, Role: "user", Content: content,
			}},
			DataVersion: CurrentDataVersion(),
		}})
		require.NoError(t, err)
		require.Equal(t, 1, result.WrittenSessions)
	}

	write("old content")
	baselineSessionSource(t, d, defaultMachine, "claude", path)
	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "session", path,
	)
	require.NoError(t, err)
	require.True(t, changed)

	write("new content")
	messages, err := d.GetAllMessages(t.Context(), "session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "new content", messages[0].Content,
		"source-missing revival must override append-only batch hints")
}

func TestUserTrashRemainsRejectedByEverySessionUpsert(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "project")
	require.NoError(t, d.SoftDeleteSession("session"))

	err := d.UpsertSession(Session{ID: "session", Project: "changed"})
	require.ErrorIs(t, err, ErrSessionTrashed)
	result, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session:     Session{ID: "session", Project: "changed"},
		DataVersion: CurrentDataVersion(),
	}})
	require.ErrorIs(t, err, ErrSessionTrashed)
	assert.Equal(t, 1, result.ExcludedSessions)

	var cause *string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT deletion_cause FROM sessions WHERE id = ?", "session",
	).Scan(&cause))
	assert.Nil(t, cause, "legacy and user trash keep the established NULL cause")
}

func TestSourceMissingTombstoneIsNotUserTrash(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	missingPath := filepath.Join(root, "missing.jsonl")
	userPath := filepath.Join(root, "user.jsonl")
	insertSessionWithSourcePath(t, d, "missing", "claude", missingPath)
	insertSessionWithSourcePath(t, d, "user", "claude", userPath)
	baselineSessionSource(t, d, defaultMachine, "claude", missingPath)
	changed, err := d.SoftDeleteSessionSourceOwnership(
		t.Context(), defaultMachine, "claude", "missing", missingPath,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, d.SoftDeleteSession("user"))

	assert.False(t, d.IsSessionTrashed("missing"))
	assert.True(t, d.IsSessionTrashed("user"))
	assert.False(t, d.HasTrashedSessionByFilePath(missingPath, "claude"))
	assert.True(t, d.HasTrashedSessionByFilePath(userPath, "claude"))
	trashed, err := d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, trashed, 1)
	assert.Equal(t, "user", trashed[0].ID)

	emptied, err := d.EmptyTrash()
	require.NoError(t, err)
	assert.Equal(t, 1, emptied)
	assert.False(t, d.IsSessionExcluded("missing"))
	assertDeletionState(t, d, "missing", true, deletionCauseSourceMissing)
}

func insertSessionWithSourcePath(
	t *testing.T,
	d *DB,
	id string,
	agent string,
	path string,
	opts ...func(*Session),
) {
	t.Helper()
	insertSession(t, d, id, "proj", append([]func(*Session){
		func(s *Session) {
			s.Agent = agent
			s.FilePath = &path
		},
	}, opts...)...)
}

type storedSourcePathSeed struct {
	id    string
	agent string
	path  string
}

func sourcePathsFromSeeds(seeds []storedSourcePathSeed) []SessionSourcePath {
	paths := make([]SessionSourcePath, 0, len(seeds))
	for _, seed := range seeds {
		paths = append(paths, SessionSourcePath{
			Agent: seed.agent, FilePath: seed.path,
		})
	}
	return paths
}

func baselineSessionSource(
	t *testing.T, d *DB, machine string, agent string, path string,
) {
	t.Helper()
	require.NoError(t, d.BaselineActiveSessionSourcePaths(
		t.Context(), machine,
		[]SessionSourcePath{{Agent: agent, FilePath: path}},
	))
}

// totalWriterChanges reads the writer connection's cumulative count of rows
// inserted, updated, or deleted. The writer pool holds a single connection,
// so deltas measure exactly the rows a write path touched.
func totalWriterChanges(t *testing.T, d *DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t,
		d.getWriter().QueryRow("SELECT total_changes()").Scan(&n))
	return n
}

// listBaselineOwnership returns every local_session_source_baselines row as
// session_id -> (agent, file_path) for the machine.
func listBaselineOwnership(
	t *testing.T, d *DB, machine string,
) map[string]SessionSourcePath {
	t.Helper()
	rows, err := d.Reader().Query(`
		SELECT session_id, agent, file_path
		FROM local_session_source_baselines WHERE machine = ?`, machine)
	require.NoError(t, err)
	defer rows.Close()
	ownership := make(map[string]SessionSourcePath)
	for rows.Next() {
		var id string
		var source SessionSourcePath
		require.NoError(t, rows.Scan(&id, &source.Agent, &source.FilePath))
		ownership[id] = source
	}
	require.NoError(t, rows.Err())
	return ownership
}

// TestReplaceActiveSessionSourceBaselinesWarmPassWritesBounded pins the
// AGENTS.md cardinality rule for the watcher-proof table: a warm no-op full
// sync replays every unchanged source as an admitted candidate, and the
// replacement must not rewrite their baseline rows — write work scales with
// the changed batch, never the archive. Rejection (proof withdrawal) and
// tombstoned-session cleanup must keep working in the same pass.
func TestReplaceActiveSessionSourceBaselinesWarmPassWritesBounded(t *testing.T) {
	for _, total := range []int{3, 2*baselinePairChunk + 7} {
		t.Run(fmt.Sprintf("sessions-%d", total), func(t *testing.T) {
			d := testDB(t)
			root := t.TempDir()
			seeds := make([]storedSourcePathSeed, 0, total)
			for i := range total {
				seeds = append(seeds, storedSourcePathSeed{
					id:    fmt.Sprintf("claude:s-%04d", i),
					agent: "claude",
					path:  filepath.Join(root, fmt.Sprintf("s-%04d.jsonl", i)),
				})
			}
			insertSessionsWithSourcePaths(t, d, seeds)
			sources := sourcePathsFromSeeds(seeds)
			require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
				t.Context(), defaultMachine, sources, sources,
			))
			require.Len(t, listBaselineOwnership(t, d, defaultMachine), total)

			before := totalWriterChanges(t, d)
			require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
				t.Context(), defaultMachine, sources, sources,
			))
			assert.Zero(t, totalWriterChanges(t, d)-before,
				"a warm no-op pass must not rewrite unchanged baseline rows")

			// One rejected source and one tombstoned session: the pass must
			// withdraw exactly those two proofs, independent of archive size.
			require.NoError(t, d.SoftDeleteSession(seeds[1].id))
			before = totalWriterChanges(t, d)
			require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
				t.Context(), defaultMachine, sources, sources[1:],
			))
			assert.Equal(t, int64(2), totalWriterChanges(t, d)-before,
				"proof withdrawal must write only the changed rows")
			ownership := listBaselineOwnership(t, d, defaultMachine)
			assert.Len(t, ownership, total-2)
			assert.NotContains(t, ownership, seeds[0].id,
				"a rejected candidate must lose its deletion proof")
			assert.NotContains(t, ownership, seeds[1].id,
				"a tombstoned session must lose its deletion proof")
		})
	}
}

// TestReplaceActiveSessionSourceBaselinesReplacesMovedOwnership pins the
// delete-then-readmit end state for changed ownership: when a session's stored
// source moves, replacing the old and new pairs leaves exactly one baseline
// row carrying the new ownership.
func TestReplaceActiveSessionSourceBaselinesReplacesMovedOwnership(t *testing.T) {
	d := testDB(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.jsonl")
	newPath := filepath.Join(root, "new.jsonl")
	insertSessionWithSourcePath(t, d, "claude:moved", "claude", oldPath)
	oldSources := []SessionSourcePath{{Agent: "claude", FilePath: oldPath}}
	require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
		t.Context(), defaultMachine, oldSources, oldSources,
	))

	insertSessionWithSourcePath(t, d, "claude:moved", "claude", newPath)
	both := []SessionSourcePath{
		{Agent: "claude", FilePath: oldPath},
		{Agent: "claude", FilePath: newPath},
	}
	require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
		t.Context(), defaultMachine, both, both,
	))

	ownership := listBaselineOwnership(t, d, defaultMachine)
	require.Len(t, ownership, 1)
	assert.Equal(t, SessionSourcePath{Agent: "claude", FilePath: newPath},
		ownership["claude:moved"],
		"the baseline must follow the session to its new source")

	// A move whose destination is outside the replaced page still withdraws
	// the old proof: the row is no longer backed by a session with that
	// exact ownership.
	elsewhere := filepath.Join(root, "elsewhere.jsonl")
	insertSessionWithSourcePath(t, d, "claude:moved", "claude", elsewhere)
	require.NoError(t, d.ReplaceActiveSessionSourceBaselines(
		t.Context(), defaultMachine,
		[]SessionSourcePath{{Agent: "claude", FilePath: newPath}},
		[]SessionSourcePath{{Agent: "claude", FilePath: newPath}},
	))
	assert.Empty(t, listBaselineOwnership(t, d, defaultMachine),
		"proof for the vacated source must not outlive the move")
}

func insertSessionsWithSourcePaths(
	t *testing.T,
	d *DB,
	seeds []storedSourcePathSeed,
) {
	t.Helper()

	writes := make([]SessionBatchWrite, 0, len(seeds))
	for _, seed := range seeds {
		path := seed.path
		writes = append(writes, SessionBatchWrite{
			Session: Session{
				ID:           seed.id,
				Project:      "proj",
				Machine:      defaultMachine,
				Agent:        seed.agent,
				MessageCount: 1,
				FilePath:     &path,
			},
			DataVersion: CurrentDataVersion(),
		})
	}
	result, err := d.WriteSessionBatchAtomic(writes)
	require.NoError(t, err, "insert source path sessions")
	require.Equal(t, len(seeds), result.WrittenSessions, "WrittenSessions")
}
