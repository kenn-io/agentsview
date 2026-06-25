package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiroProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentKiro)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentKiro, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

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
	assert.False(t, plan.Roots[0].Recursive)
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

	require.NoError(t, os.Remove(dbPath))
	missingOutcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	assert.True(t, missingOutcome.ResultSetComplete)
	assert.True(t, missingOutcome.ForceReplace)
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
		StoredFilePath: shadowedPath,
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)

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
		StoredFilePath: legacyPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: legacySource,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
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
	assert.True(t, physicalOutcome.ForceReplace)
	assert.Equal(t, SkipNoSession, physicalOutcome.SkipReason)
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

func TestKiroIDEProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentKiroIDE)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentKiroIDE, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
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
