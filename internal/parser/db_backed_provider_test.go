package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseDBBackedAll parses every session in a db-backed agent's database through
// the provider facade (Discover + Fingerprint + Parse), the path production
// uses. It replaces the per-agent whole-database parse free functions
// (ParseForgeDB/ParseWarpDB) that were folded onto the provider, so the legacy
// parse tests exercise the provider rather than a deleted shim.
func parseDBBackedAll(
	agent AgentType, dbPath, machine string,
) ([]ParseResult, error) {
	provider, ok := NewProvider(agent, ProviderConfig{
		Roots:   []string{filepath.Dir(dbPath)},
		Machine: machine,
	})
	if !ok {
		return nil, fmt.Errorf("no provider registered for %s", agent)
	}
	sources, err := provider.Discover(context.Background())
	if err != nil {
		return nil, err
	}
	var out []ParseResult
	for _, src := range sources {
		fp, err := provider.Fingerprint(context.Background(), src)
		if err != nil {
			return nil, err
		}
		outcome, err := provider.Parse(context.Background(), ParseRequest{
			Source:      src,
			Fingerprint: fp,
			Machine:     machine,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range outcome.Results {
			out = append(out, r.Result)
		}
	}
	return out, nil
}

// parseForgeAll and parseWarpAll drive the whole Forge/Warp database through the
// provider, standing in for the deleted ParseForgeDB/ParseWarpDB free functions
// in the retained per-agent parse tests.
func parseForgeAll(dbPath, machine string) ([]ParseResult, error) {
	return parseDBBackedAll(AgentForge, dbPath, machine)
}

func parseWarpAll(dbPath, machine string) ([]ParseResult, error) {
	return parseDBBackedAll(AgentWarp, dbPath, machine)
}

func TestForgeProviderSourceMethodsAndParse(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentForge, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	assertDBBackedWatchPlan(t, provider, root, ForgeDBFilename)
	assertDBBackedDiscoverFindFingerprint(
		t, provider, root, dbPath, "conv-001",
	)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "conv-001",
	})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "forge:conv-001", result.Result.Session.ID)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Len(t, result.Result.Messages, 4)
}

func TestPiebaldProviderSourceMethodsAndParse(t *testing.T) {
	dbPath := newPiebaldTestDB(t)
	seedPiebaldProviderBasicChat(t, dbPath)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentPiebald, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	assertDBBackedWatchPlan(t, provider, root, PiebaldDBFilename)
	assertDBBackedDiscoverFindFingerprint(
		t, provider, root, dbPath, "42",
	)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~piebald:42",
	})
	require.NoError(t, err)
	require.True(t, ok)
	forkSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "42-7",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, source.DisplayPath, forkSource.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "piebald:42", result.Result.Session.ID)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Len(t, result.Result.Messages, 2)
}

func TestWarpProviderSourceMethodsAndParse(t *testing.T) {
	dbPath, seeder, db := newWarpTestDB(t)
	defer db.Close()
	seedWarpConversation(t, seeder)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentWarp, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	assertDBBackedWatchPlan(t, provider, root, WarpDBFilename)
	assertDBBackedDiscoverFindFingerprint(
		t, provider, root, dbPath, "conv-001",
	)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "warp:conv-001",
	})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "warp:conv-001", result.Result.Session.ID)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.NotEmpty(t, result.Result.Messages)
}

