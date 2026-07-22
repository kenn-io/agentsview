package parser

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindsurfProviderDiscoversAndParsesWorkspaceSQLiteChat(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"windsurf-session-1",
		"How do I add support?",
		"Use the existing parser.",
	))
	provider := newTestWindsurfProvider(root)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]
	assert.Equal(t, AgentWindsurf, source.Provider)
	assert.Equal(t, "demo-workspace", source.ProjectHint)
	assert.Equal(t, dbPath+"#windsurf-session-1", source.DisplayPath)

	fp, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	require.NotZero(t, fp.Size)
	require.NotZero(t, fp.MTimeNS)
	require.NotEmpty(t, fp.Hash)

	out, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fp,
		Machine:     "machine-a",
	})
	require.NoError(t, err)
	require.True(t, out.ResultSetComplete)
	require.Len(t, out.Results, 1)

	result := out.Results[0].Result
	assert.Equal(t, "windsurf:windsurf-session-1", result.Session.ID)
	assert.Equal(t, AgentWindsurf, result.Session.Agent)
	assert.Equal(t, "demo-workspace", result.Session.Project)
	assert.Equal(t, "machine-a", result.Session.Machine)
	assert.Equal(t, source.DisplayPath, result.Session.File.Path)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "How do I add support?", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.Equal(t, "Use the existing parser.", result.Messages[1].Content)
}

func TestWindsurfProviderStreamingDiscoveryKeepsFirstDuplicateSession(t *testing.T) {
	firstPayload := windsurfVSCodeSessionJSON(
		"duplicate-session",
		"Question from the first key",
		"Answer from the first key.",
	)
	root, dbPath := windsurfProviderFixture(t, firstPayload)
	insertWindsurfStateRow(
		t,
		dbPath,
		"aiChat.chatdata",
		windsurfVSCodeSessionJSON(
			"duplicate-session",
			"Question from the second key",
			"Answer from the second key.",
		),
	)
	provider := newTestWindsurfProvider(root)
	baselineRoot, _ := windsurfProviderFixture(t, firstPayload)
	baselineProvider := newTestWindsurfProvider(baselineRoot)
	baselineSources, err := baselineProvider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, baselineSources, 1)
	baselineFingerprint, err := baselineProvider.Fingerprint(t.Context(), baselineSources[0])
	require.NoError(t, err)

	ctx, cleanup, err := WithReconciliationCache(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })
	var sources []SourceRef
	err = provider.(StreamingDiscoverer).DiscoverEach(ctx, func(source SourceRef) error {
		sources = append(sources, source)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(ctx, sources[0])
	require.NoError(t, err)
	assert.Equal(t, baselineFingerprint.Hash, fingerprint.Hash)
	assert.Equal(t, baselineFingerprint.Size, fingerprint.Size)

	out, err := provider.Parse(ctx, ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	messages := out.Results[0].Result.Messages
	require.Len(t, messages, 2)
	assert.Equal(t, "Question from the first key", messages[0].Content)
	assert.Equal(t, "Answer from the first key.", messages[1].Content)
}

func TestWindsurfProviderRehydratesExactVirtualSourceWithoutContainerFanout(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"windsurf-exact", "Exact?", "Exact.",
	))
	provider := newTestWindsurfProvider(root)
	virtualPath := dbPath + "#windsurf-exact"

	resolver, ok := provider.(ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(
		t.Context(), virtualPath, "spooled-project",
	)

	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, virtualPath, source.DisplayPath)
	assert.Equal(t, "spooled-project", source.ProjectHint)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = resolver.SourceForReconciliation(ctx, virtualPath, "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWindsurfProviderFingerprintHashIsContentBased(t *testing.T) {
	payload := windsurfVSCodeSessionJSON(
		"windsurf-session-hash",
		"Hash this",
		"Same content.",
	)
	rootA, _ := windsurfProviderFixture(t, payload)
	rootB, _ := windsurfProviderFixture(t, payload)
	providerA := newTestWindsurfProvider(rootA)
	providerB := newTestWindsurfProvider(rootB)

	sourcesA, err := providerA.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sourcesA, 1)
	sourcesB, err := providerB.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sourcesB, 1)
	fpA, err := providerA.Fingerprint(context.Background(), sourcesA[0])
	require.NoError(t, err)
	fpB, err := providerB.Fingerprint(context.Background(), sourcesB[0])
	require.NoError(t, err)

	require.NotEmpty(t, fpA.Hash)
	assert.Equal(t, fpA.Hash, fpB.Hash)
}

