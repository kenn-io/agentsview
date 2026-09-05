package parser

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiroProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/shadowed", "shadowed-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012040000,
	)
	legacyPath := filepath.Join(root, "legacy-session.jsonl")
	writeSourceFile(t, legacyPath, kiroProviderJSONLFixture("Legacy question"))
	writeSourceFile(t, filepath.Join(root, "legacy-session.json"),
		kiroProviderMetaFixture("legacy-session", "/home/user/code/legacy"))
	shadowedPath := filepath.Join(root, "shadowed-session.jsonl")
	writeSourceFile(t, shadowedPath, kiroProviderJSONLFixture("Shadowed question"))
	writeSourceFile(t, filepath.Join(root, "notes", "nested.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.jsonl")
	assert.Contains(t, plan.Roots[0].IncludeGlobs, kiroSQLiteDBName)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, kiroSQLiteDBName+"-*")

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)
	assert.Equal(t, legacyPath, discovered[1].DisplayPath)

	foundSQLite, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~kiro:sqlite-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, KiroSQLiteVirtualPath(dbPath, "sqlite-session"), foundSQLite.DisplayPath)
	assert.Equal(t, foundSQLite.DisplayPath, foundSQLite.FingerprintKey)

	foundLegacy, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "legacy-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, legacyPath, foundLegacy.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath)
}

func TestKiroProviderParsePhysicalVirtualAndLegacySources(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	legacyPath := filepath.Join(root, "legacy-session.jsonl")
	writeSourceFile(t, legacyPath, kiroProviderJSONLFixture("Legacy question"))

	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	allOutcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.True(t, allOutcome.ResultSetComplete)
	require.True(t, allOutcome.ForceReplace)
	require.Len(t, allOutcome.Results, 1)
	assert.Equal(t, "kiro:sqlite-session", allOutcome.Results[0].Result.Session.ID)

	virtualSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sqlite-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	oneOutcome, err := provider.Parse(context.Background(), ParseRequest{Source: virtualSource})
	require.NoError(t, err)
	require.True(t, oneOutcome.ResultSetComplete)
	require.True(t, oneOutcome.ForceReplace)
	require.Len(t, oneOutcome.Results, 1)
	assert.Equal(t, "devbox", oneOutcome.Results[0].Result.Session.Machine)

	legacySource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: legacyPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	legacyOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      legacySource,
		Fingerprint: SourceFingerprint{Hash: "legacy-hash"},
	})
	require.NoError(t, err)
	require.True(t, legacyOutcome.ResultSetComplete)
	require.False(t, legacyOutcome.ForceReplace)
	require.Len(t, legacyOutcome.Results, 1)
	assert.Equal(t, "kiro:legacy-session", legacyOutcome.Results[0].Result.Session.ID)
	assert.Equal(t, "legacy-hash", legacyOutcome.Results[0].Result.Session.File.Hash)

	// Close the setup handle before deleting; Windows will not unlink a file
	// this process still holds open.
	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath))
	missingOutcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	assert.True(t, missingOutcome.ResultSetComplete)
	// The backing DB file was deleted; preserve the stored sessions by not
	// force-replacing, which would delete them from the archive.
	assert.False(t, missingOutcome.ForceReplace)
	assert.Equal(t, SkipNoSession, missingOutcome.SkipReason)
}

func TestKiroProviderSkipsShadowedLegacySource(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/shadowed", "shadowed-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	shadowedPath := filepath.Join(root, "shadowed-session.jsonl")
	writeSourceFile(t, shadowedPath, kiroProviderJSONLFixture("Shadowed question"))

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:   "shadowed-session",
		StoredFilePath: shadowedPath,
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.Len(t, outcome.Results, 1)
	assert.Equal(t, "kiro:shadowed-session", outcome.Results[0].Result.Session.ID)

	source, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID:  "host~kiro:shadowed-session",
		StoredFilePath: shadowedPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, KiroSQLiteVirtualPath(dbPath, "shadowed-session"), source.DisplayPath)
}

