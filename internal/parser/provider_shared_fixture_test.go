package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var sharedParserFixtureRoots = struct {
	sync.Mutex
	paths []string
}{}

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupSharedParserFixtureRoots()
	os.Exit(code)
}

func registerSharedParserFixtureRoot(path string) {
	sharedParserFixtureRoots.Lock()
	defer sharedParserFixtureRoots.Unlock()
	sharedParserFixtureRoots.paths = append(sharedParserFixtureRoots.paths, path)
}

func cleanupSharedParserFixtureRoots() {
	sharedParserFixtureRoots.Lock()
	paths := append([]string(nil), sharedParserFixtureRoots.paths...)
	sharedParserFixtureRoots.Unlock()

	for _, path := range paths {
		_ = os.RemoveAll(path)
	}
}

type sharedZedProviderFixture struct {
	Root           string
	DBPath         string
	FirstThreadID  string
	SecondThreadID string
}

var sharedZedProviderReadFixture = struct {
	sync.Once
	fixture sharedZedProviderFixture
	err     error
}{}

func zedProviderReadFixture(t *testing.T) sharedZedProviderFixture {
	t.Helper()

	sharedZedProviderReadFixture.Do(func() {
		root, err := os.MkdirTemp("", "agentsview-zed-provider-fixture-*")
		if err != nil {
			sharedZedProviderReadFixture.err = err
			return
		}
		registerSharedParserFixtureRoot(root)

		dbPath := filepath.Join(root, zedThreadsDBRelPath)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			sharedZedProviderReadFixture.err = err
			return
		}

		firstThreadID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
		secondThreadID := "20431c84-c47b-4e6c-b2df-f9f3b9ad025b"
		createZedThreadsDBAt(t, dbPath, []zedTestThread{
			{
				id:        firstThreadID,
				summary:   "First thread",
				createdAt: "2026-06-08T09:12:41Z",
				updatedAt: "2026-06-08T09:14:10Z",
				dataType:  "json",
				data:      []byte(`{"messages":[{"User":{"content":[{"Text":"First"}]}}]}`),
			},
			{
				id:        secondThreadID,
				summary:   "Second thread",
				createdAt: "2026-06-08T09:15:41Z",
				updatedAt: "2026-06-08T09:16:10Z",
				dataType:  "json",
				data:      []byte(`{"messages":[{"User":{"content":[{"Text":"Second"}]}}]}`),
			},
		})
		sharedZedProviderReadFixture.fixture = sharedZedProviderFixture{
			Root:           root,
			DBPath:         dbPath,
			FirstThreadID:  firstThreadID,
			SecondThreadID: secondThreadID,
		}
	})
	require.NoError(t, sharedZedProviderReadFixture.err,
		"create shared zed provider fixture")
	fixture := sharedZedProviderReadFixture.fixture
	root, dbPath := copySharedParserFixtureFile(t, fixture.DBPath, zedThreadsDBRelPath)
	return sharedZedProviderFixture{
		Root:           root,
		DBPath:         dbPath,
		FirstThreadID:  fixture.FirstThreadID,
		SecondThreadID: fixture.SecondThreadID,
	}
}

type sharedShelleyProviderFixture struct {
	Root   string
	DBPath string
}

var sharedShelleyProviderReadFixture = struct {
	sync.Once
	fixture sharedShelleyProviderFixture
	err     error
}{}

func shelleyProviderReadFixture(t *testing.T) sharedShelleyProviderFixture {
	t.Helper()

	sharedShelleyProviderReadFixture.Do(func() {
		root, err := os.MkdirTemp("", "agentsview-shelley-provider-fixture-*")
		if err != nil {
			sharedShelleyProviderReadFixture.err = err
			return
		}
		registerSharedParserFixtureRoot(root)

		dbPath := filepath.Join(root, shelleyDBName)
		db, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			sharedShelleyProviderReadFixture.err = err
			return
		}
		defer db.Close()
		if _, err := db.Exec(shelleySchema); err != nil {
			sharedShelleyProviderReadFixture.err = err
			return
		}
		seedShelleyMainConversation(t, db)
		seedShelleyConversation(
			t, db, "cAUX1", "Auxiliary", "/home/user/dev/aux",
			"claude-sonnet-4-6", "", true,
			"2026-06-15T11:00:00Z", "2026-06-15T11:03:00Z",
		)
		seedShelleyMessage(t, db, "cAUX1", 1, 1, "user",
			`{"Role":0,"Content":[{"Type":2,"Text":"Aux request"}]}`,
			"", "", "2026-06-15T11:00:00Z")

		sharedShelleyProviderReadFixture.fixture = sharedShelleyProviderFixture{
			Root:   root,
			DBPath: dbPath,
		}
	})
	require.NoError(t, sharedShelleyProviderReadFixture.err,
		"create shared shelley provider fixture")
	fixture := sharedShelleyProviderReadFixture.fixture
	root, dbPath := copySharedParserFixtureFile(t, fixture.DBPath, shelleyDBName)
	return sharedShelleyProviderFixture{
		Root:   root,
		DBPath: dbPath,
	}
}