func TestWindsurfProviderFingerprintIgnoresSHM(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"windsurf-session-shm",
		"Ignore SHM",
		"Use chat data only.",
	))
	provider := newTestWindsurfProvider(root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("first"), 0o644))
	firstTime := mustParseTestTime(t, "2026-06-28T12:00:00Z")
	require.NoError(t, os.Chtimes(dbPath+"-shm", firstTime, firstTime))

	before, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("second"), 0o644))
	secondTime := mustParseTestTime(t, "2026-06-28T13:00:00Z")
	require.NoError(t, os.Chtimes(dbPath+"-shm", secondTime, secondTime))
	after, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	assert.Equal(t, before.Hash, after.Hash)
	assert.Equal(t, before.Size, after.Size)
	assert.Equal(t, before.MTimeNS, after.MTimeNS)
}

func TestWindsurfProviderParsesTabContainerChatData(t *testing.T) {
	root, _ := windsurfProviderFixture(t, `{
		"tabs": [{
			"tabId": "tab-session",
			"chatTitle": "Tab title",
			"bubbles": [
				{"type": "user", "text": "Question from tab"},
				{"type": "assistant", "text": "Answer from tab"}
			]
		}]
	}`)
	provider := newTestWindsurfProvider(root)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	out, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	result := out.Results[0].Result
	assert.Equal(t, "windsurf:tab-session", result.Session.ID)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "Question from tab", result.Messages[0].Content)
	assert.Equal(t, "Answer from tab", result.Messages[1].Content)
}

func TestWindsurfProviderMalformedChatDataReturnsError(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, `{"tabs":`)
	provider := newTestWindsurfProvider(root)

	_, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentWindsurf,
			DisplayPath:    dbPath + "#corrupt-session",
			FingerprintKey: dbPath + "#corrupt-session",
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "parse windsurf chatdata")
}

func TestWindsurfProviderParsesNumericTabBubbleTypes(t *testing.T) {
	root, _ := windsurfProviderFixture(t, `{
		"tabs": [{
			"tabId": "numeric-tab-session",
			"chatTitle": "Numeric tab",
			"bubbles": [
				{"type": 1, "text": "Numeric question"},
				{"type": 2, "text": "Numeric answer"}
			]
		}]
	}`)
	provider := newTestWindsurfProvider(root)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	out, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0],
	})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	messages := out.Results[0].Result.Messages
	require.Len(t, messages, 2)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "Numeric question", messages[0].Content)
	assert.Equal(t, RoleAssistant, messages[1].Role)
	assert.Equal(t, "Numeric answer", messages[1].Content)
}

func TestWindsurfProviderFallbackSessionIDUsesWorkspaceIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	dbA := filepath.Join(root, "workspaceStorage", "workspace-a", "state.vscdb")
	dbB := filepath.Join(root, "workspaceStorage", "workspace-b", "state.vscdb")
	payload := `{
		"version": 1,
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Missing session"},
			"response": [{"value": "Fallback response"}],
			"timestamp": 1710000000000
		}]
	}`
	writeSourceFile(t, filepath.Join(filepath.Dir(dbA), "workspace.json"), `{"folder":"file:///work/a"}`)
	writeSourceFile(t, filepath.Join(filepath.Dir(dbB), "workspace.json"), `{"folder":"file:///work/b"}`)
	writeWindsurfStateDB(t, dbA, payload)
	writeWindsurfStateDB(t, dbB, payload)
	provider := newTestWindsurfProvider(root)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{
		dbA + "#workspace-workspace-a",
		dbB + "#workspace-workspace-b",
	}, []string{sources[0].DisplayPath, sources[1].DisplayPath})

	var ids []string
	for _, source := range sources {
		out, err := provider.Parse(context.Background(), ParseRequest{
			Source: source,
		})
		require.NoError(t, err)
		require.Len(t, out.Results, 1)
		ids = append(ids, out.Results[0].Result.Session.ID)
	}
	assert.ElementsMatch(t, []string{
		"windsurf:workspace-workspace-a",
		"windsurf:workspace-workspace-b",
	}, ids)
}