func TestKiroProviderShadowsLegacyAcrossAllRoots(t *testing.T) {
	sqliteRoot := t.TempDir()
	legacyRoot := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, sqliteRoot)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/current", "shared-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	legacyPath := filepath.Join(legacyRoot, "legacy-storage.jsonl")
	writeSourceFile(t, legacyPath, kiroProviderJSONLFixture("Legacy question"))
	writeSourceFile(t, filepath.Join(legacyRoot, "legacy-storage.json"),
		kiroProviderMetaFixture("shared-session", "/home/user/code/legacy"))

	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots: []string{sqliteRoot, legacyRoot},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)

	legacySource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:   "shared-session",
		StoredFilePath: legacyPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: legacySource,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.Len(t, outcome.Results, 1)
	assert.Equal(t, "kiro:shared-session", outcome.Results[0].Result.Session.ID)
}

func TestKiroProviderZeroWinnerSQLitePreservesArchive(t *testing.T) {
	currentRoot := t.TempDir()
	sqliteRoot := t.TempDir()
	rawID := "sess_0123456789abcdef"
	winnerDBPath, winnerDB := newKiroProviderSQLiteDBAt(t, currentRoot)
	seedKiroSQLiteSession(
		t, winnerDB, "/home/user/code/current", rawID,
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	dbPath, db := newKiroProviderSQLiteDBAt(t, sqliteRoot)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/current", rawID,
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots: []string{currentRoot, sqliteRoot},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	var database SourceRef
	var foundWinner bool
	for _, source := range discovered {
		if source.DisplayPath == dbPath {
			database = source
		}
		if source.DisplayPath == winnerDBPath {
			foundWinner = true
		}
	}
	require.True(t, foundWinner)
	require.Equal(t, dbPath, database.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: database})
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	assert.False(t, outcome.ForceReplace)
	assert.Equal(t, []string{"kiro:" + rawID}, provider.(interface {
		PreservedSessionIDs(SourceRef) []string
	}).PreservedSessionIDs(database))
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
}

func TestKiroProviderFingerprintsSQLiteAndLegacySources(t *testing.T) {
	root := t.TempDir()
	payload := readKiroFixture(t, "standard_payload.json")
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		payload,
		1779012000000, 1779012030000,
	)
	legacyPath := filepath.Join(root, "legacy-session.jsonl")
	writeSourceFile(t, legacyPath, kiroProviderJSONLFixture("Legacy question"))

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	virtualSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sqlite-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	virtualFingerprint, err := provider.Fingerprint(context.Background(), virtualSource)
	require.NoError(t, err)
	assert.Equal(t, KiroSQLiteVirtualPath(dbPath, "sqlite-session"), virtualFingerprint.Key)
	assert.Equal(t, int64(len(payload)), virtualFingerprint.Size)
	assert.Equal(t, int64(1779012030000)*1_000_000, virtualFingerprint.MTimeNS)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	sqliteSource := sources[0]
	require.Equal(t, dbPath, sqliteSource.DisplayPath)
	beforePhysical, err := provider.Fingerprint(context.Background(), sqliteSource)
	require.NoError(t, err)
	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, "wal")
	walTime := time.Unix(0, beforePhysical.MTimeNS+int64(time.Second))
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))
	afterPhysical, err := provider.Fingerprint(context.Background(), sqliteSource)
	require.NoError(t, err)
	assert.Greater(t, afterPhysical.MTimeNS, beforePhysical.MTimeNS)

	legacySource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: legacyPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	legacyFingerprint, err := provider.Fingerprint(context.Background(), legacySource)
	require.NoError(t, err)
	assert.Equal(t, legacyPath, legacyFingerprint.Key)
	assert.NotEmpty(t, legacyFingerprint.Hash)
}