type sharedOpenCodeSQLiteProviderFixture struct {
	Root              string
	DBPath            string
	TargetSessionID   string
	SessionIDs        []string
	SQLiteVirtualPath string
	AllVirtualPaths   []string
}

var sharedOpenCodeSQLiteProviderReadFixture = struct {
	sync.Once
	fixture sharedOpenCodeSQLiteProviderFixture
	err     error
}{}

func openCodeSQLiteProviderReadFixture(
	t *testing.T,
) sharedOpenCodeSQLiteProviderFixture {
	t.Helper()

	sharedOpenCodeSQLiteProviderReadFixture.Do(func() {
		root, err := os.MkdirTemp("", "agentsview-opencode-sqlite-provider-fixture-*")
		if err != nil {
			sharedOpenCodeSQLiteProviderReadFixture.err = err
			return
		}
		registerSharedParserFixtureRoot(root)

		dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
		defer db.Close()
		seeder.AddProject("prj_1", "/home/user/code/sqlite-app")

		targetSessionID := "ses_sqlite"
		sessionIDs := []string{targetSessionID, "ses_aux_a", "ses_aux_b"}
		for i, sessionID := range sessionIDs {
			start := int64(1700000000000 + i*1000)
			seeder.AddSession(
				sessionID, "prj_1", "", "Session "+sessionID, start, start+60000,
			)
		}
		seeder.AddMessage("msg_1", targetSessionID, 1700000000000, 1700000000000,
			`{"role":"user"}`)
		seeder.AddPart(
			"prt_1", "msg_1", targetSessionID, 1700000000000, 1700000000000,
			`{"type":"text","text":"Hello from sqlite"}`,
		)

		virtualPaths := make([]string, 0, len(sessionIDs))
		for _, sessionID := range sessionIDs {
			virtualPaths = append(virtualPaths, OpenCodeSQLiteVirtualPath(dbPath, sessionID))
		}
		sharedOpenCodeSQLiteProviderReadFixture.fixture = sharedOpenCodeSQLiteProviderFixture{
			Root:              root,
			DBPath:            dbPath,
			TargetSessionID:   targetSessionID,
			SessionIDs:        sessionIDs,
			SQLiteVirtualPath: OpenCodeSQLiteVirtualPath(dbPath, targetSessionID),
			AllVirtualPaths:   virtualPaths,
		}
	})
	require.NoError(t, sharedOpenCodeSQLiteProviderReadFixture.err,
		"create shared opencode sqlite provider fixture")

	fixture := sharedOpenCodeSQLiteProviderReadFixture.fixture
	root, dbPath := copySharedParserFixtureFile(t, fixture.DBPath, "opencode.db")
	virtualPaths := make([]string, 0, len(fixture.SessionIDs))
	for _, sessionID := range fixture.SessionIDs {
		virtualPaths = append(virtualPaths, OpenCodeSQLiteVirtualPath(dbPath, sessionID))
	}
	return sharedOpenCodeSQLiteProviderFixture{
		Root:              root,
		DBPath:            dbPath,
		TargetSessionID:   fixture.TargetSessionID,
		SessionIDs:        append([]string(nil), fixture.SessionIDs...),
		SQLiteVirtualPath: OpenCodeSQLiteVirtualPath(dbPath, fixture.TargetSessionID),
		AllVirtualPaths:   virtualPaths,
	}
}

func copySharedParserFixtureFile(
	t *testing.T,
	srcPath string,
	relPath string,
) (string, string) {
	t.Helper()

	root := t.TempDir()
	dstPath := filepath.Join(root, relPath)
	raw, err := os.ReadFile(srcPath)
	require.NoError(t, err, "read shared fixture file %s", srcPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(dstPath), 0o755),
		"mkdir shared fixture copy")
	require.NoError(t, os.WriteFile(dstPath, raw, 0o644),
		"copy shared fixture file %s", dstPath)
	return root, dstPath
}

func newRemovedOpenCodeDBPath(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	dbPath, _, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath), "remove sqlite db")
	return root, dbPath
}

func requireContainsSourcePath(t *testing.T, sources []SourceRef, path string) {
	t.Helper()

	require.Contains(t, sourceDisplayPaths(sources), path)
}

func requireSourcePathsMatch(t *testing.T, sources []SourceRef, paths []string) {
	t.Helper()

	require.ElementsMatch(t, paths, sourceDisplayPaths(sources))
}