func TestWindsurfProviderFindSourceAndChangedPath(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"windsurf-session-lookup",
		"Find me",
		"Found.",
	))
	provider := newTestWindsurfProvider(root)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "windsurf-session-lookup",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath+"#windsurf-session-lookup", found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID:      "windsurf:windsurf-session-lookup",
		StoredFilePath:     dbPath + "#windsurf-session-lookup",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath+"#windsurf-session-lookup", found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:      dbPath + "-wal",
		EventKind: "write",
		WatchRoot: filepath.Join(
			root,
			"workspaceStorage",
		),
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath+"#windsurf-session-lookup", changed[0].DisplayPath)

	manifestChanged, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: filepath.Join(
			filepath.Dir(dbPath),
			"workspace.json",
		),
		EventKind: "write",
		WatchRoot: filepath.Join(
			root,
			"workspaceStorage",
		),
	})
	require.NoError(t, err)
	require.Len(t, manifestChanged, 1)
	assert.Equal(t, dbPath+"#windsurf-session-lookup", manifestChanged[0].DisplayPath)
}

func TestWindsurfProviderDeletedDBChangedPathPreservesStoredArchive(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"deleted-session",
		"Question before delete",
		"Answer before delete.",
	))
	provider := newTestWindsurfProvider(root)
	source := SourceRef{
		Provider:       AgentWindsurf,
		Key:            dbPath + "#deleted-session",
		DisplayPath:    dbPath + "#deleted-session",
		FingerprintKey: dbPath + "#deleted-session",
	}
	require.NoError(t, os.Remove(dbPath))

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path:              dbPath,
		EventKind:         "remove",
		WatchRoot:         filepath.Join(root, "workspaceStorage"),
		StoredSourcePaths: []string{source.DisplayPath},
	})
	require.NoError(t, err)
	assert.Empty(t, changed)

	fp, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, source.FingerprintKey, fp.Key)
	assert.Empty(t, fp.Hash)

	out, err := provider.Parse(context.Background(), ParseRequest{
		Source: source,
	})
	require.NoError(t, err)
	assert.True(t, out.ResultSetComplete)
	assert.False(t, out.ForceReplace)
	assert.Equal(t, SkipNoSession, out.SkipReason)
}

func TestSplitWindsurfVirtualPathRequiresStateDB(t *testing.T) {
	dbPath, sessionID, ok := SplitWindsurfVirtualPath(
		filepath.Join("profile", "workspaceStorage", "hash", "state.vscdb") + "#session",
	)
	require.True(t, ok)
	assert.Equal(t, "session", sessionID)
	assert.True(t, strings.HasSuffix(filepath.ToSlash(dbPath), "/state.vscdb"))

	_, _, ok = SplitWindsurfVirtualPath(
		filepath.Join("profile", "notes#draft.json") + "#session",
	)
	assert.False(t, ok)
}