func TestKiroProviderMissingSQLiteSourcesCanReachParse(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	physicalSource := sources[0]
	virtualSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "sqlite-session",
	})
	require.NoError(t, err)
	require.True(t, ok)

	_, err = db.Exec(`DELETE FROM conversations_v2 WHERE conversation_id = ?`, "sqlite-session")
	require.NoError(t, err)
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     virtualSource.DisplayPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted SQLite row")
	staleVirtualSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtualSource.DisplayPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps virtual tombstone identity")
	assert.Equal(t, virtualSource.DisplayPath, staleVirtualSource.DisplayPath)
	virtualFingerprint, err := provider.Fingerprint(context.Background(), virtualSource)
	require.NoError(t, err)
	assert.Equal(t, virtualSource.FingerprintKey, virtualFingerprint.Key)
	virtualOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      virtualSource,
		Fingerprint: virtualFingerprint,
	})
	require.NoError(t, err)
	assert.True(t, virtualOutcome.ResultSetComplete)
	assert.True(t, virtualOutcome.ForceReplace)
	assert.Equal(t, SkipNoSession, virtualOutcome.SkipReason)

	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath))
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     physicalSource.DisplayPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted SQLite DB")
	physicalFingerprint, err := provider.Fingerprint(context.Background(), physicalSource)
	require.NoError(t, err)
	assert.Equal(t, physicalSource.FingerprintKey, physicalFingerprint.Key)
	physicalOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      physicalSource,
		Fingerprint: physicalFingerprint,
	})
	require.NoError(t, err)
	assert.True(t, physicalOutcome.ResultSetComplete)
	// The whole DB file was removed: preserve the stored sessions by not
	// force-replacing. (The virtual case above keeps ForceReplace because the
	// DB file is still present and only the row was deleted.)
	assert.False(t, physicalOutcome.ForceReplace)
	assert.Equal(t, SkipNoSession, physicalOutcome.SkipReason)
}

// TestKiroProviderChangedPathTombstonesDeletedRow verifies the changed-path
// classifier emits a per-session tombstone for a stored Kiro SQLite member
// whose row was deleted from a still-present database, so the engine can
// force-replace it out of the archive. The surviving member is left to the
// whole-DB fan-out, and a vanished database emits no tombstone (the stored
// sessions are preserved per the persistent-archive rule).
func TestKiroProviderChangedPathTombstonesDeletedRow(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "surviving",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "deleted",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012040000,
	)

	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	survivingPath := KiroSQLiteVirtualPath(dbPath, "surviving")
	deletedPath := KiroSQLiteVirtualPath(dbPath, "deleted")

	// Delete one row while the database file stays present.
	_, err := db.Exec(`DELETE FROM conversations_v2 WHERE conversation_id = ?`, "deleted")
	require.NoError(t, err)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "write",
			WatchRoot:         root,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(t, err)
	gotPaths := make([]string, len(changed))
	for i, src := range changed {
		gotPaths[i] = src.DisplayPath
	}
	assert.ElementsMatch(t, []string{dbPath, deletedPath}, gotPaths,
		"whole-DB source plus a tombstone for the deleted row only")

	var tombstone SourceRef
	for _, src := range changed {
		if src.DisplayPath == deletedPath {
			tombstone = src
		}
	}
	require.NotEmpty(t, tombstone.DisplayPath, "deleted-row tombstone source")
	fingerprint, err := provider.Fingerprint(context.Background(), tombstone)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      tombstone,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace,
		"a row deleted from a present DB is force-replaced out of the archive")
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)

	// When the whole database file is gone, no tombstone is emitted so the
	// stored sessions are preserved.
	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath))
	gone, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              dbPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{survivingPath, deletedPath},
		},
	)
	require.NoError(t, err)
	for _, src := range gone {
		assert.NotEqual(t, deletedPath, src.DisplayPath,
			"a vanished database must not tombstone stored sessions")
		assert.NotEqual(t, survivingPath, src.DisplayPath)
	}
}

func TestKiroProviderRejectsInvalidStoredSQLitePaths(t *testing.T) {
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, path := range []string{
		dbPath + "#",
		filepath.Join(root, "data-copy.sqlite3") + "#sqlite-session",
		filepath.Join(root, "nested", kiroSQLiteDBName) + "#sqlite-session",
	} {
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}
}