func TestDBBackedProviderFingerprintIgnoresUnrelatedRows(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "conv-001",
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, dbPath+"#conv-001", before.Key)
	assert.NotZero(t, before.MTimeNS)
	assert.Zero(t, before.Size)
	assert.Empty(t, before.Hash)

	seeder.AddConversation(
		"conv-002",
		"Unrelated",
		123,
		`{"conversation_id":"conv-002","messages":[]}`,
		"2026-05-03 09:58:15.000000000",
		"2026-05-03 10:00:16.000000000",
		"",
	)

	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestDBBackedProviderDeletedRowFingerprintsTombstoneAndSkips(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "conv-001",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath+"#conv-001", source.DisplayPath)
	_, err = db.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, "conv-001")
	require.NoError(t, err)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "write",
			WatchRoot:         root,
			StoredSourcePaths: []string{dbPath + "#conv-001"},
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	source = changed[0]

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: dbPath + "#conv-001"}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestDBBackedProviderStoredVirtualPathFreshness(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)
	virtualPath := dbPath + "#conv-001"

	provider, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	_, err = db.Exec(`DELETE FROM conversations WHERE conversation_id = ?`, "conv-001")
	require.NoError(t, err)
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted DB row")

	staleSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps virtual tombstone identity")
	assert.Equal(t, virtualPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: staleSource,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)

	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath))
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     virtualPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted DB file")
}

func TestDBBackedProviderRejectsInvalidStoredVirtualPaths(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	defer db.Close()
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)
	virtualPath := dbPath + "#conv-001"
	otherPath := dbPath + "#conv-002"
	seeder.AddConversation(
		"conv-002",
		"Other",
		123,
		`{"conversation_id":"conv-002","messages":[]}`,
		"2026-05-03 09:58:15.000000000",
		"2026-05-03 10:00:16.000000000",
		"",
	)

	provider, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "forge-copy.db") + "#conv-001",
		filepath.Join(root, "nested", ForgeDBFilename) + "#conv-001",
	} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:       "conv-001",
		StoredFilePath:     otherPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath,
		"raw session identity must remain authoritative when a stored-path hint is stale")

	source, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath)
}

func TestDBBackedProviderMissingDBSkipsAndPreservesSessions(t *testing.T) {
	dbPath, seeder, db := newForgeTestDB(t)
	seedForgeConversation(t, seeder)
	root := filepath.Dir(dbPath)
	virtualPath := dbPath + "#conv-001"

	provider, ok := NewProvider(AgentForge, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, source.DisplayPath)

	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{virtualPath},
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	source = changed[0]

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	// The backing DB file is gone, so the outcome must NOT force-replace: the
	// persistent archive preserves sessions whose source file no longer exists
	// on disk rather than letting the engine delete them.
	assert.False(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func assertDBBackedWatchPlan(
	t *testing.T,
	provider Provider,
	root string,
	dbName string,
) {
	t.Helper()
	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, dbName)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, dbName+"-*")
}

func assertDBBackedDiscoverFindFingerprint(
	t *testing.T,
	provider Provider,
	root, dbPath, rawID string,
) {
	t.Helper()
	virtualPath := dbPath + "#" + rawID
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)
	assert.Equal(t, virtualPath, discovered[0].FingerprintKey)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, virtualPath, changed[0].DisplayPath)

	unrelated, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-backup", EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, unrelated)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.Empty(t, fingerprint.Hash)
}

func seedPiebaldProviderBasicChat(t *testing.T, dbPath string) {
	t.Helper()
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO projects (id, directory, name) VALUES (1, '/repo/app', 'app')`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO chats
		 (id, title, created_at, updated_at, is_deleted, message_count, current_directory, branch_name, project_id)
		 VALUES (42, 'Fix bug', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 2, '/repo/app', 'main', 1)`)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, status)
		 VALUES (100, 42, 'user', '', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
	seedPiebaldTextPart(t, dbPath, 200, 100, 0, "Please fix this", false)
	execPiebaldTestSQL(t, dbPath,
		`INSERT INTO messages
		 (id, parent_chat_id, role, model, created_at, updated_at, status, finish_reason)
		 VALUES (101, 42, 'assistant', 'claude-test', '2026-05-01T10:00:02Z', '2026-05-01T10:00:03Z', 'completed', 'end_turn')`)
	seedPiebaldTextPart(t, dbPath, 201, 101, 0, "I fixed it", false)
}