func TestWindsurfProviderWatchPlan(t *testing.T) {
	provider := newTestWindsurfProvider(filepath.Join(t.TempDir(), "Windsurf", "User"))

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	root := plan.Roots[0]
	assert.True(t, root.Recursive)
	assert.True(t, strings.HasSuffix(filepath.ToSlash(root.Path), "/workspaceStorage"))
	assert.Contains(t, root.IncludeGlobs, "state.vscdb")
	assert.Contains(t, root.IncludeGlobs, "state.vscdb-wal")
	assert.NotContains(t, root.IncludeGlobs, "state.vscdb-*")
	assert.NotContains(t, root.IncludeGlobs, "state.vscdb-shm")
	assert.Contains(t, root.IncludeGlobs, "workspace.json")
}

func TestWindsurfProviderAcceptsWorkspaceStorageRoot(t *testing.T) {
	root, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"workspace-root-session",
		"Root question",
		"Root answer.",
	))
	provider := newTestWindsurfProvider(filepath.Join(root, "workspaceStorage"))

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, dbPath+"#workspace-root-session", sources[0].DisplayPath)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, filepath.Join(root, "workspaceStorage"), plan.Roots[0].Path)
}

func TestWriteWindsurfSessionJSONScopesVirtualSource(t *testing.T) {
	_, dbPath := windsurfProviderFixture(t, `{
		"tabs": [
			{"tabId": "export-a", "bubbles": [{"type": "user", "text": "A only"}]},
			{"tabId": "export-b", "bubbles": [{"type": "user", "text": "B hidden"}]}
		]
	}`)
	var buf bytes.Buffer

	require.NoError(t, WriteWindsurfSessionJSON(&buf, dbPath, "export-a"))

	assert.Contains(t, buf.String(), "export-a")
	assert.Contains(t, buf.String(), "A only")
	assert.NotContains(t, buf.String(), "export-b")
	assert.NotContains(t, buf.String(), "B hidden")
}

func TestWriteSanitizedWindsurfStateDBCopiesOnlyChatKeys(t *testing.T) {
	_, dbPath := windsurfProviderFixture(t, windsurfVSCodeSessionJSON(
		"sanitized-export",
		"Export chat",
		"Do not export secrets.",
	))
	insertWindsurfStateRow(t, dbPath, "extension.secret", "TOP-SECRET")
	outPath := filepath.Join(t.TempDir(), "state.vscdb")

	require.NoError(t, WriteSanitizedWindsurfStateDB(outPath, dbPath))

	conn, err := sql.Open("sqlite3", outPath)
	require.NoError(t, err)
	defer conn.Close()
	rows, err := conn.Query(`SELECT key, value FROM ItemTable ORDER BY key`)
	require.NoError(t, err)
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var key, value string
		require.NoError(t, rows.Scan(&key, &value))
		got[key] = value
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, got, "workbench.panel.aichat.view.aichat.chatdata")
	assert.Contains(t, got["workbench.panel.aichat.view.aichat.chatdata"], "Export chat")
	assert.NotContains(t, got, "extension.secret")
}

func newTestWindsurfProvider(root string) Provider {
	provider, ok := NewProvider(AgentWindsurf, ProviderConfig{
		Roots: []string{root},
	})
	if !ok {
		panic("missing Windsurf provider")
	}
	return provider
}

func windsurfProviderFixture(t *testing.T, payload string) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	writeSourceFile(t, filepath.Join(workspaceDir, "workspace.json"), `{"folder":"file:///work/demo-workspace"}`)
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	writeWindsurfStateDB(t, dbPath, payload)
	return root, dbPath
}

func writeWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"workbench.panel.aichat.view.aichat.chatdata",
		payload,
	)
	require.NoError(t, err)
}

func insertWindsurfStateRow(t *testing.T, dbPath, key, value string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		key,
		value,
	)
	require.NoError(t, err)
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

func windsurfVSCodeSessionJSON(sessionID, user, assistant string) string {
	return `{
		"version": 1,
		"sessionId": "` + sessionID + `",
		"creationDate": 1710000000000,
		"lastMessageDate": 1710000001000,
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "` + user + `"},
			"response": [{"value": "` + assistant + `"}],
			"timestamp": 1710000000000
		}]
	}`
}