// Synthetic reproduction: the reporter supplied no loadable transcript.
func TestKiroProviderCurrentLayoutParsesMessages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspace", "sess_0123456789abcdef", "messages.jsonl")
	writeSourceFile(t, path, strings.Join([]string{
		`{"payload":{"type":"user","content":"hello"}}`,
		`{"payload":{"type":"assistant","content":"hi"}}`,
		`{"payload":{"type":"tool_call","toolName":"read","toolCallId":"call-1","args":{"path":"a.go"}}}`,
		`{"payload":{"type":"tool_result","toolCallId":"call-1","content":"ok"}}`,
		`{"payload":{"type":"informational","content":"skip"}}`,
	}, "\n")+"\n")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "kiro:sess_0123456789abcdef", outcome.Results[0].Result.Session.ID)
	assert.Len(t, outcome.Results[0].Result.Messages, 4)
}

func TestKiroProviderCurrentLayoutRejectsLookalikesAndEscapes(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "project-a", "sess_0123456789abcdef", "messages.jsonl")
	writeSourceFile(t, valid, `{"payload":{"type":"user","content":"ok"}}`+"\n")
	nestedSession := filepath.Join(root, "sess_fedcba9876543210", "sess_aaaaaaaaaaaaaaaa", "messages.jsonl")
	writeSourceFile(t, nestedSession, `{"payload":{"type":"user","content":"bad"}}`+"\n")
	for _, path := range []string{
		filepath.Join(root, ".history", "sess_0123456789abcdef", "messages.jsonl"),
		filepath.Join(root, "snapshots", "sess_0123456789abcdef", "messages.jsonl"),
		filepath.Join(root, "workspace", ".history", "sess_0123456789abcdef", "messages.jsonl"),
		filepath.Join(root, "workspace", "sess_0123456789abcdef", "snapshots", "messages.jsonl"),
		filepath.Join(root, "workspace", "nested", "sess_0123456789abcdef", "messages.jsonl"),
		filepath.Join(root, "workspace", "session_0123456789abcdef", "messages.jsonl"),
		filepath.Join(root, "project-a", "sess_bad.id", "messages.jsonl"),
	} {
		writeSourceFile(t, path, `{"payload":{"type":"user","content":"bad"}}`+"\n")
	}
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, valid, sources[0].DisplayPath)
	for _, tc := range []struct{ id, path string }{
		{"sess_0123456789abcdef", valid},
	} {
		found, foundOK, findErr := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: tc.id})
		require.NoError(t, findErr)
		require.True(t, foundOK)
		assert.Equal(t, tc.path, found.DisplayPath)
	}
	sidecar := filepath.Join(filepath.Dir(valid), "session.json")
	writeSourceFile(t, sidecar, `{"title":"Synthetic"}`)
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: sidecar, WatchRoot: root, EventKind: "write"})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, valid, changed[0].DisplayPath)
	changed, err = provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: valid, WatchRoot: root, EventKind: "write"})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, valid, changed[0].DisplayPath)
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "sess_../escape"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestKiroProviderCurrentLayoutLifecycleAndExactLookup(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sess_0123456789abcdef", "messages.jsonl")
	writeSourceFile(t, path, `{"payload":{"type":"user","content":"hello"}}`+"\n")
	sidecar := filepath.Join(filepath.Dir(path), "session.json")
	writeSourceFile(t, sidecar, `{"title":"Synthetic","workspacePaths":["/home/user/project"]}`)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: sidecar, WatchRoot: root, EventKind: "write"})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: "sess_0123456789abcdef"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.DisplayPath)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Greater(t, fingerprint.Size, int64(len(`{"payload":{"type":"user","content":"hello"}}`)+1))
}

