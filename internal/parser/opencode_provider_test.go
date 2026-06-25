package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeFamilyProviderFactoriesReplaceLegacyAdapter(t *testing.T) {
	for _, agent := range []AgentType{
		AgentOpenCode,
		AgentKilo,
		AgentMiMoCode,
	} {
		t.Run(string(agent), func(t *testing.T) {
			factory, ok := ProviderFactoryByType(agent)
			require.True(t, ok)
			require.NotNil(t, factory)

			provider, ok := NewProvider(agent, ProviderConfig{
				Roots:   []string{t.TempDir()},
				Machine: "devbox",
			})
			require.True(t, ok)
			require.NotNil(t, provider)
		})
	}
}

func TestOpenCodeProviderStorageSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionPath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_provider", "opencode-app", "Provider Session",
	)

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, filepath.Join(root, "storage"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	source := discovered[0]
	assert.Equal(t, AgentOpenCode, source.Provider)
	assert.Equal(t, sessionPath, source.DisplayPath)
	assert.Equal(t, sessionPath, source.FingerprintKey)
	assert.Equal(t, "opencode_app", source.ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~opencode:ses_provider",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sessionPath, found.DisplayPath)

	messagePath := filepath.Join(
		root, "storage", "message", "ses_provider", "msg_1.json",
	)
	partPath := filepath.Join(root, "storage", "part", "msg_1", "prt_1.json")
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "session", path: sessionPath},
		{name: "message", path: messagePath},
		{name: "part", path: partPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      tc.path,
					EventKind: "write",
					WatchRoot: filepath.Join(root, "storage"),
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, sessionPath, changed[0].DisplayPath)
		})
	}

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sessionPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.True(t, HasOpenCodeStorageFingerprint(fingerprint.Hash))

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "opencode:ses_provider", result.Result.Session.ID)
	assert.Equal(t, AgentOpenCode, result.Result.Session.Agent)
	assert.Equal(t, "opencode_app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 1)

	require.NoError(t, os.Remove(sessionPath), "remove storage session")
	removed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      sessionPath,
			EventKind: "remove",
			WatchRoot: filepath.Join(root, "storage"),
		},
	)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, sessionPath, removed[0].DisplayPath)
	assert.Equal(t, "global", removed[0].ProjectHint)
}

func TestOpenCodeProviderSQLiteSourceMethods(t *testing.T) {
	root := t.TempDir()
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	defer db.Close()
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession("ses_sqlite", "prj_1", "", "SQLite Session", 1700000000000, 1700000060000)
	seeder.AddMessage("msg_1", "ses_sqlite", 1700000000000, 1700000000000, `{"role":"user"}`)
	seeder.AddPart("prt_1", "msg_1", "ses_sqlite", 1700000000000, 1700000000000, `{"type":"text","text":"Hello from sqlite"}`)
	virtualPath := OpenCodeSQLiteVirtualPath(dbPath, "ses_sqlite")

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)
	assert.Equal(t, virtualPath, discovered[0].FingerprintKey)

	for _, path := range []string{dbPath, dbPath + "-wal"} {
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: path, EventKind: "write", WatchRoot: root},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, virtualPath, changed[0].DisplayPath)
	}

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~opencode:ses_sqlite",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.Equal(t, int64(1700000060000)*1_000_000, fingerprint.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "opencode:ses_sqlite", result.Result.Session.ID)
	assert.Equal(t, "sqlite_app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, "Hello from sqlite", result.Result.Messages[0].Content)

	require.NoError(t, db.Close())
	require.NoError(t, os.Remove(dbPath), "remove sqlite db")
	removed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, removed, "removed sqlite DBs have no stateless virtual source list")
}

func TestOpenCodeProviderHybridDiscoveryFiltersSQLiteDuplicate(t *testing.T) {
	root := t.TempDir()
	storagePath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_dup", "storage-app", "Storage Session",
	)
	dbPath, seeder, db := newTestDBAt(t, filepath.Join(root, "opencode.db"))
	defer db.Close()
	seeder.AddProject("prj_1", "/home/user/code/sqlite-app")
	seeder.AddSession("ses_dup", "prj_1", "", "Duplicate", 1700000000000, 1700000010000)
	seeder.AddSession("ses_db_only", "prj_1", "", "DB Only", 1700000000000, 1700000020000)
	virtualOnly := OpenCodeSQLiteVirtualPath(dbPath, "ses_db_only")

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{storagePath, virtualOnly}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: OpenCodeSQLiteVirtualPath(dbPath, "ses_dup"),
		FullSessionID:  "opencode:ses_dup",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, storagePath, found.DisplayPath)
}

func TestOpenCodeFamilyProviderRelabelsForks(t *testing.T) {
	for _, tc := range []struct {
		agent         AgentType
		sessionSubdir string
		prefix        string
		project       string
	}{
		{agent: AgentKilo, sessionSubdir: "session", prefix: "kilo:", project: "kilo-app"},
		{agent: AgentMiMoCode, sessionSubdir: "session_diff", prefix: "mimocode:", project: "mimo-app"},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			sessionPath := writeOpenCodeProviderStorageSession(
				t, root, tc.sessionSubdir, "ses_provider", tc.project, "Provider Session",
			)
			provider, ok := NewProvider(tc.agent, ProviderConfig{
				Roots:   []string{root},
				Machine: "devbox",
			})
			require.True(t, ok)
			source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				FullSessionID: "host~" + tc.prefix + "ses_provider",
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, sessionPath, source.DisplayPath)

			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: source,
			})
			require.NoError(t, err)
			require.True(t, outcome.ResultSetComplete)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(t, tc.prefix+"ses_provider", result.Session.ID)
			assert.Equal(t, tc.agent, result.Session.Agent)
			assert.Equal(t, strings.ReplaceAll(tc.project, "-", "_"), result.Session.Project)

			require.NoError(t, os.Remove(sessionPath), "remove storage session")
			removed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "rename",
					WatchRoot: filepath.Join(root, "storage"),
				},
			)
			require.NoError(t, err)
			require.Len(t, removed, 1)
			assert.Equal(t, sessionPath, removed[0].DisplayPath)
		})
	}
}

func writeOpenCodeProviderStorageSession(
	t *testing.T,
	root, sessionSubdir, sessionID, project, title string,
) string {
	t.Helper()
	sessionPath := filepath.Join(
		root, "storage", sessionSubdir, "global", sessionID+".json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        sessionID,
		"directory": filepath.Join("/home/user/code", project),
		"title":     title,
		"time": map[string]any{
			"created": int64(1700000000000),
			"updated": int64(1700000060000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", sessionID, "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": sessionID,
		"role":      "user",
		"time": map[string]any{
			"created": int64(1700000000000),
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": sessionID,
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from storage",
		"time": map[string]any{
			"created": int64(1700000000000),
		},
	})
	return sessionPath
}

func newTestDBAt(
	t *testing.T,
	dbPath string,
) (string, *OpenCodeSeeder, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open test db")
	_, err = db.Exec(openCodeSchema)
	require.NoError(t, err, "create schema")
	return dbPath, &OpenCodeSeeder{db: db, t: t}, db
}