func TestKiroProviderCurrentFingerprintIncludesSidecarContent(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	path := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	sidecar := filepath.Join(filepath.Dir(path), "session.json")
	writeSourceFile(t, path, `{"payload":{"type":"user","content":"hello"}}`+"\n")
	writeSourceFile(t, sidecar, `{"title":"A"}`)

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, found)
	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"B"}`), 0o644))
	transcriptInfo, err := os.Stat(path)
	require.NoError(t, err)
	earlier := transcriptInfo.ModTime().Add(-time.Minute)
	require.NoError(t, os.Chtimes(sidecar, earlier, earlier))
	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestKiroProviderCurrentMetadataDecodeFailureIsRetryable(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	path := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	sidecar := filepath.Join(filepath.Dir(path), "session.json")
	writeSourceFile(t, path, `{"payload":{"type":"user","content":"hello"}}`+"\n")
	writeSourceFile(t, sidecar, "{")

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, found)
	_, err = provider.Parse(context.Background(), ParseRequest{Source: source})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode Kiro current metadata")
}

func TestKiroProviderCurrentBoundsUseAcceptedMessageTimestamps(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	path := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, path, strings.Join([]string{
		`{"timestamp":"2099-01-01T00:00:00Z","payload":{"type":"session_metadata"}}`,
		`{"timestamp":"2026-08-24T12:30:00Z","payload":{"type":"assistant","content":"latest"}}`,
		`{"timestamp":"2026-08-24T12:00:00Z","payload":{"type":"user","content":"earliest"}}`,
	}, "\n")+"\n")

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), session.StartedAt)
	assert.Equal(t, time.Date(2026, 8, 24, 12, 30, 0, 0, time.UTC), session.EndedAt)
}

func TestKiroProviderStablePathTieBreak(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	direct := filepath.Join(root, rawID, "messages.jsonl")
	workspace := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	fixture := `{"timestamp":"2026-08-24T12:00:00Z","payload":{"type":"user","content":"same"}}` + "\n"
	writeSourceFile(t, direct, fixture)
	writeSourceFile(t, workspace, fixture)
	tie := time.Unix(1700000000, 0)
	require.NoError(t, os.Chtimes(direct, tie, tie))
	require.NoError(t, os.Chtimes(workspace, tie, tie))

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, foundOK, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, foundOK)
	assert.Equal(t, direct, found.DisplayPath)
}

func TestKiroProviderDiscoveryFailsOnSQLiteMetadataError(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, kiroSQLiteDBName)
	writeSourceFile(t, dbPath, "not a sqlite database")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(context.Background())
	assert.Error(t, err)
}

func TestKiroProviderLogicalIdentityAndRankUnifyLegacyAndCurrent(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	legacy := filepath.Join(root, rawID+".jsonl")
	current := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, legacy, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, current, `{"timestamp":"2026-08-24T12:34:56Z","payload":{"type":"user","content":"current"}}`+"\n")

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, current, discovered[0].DisplayPath)
	assert.Equal(t, rawID, discovered[0].Key)

	var streamed []SourceRef
	err = provider.(interface {
		DiscoverEach(context.Context, func(SourceRef) error) error
	}).DiscoverEach(context.Background(), func(source SourceRef) error {
		streamed = append(streamed, source)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, streamed, 2)
	assert.Equal(t, rawID, streamed[0].Key)
	ranker := provider.(ReconciliationSourceRanker)
	assert.Equal(t, int64(1), ranker.ReconciliationSourceRank(streamed[0]).Class)
	assert.Equal(t, int64(2), ranker.ReconciliationSourceRank(streamed[1]).Class)
	legacySource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: legacy})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawID, legacySource.Key)
	assert.Equal(t, int64(1), ranker.ReconciliationSourceRank(legacySource).Class)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, current, found.DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: found})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, time.Date(2026, 8, 24, 12, 34, 56, 0, time.UTC),
		outcome.Results[0].Result.Messages[0].Timestamp)
}

func TestKiroProviderRejectsCurrentSymlinkEscapeOnLookupAndChange(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	rawID := "sess_0123456789abcdef"
	outsidePath := filepath.Join(outside, "messages.jsonl")
	writeSourceFile(t, outsidePath, `{"payload":{"type":"user","content":"outside"}}`+"\n")
	path := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, path); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: path})
	require.NoError(t, err)
	assert.False(t, ok)
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{Path: path, WatchRoot: root, EventKind: "write"})
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestKiroProviderFindSourceRanksAllRepresentationsAndRoots(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	legacy := filepath.Join(root, rawID+".jsonl")
	current := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, legacy, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, current, `{"payload":{"type":"user","content":"current"}}`+"\n")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID, StoredFilePath: legacy,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, current, found.DisplayPath,
		"a non-pinned stored hint must use the same representation ranking as discovery")
	pinned, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID, StoredFilePath: legacy, PreferStoredSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, legacy, pinned.DisplayPath)
}

func TestKiroProviderChangedCurrentEventIncludesSQLiteDuplicate(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	current := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, current, `{"payload":{"type":"user","content":"current"}}`+"\n")
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(t, db, "/home/user/code/kiro-app", rawID,
		readKiroFixture(t, "standard_payload.json"), 1779012000000, 1779012030000)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: current, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err)
	got := make([]string, len(changed))
	for i, source := range changed {
		got[i] = source.DisplayPath
	}
	assert.Equal(t, []string{KiroSQLiteVirtualPath(dbPath, rawID)}, got)
	assert.Equal(t, int64(3), provider.(ReconciliationSourceRanker).
		ReconciliationSourceRank(changed[0]).Class)
}

func TestKiroProviderChangedCurrentEventScansOnlyAffectedSession(t *testing.T) {
	root := t.TempDir()
	targetID := "sess_0123456789abcdef"
	target := filepath.Join(root, "workspace", targetID, "messages.jsonl")
	writeSourceFile(t, target, `{"payload":{"type":"user","content":"target"}}`+"\n")
	for i := range 128 {
		id := fmt.Sprintf("sess_%016x", i)
		path := filepath.Join(root, "workspace", id, "messages.jsonl")
		writeSourceFile(t, path, `{"payload":{"type":"user","content":"decoy"}}`+"\n")
	}
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	kiro := provider.(*kiroProvider)
	var readDirs []string
	kiro.sources.readDir = func(path string) ([]os.DirEntry, error) {
		readDirs = append(readDirs, path)
		return os.ReadDir(path)
	}

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: target, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, target, changed[0].DisplayPath)
	assert.Equal(t, []string{root}, readDirs,
		"a non-database event should inspect only the root directory")
}

func TestKiroProviderChangedCurrentEventIgnoresUnrelatedLegacyDamage(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	current := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, current, `{"payload":{"type":"user","content":"current"}}`+"\n")
	writeSourceFile(t, filepath.Join(root, "broken.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "broken.json"), "{")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: current, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err,
		"an unrelated malformed legacy sidecar must not fail a current-session event")
	require.Len(t, changed, 1)
	assert.Equal(t, current, changed[0].DisplayPath)
}

func TestKiroProviderChangedLegacyEventPreservesMetadataIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage-name.jsonl")
	writeSourceFile(t, path, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, filepath.Join(root, "storage-name.json"),
		kiroProviderMetaFixture("mapped-session", "/home/user/code/legacy"))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: path, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
}

func TestKiroProviderLegacySidecarEventAndFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage-name.jsonl")
	sidecar := filepath.Join(root, "storage-name.json")
	writeSourceFile(t, path, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, sidecar, kiroProviderMetaFixture("mapped-session", "/home/user/code/legacy"))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: sidecar, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
	before, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sidecar, []byte(
		strings.Replace(
			kiroProviderMetaFixture("mapped-session", "/home/user/code/legacy"),
			`"title":"mapped-session"`, `"title":"mapped-sessioX"`, 1,
		),
	), 0o644))
	transcriptInfo, err := os.Stat(path)
	require.NoError(t, err)
	earlier := transcriptInfo.ModTime().Add(-time.Minute)
	require.NoError(t, os.Chtimes(sidecar, earlier, earlier))
	after, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestKiroProviderLegacyMetadataDecodeFailureIsRetryable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage-name.jsonl")
	writeSourceFile(t, path, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, filepath.Join(root, "storage-name.json"), "{")
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode Kiro legacy metadata")
}

func TestKiroProviderChangedCurrentEventRanksMetadataMappedLegacy(t *testing.T) {
	currentRoot := t.TempDir()
	legacyRoot := t.TempDir()
	rawID := "sess_0123456789abcdef"
	current := filepath.Join(currentRoot, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, current, `{"payload":{"type":"user","content":"current"}}`+"\n")
	legacy := filepath.Join(legacyRoot, "storage-name.jsonl")
	writeSourceFile(t, legacy, kiroProviderJSONLFixture("legacy"))
	writeSourceFile(t, filepath.Join(legacyRoot, "storage-name.json"),
		kiroProviderMetaFixture(rawID, "/home/user/code/legacy"))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots: []string{currentRoot, legacyRoot},
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: current, WatchRoot: currentRoot, EventKind: "write",
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, current, changed[0].DisplayPath)
}

func TestKiroProviderDiscoveryFailsOnCurrentRootReadError(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	provider.(*kiroProvider).sources.readDir = func(string) ([]os.DirEntry, error) {
		return nil, fmt.Errorf("current root is unreadable")
	}
	_, err := provider.Discover(context.Background())
	assert.Error(t, err)
}

func TestKiroProviderDiscoveryFailsOnCurrentWorkspaceReadError(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o755))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	provider.(*kiroProvider).sources.readDir = func(path string) ([]os.DirEntry, error) {
		if samePath(path, root) {
			return os.ReadDir(path)
		}
		return nil, fmt.Errorf("current workspace is unreadable")
	}
	_, err := provider.Discover(context.Background())
	assert.Error(t, err)
}

func TestKiroProviderCurrentSidecarRequiresRegularContainedFile(t *testing.T) {
	root := t.TempDir()
	rawID := "sess_0123456789abcdef"
	current := filepath.Join(root, "workspace", rawID, "messages.jsonl")
	writeSourceFile(t, current, `{"payload":{"type":"user","content":"hello"}}`+"\n")
	sidecar := filepath.Join(filepath.Dir(current), "session.json")
	require.NoError(t, os.Mkdir(sidecar, 0o755))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: sidecar, WatchRoot: root, EventKind: "write",
	})
	require.NoError(t, err)
	assert.Empty(t, changed, "a directory named session.json is not metadata")
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Empty(t, outcome.Results[0].Result.Session.SessionName)
}

func TestKiroProviderRejectsSQLiteSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideDB := filepath.Join(outside, kiroSQLiteDBName)
	_, db := newKiroProviderSQLiteDBAt(t, outside)
	require.NoError(t, db.Close())
	dbPath := filepath.Join(root, kiroSQLiteDBName)
	if err := os.Symlink(outsideDB, dbPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, sources)

	virtual := KiroSQLiteVirtualPath(dbPath, "sqlite-session")
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: virtual,
		RawSessionID:   "sqlite-session",
	})
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = OpenKiroSQLiteStore(dbPath)
	assert.Error(t, err, "a symlinked database must not be opened")
}

func TestKiroIDEProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	oldWSHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldFileHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldPath := filepath.Join(root, oldWSHash, oldFileHash+".chat")
	writeSourceFile(t, oldPath, kiroIDEProviderOldFixture("Old IDE question"))
	newPath := filepath.Join(root, "workspace-sessions", "encoded-workspace", "new-session.json")
	writeSourceFile(t, newPath, kiroIDEProviderNewFixture("New IDE question"))
	writeSourceFile(t, filepath.Join(root, "workspace-sessions", "encoded-workspace", "sessions.json"), "[]\n")
	writeSourceFile(t, filepath.Join(root, "default", "ignored.chat"), kiroIDEProviderOldFixture("Ignored"))

	provider, ok := NewProvider(AgentKiroIDE, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.chat")
	assert.Contains(t, plan.Roots[0].IncludeGlobs, "*.json")

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, oldPath, discovered[0].DisplayPath)
	assert.Equal(t, newPath, discovered[1].DisplayPath)

	foundOld, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: oldWSHash + ":" + oldFileHash,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, oldPath, foundOld.DisplayPath)

	foundNew, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~kiro-ide:new-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newPath, foundNew.DisplayPath)
}

func TestKiroIDEProviderParsesOldAndNewSources(t *testing.T) {
	root := t.TempDir()
	oldWSHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldFileHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldPath := filepath.Join(root, oldWSHash, oldFileHash+".chat")
	writeSourceFile(t, oldPath, kiroIDEProviderOldFixture("Old IDE question"))
	newPath := filepath.Join(root, "workspace-sessions", "encoded-workspace", "new-session.json")
	writeSourceFile(t, newPath, kiroIDEProviderNewFixture("New IDE question"))

	provider, ok := NewProvider(AgentKiroIDE, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	oldOutcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.True(t, oldOutcome.ResultSetComplete)
	require.Len(t, oldOutcome.Results, 1)
	assert.Equal(t, "kiro-ide:"+oldWSHash+":"+oldFileHash, oldOutcome.Results[0].Result.Session.ID)
	assert.Equal(t, "devbox", oldOutcome.Results[0].Result.Session.Machine)

	newOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[1],
		Fingerprint: SourceFingerprint{Hash: "new-hash"},
	})
	require.NoError(t, err)
	require.True(t, newOutcome.ResultSetComplete)
	require.Len(t, newOutcome.Results, 1)
	assert.Equal(t, "kiro-ide:new-session", newOutcome.Results[0].Result.Session.ID)
	assert.Equal(t, "new-hash", newOutcome.Results[0].Result.Session.File.Hash)
}

func TestKiroIDEProviderFingerprintsSessionContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspace-sessions", "encoded-workspace", "new-session.json")
	writeSourceFile(t, path, kiroIDEProviderNewFixture("New IDE question"))

	provider, ok := NewProvider(AgentKiroIDE, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "new-session",
	})
	require.NoError(t, err)
	require.True(t, ok)
	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	writeSourceFile(t, path, kiroIDEProviderNewFixture("Changed IDE question"))
	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func newKiroProviderSQLiteDBAt(t *testing.T, root string) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(root, kiroSQLiteDBName)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open kiro provider sqlite db")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(kiroSQLiteSchema)
	require.NoError(t, err, "create kiro sqlite schema")
	return dbPath, db
}

func kiroProviderJSONLFixture(question string) string {
	return `{"kind":"Prompt","data":{"content":[{"kind":"text","data":"` + question + `"}]}}` + "\n" +
		`{"kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"Kiro answer"}]}}` + "\n"
}

func kiroProviderMetaFixture(sessionID, cwd string) string {
	return `{"session_id":"` + sessionID + `","cwd":"` + cwd + `","title":"` + sessionID + `","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:01:00Z"}` + "\n"
}

func kiroIDEProviderOldFixture(question string) string {
	return `{"executionId":"exec-old","actionId":"act-old","chat":[{"role":"human","content":"` + question + `"},{"role":"bot","content":"Old IDE answer"}],"metadata":{"modelId":"claude-sonnet-4-6","startTime":1779012000000,"endTime":1779012030000}}` + "\n"
}

func kiroIDEProviderNewFixture(question string) string {
	return `{"sessionId":"new-session","title":"New title","workspaceDirectory":"/home/user/dev/new-app","history":[{"message":{"role":"user","content":"` + question + `","id":"m1"}},{"message":{"role":"assistant","content":"New IDE answer","id":"m2"}}]}` + "\n"
}

func TestKiroProviderIgnoresBareShmSibling(t *testing.T) {
	// The provider's own read connection rewrites the -shm index, so neither
	// a bare -shm event nor the -shm mtime may move the physical database
	// source, or every scan would schedule the next one.
	root := t.TempDir()
	dbPath, db := newKiroProviderSQLiteDBAt(t, root)
	seedKiroSQLiteSession(
		t, db, "/home/user/code/kiro-app", "sqlite-session",
		readKiroFixture(t, "standard_payload.json"),
		1779012000000, 1779012030000,
	)

	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-shm", EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	require.Equal(t, dbPath, sources[0].DisplayPath)
	before, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	shmPath := dbPath + "-shm"
	writeSourceFile(t, shmPath, "shm")
	shmTime := time.Unix(0, before.MTimeNS+int64(time.Hour))
	require.NoError(t, os.Chtimes(shmPath, shmTime, shmTime))
	after, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, before.MTimeNS, after.MTimeNS)
}
