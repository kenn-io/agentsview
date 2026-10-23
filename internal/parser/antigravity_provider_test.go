package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAntigravityProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 4)
	assert.Equal(t, filepath.Join(root, "annotations"), plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, filepath.Join(root, "brain"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, 1, plan.Roots[1].MaxDepth,
		"generated brain trees stay unwatched")
	assert.Equal(t, []string{"*/.system_generated/logs"}, plan.Roots[1].ExtraDirectories,
		"the plaintext transcript directory stays watched")
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.False(t, plan.Roots[2].Recursive)
	assert.Equal(t, filepath.Clean(root), filepath.Clean(plan.Roots[3].Path))
	assert.False(t, plan.Roots[3].Recursive)
	assert.Equal(t, []string{"conversation_summaries.db", "conversation_summaries.db-wal"}, plan.Roots[3].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)
	assert.Equal(t, dbPath, discovered[0].FingerprintKey)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~antigravity:" + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	for _, changedPath := range []string{
		dbPath + "-wal",
		filepath.Join(root, "annotations", id+".pbtxt"),
		filepath.Join(root, "brain", id, "plan.md"),
	} {
		changed, err := provider.SourcesForChangedPath(
			t.Context(),
			ChangedPathRequest{Path: changedPath, EventKind: "write"},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, dbPath, changed[0].DisplayPath)
	}
}

func TestAntigravityProviderFingerprintAndParse(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)
	titles, err := sql.Open("sqlite3", filepath.Join(root, AntigravitySummariesDBName))
	require.NoError(t, err)
	_, err = titles.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
	require.NoError(t, err)
	_, err = titles.ExecContext(t.Context(), `INSERT INTO conversation_summaries VALUES (?, 'Database conversation')`, id)
	require.NoError(t, err)
	require.NoError(t, titles.Close())

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, dbPath, before.Key)
	assert.NotEmpty(t, before.Hash)

	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, walWithFramesFixture)
	walTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Greater(t, after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: after,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "antigravity:"+id, result.Result.Session.ID)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, after.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 3)
	assert.Equal(t, "Database conversation", result.Result.Session.SessionName)
	assert.True(t, result.Result.Session.SessionNamePresent)
}

func TestAntigravityProviderStoredPathFreshness(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted Antigravity DB")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(t, dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: staleSource})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestAntigravityProviderRejectsInvalidStoredPaths(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	otherID := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	dbPath := filepath.Join(root, "conversations", id+".db")
	otherDBPath := filepath.Join(root, "conversations", otherID+".db")
	writeAntigravityIDEProviderFixture(t, root, id)
	writeAntigravityIDEProviderFixture(t, root, otherID)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	for _, path := range []string{
		dbPath + "#stale",
		filepath.Join(root, "debug", id+".db"),
		filepath.Join(root, "conversations", id+".txt"),
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       id,
		StoredFilePath:     otherDBPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a stored path for a different session")
}

func TestAntigravityCLIProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	dbPath := filepath.Join(root, "conversations", id+".db")
	implicitPath := filepath.Join(root, "implicit", id+".pb")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 5)
	assert.Equal(t, filepath.Join(root, "brain"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, 1, plan.Roots[0].MaxDepth)
	assert.Equal(t, []string{
		"*/.system_generated/logs",
	}, plan.Roots[0].ExtraDirectories)
	assert.Equal(t, []string{
		"*.md",
		"*.md.metadata.json",
		"transcript.jsonl",
	}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[1].Path)
	assert.False(t, plan.Roots[1].Recursive)
	assert.Equal(t, filepath.Clean(root), filepath.Clean(plan.Roots[2].Path))
	assert.False(t, plan.Roots[2].Recursive)
	assert.Equal(t, []string{"history.jsonl"}, plan.Roots[2].IncludeGlobs)
	assert.Equal(t, filepath.Join(root, "cache"), plan.Roots[3].Path)
	assert.False(t, plan.Roots[3].Recursive)
	assert.Equal(t, []string{"last_conversations.json"}, plan.Roots[3].IncludeGlobs)
	assert.Equal(t, filepath.Join(root, "implicit"), plan.Roots[4].Path)
	assert.False(t, plan.Roots[4].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)
	assert.Equal(t, "/tmp/db-proj", discovered[0].ProjectHint)
	assert.Equal(t, implicitPath, discovered[1].DisplayPath)

	foundConversation, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~antigravity-cli:" + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, foundConversation.DisplayPath)

	foundImplicit, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "implicit-" + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, implicitPath, foundImplicit.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "brain", id, "task.md"),
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.Equal(t, dbPath, changed[0].DisplayPath)
	assert.Equal(t, implicitPath, changed[1].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "history.jsonl"),
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.Equal(t, dbPath, changed[0].DisplayPath)
	assert.Equal(t, implicitPath, changed[1].DisplayPath)

	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "history.jsonl"),
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		dbPath,
		filepath.Join(root, "conversations", otherID+".db"),
		implicitPath,
	)
}

func TestAntigravityCLIProviderUsesLastConversationsWorkspace(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)
	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(`{"/tmp/cache-proj":"`+id+`"}`))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	conversation := discovered[0]
	assert.Equal(t, "/tmp/cache-proj", conversation.ProjectHint)
	assert.Equal(t, SourceCwdResolved, conversation.CwdResolution.State)
	assert.Equal(t, "/tmp/cache-proj", conversation.CwdResolution.Path)
	parsed, err := provider.Parse(t.Context(), ParseRequest{
		Source: conversation, Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	assert.Equal(t, "cache_proj", parsed.Results[0].Result.Session.Project)
	assert.Equal(t, "/tmp/cache-proj", parsed.Results[0].Result.Session.Cwd)
	before, err := provider.Fingerprint(t.Context(), conversation)
	require.NoError(t, err)

	mustWrite(t, cachePath, []byte(`{"/tmp/cache-proj-2":"`+id+`"}`))
	changed, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path:      cachePath,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cache"),
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 2)
	conversation = changed[0]
	assert.Equal(t, "/tmp/cache-proj-2", conversation.CwdResolution.Path)
	after, err := provider.Fingerprint(t.Context(), conversation)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
}

func TestAntigravityCLIProviderMarksRemoteCwd(t *testing.T) {
	tests := map[string]func(string) ProviderConfig{
		"path rewriter": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots:        []string{root},
				Machine:      "localbox",
				PathRewriter: func(path string) string { return "remote:" + path },
			}
		},
		"foreign source machine": func(root string) ProviderConfig {
			return ProviderConfig{
				Roots:   []string{root},
				Machine: "localbox",
				SourceMachines: map[string]string{
					root: "archivebox",
				},
			}
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "44444444-5555-6666-7777-888888888888"
			writeAntigravityCLIProviderFixture(t, root, id)

			provider, ok := NewProvider(AgentAntigravityCLI, config(root))
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: id,
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, SourceCwdRemote, source.CwdResolution.State)
			assert.Empty(t, source.CwdResolution.Path)
			assert.Equal(t, "/tmp/db-proj", source.ProjectHint)
		})
	}
}

func TestAntigravityCLIProviderForeignMachineSkipsLocalProjectDiscovery(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)

	workspaceRoot := t.TempDir()
	localRepo := filepath.Join(workspaceRoot, "conflicting-local-repo")
	workspace := filepath.Join(localRepo, "recorded-project")
	mustMkdir(t, filepath.Join(localRepo, ".git"))
	mustMkdir(t, workspace)
	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(fmt.Sprintf(`{%q:%q}`, workspace, id)))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "localbox",
		SourceMachines: map[string]string{
			root: "archivebox",
		},
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	assert.Equal(t, "recorded_project", parsed.Results[0].Result.Session.Project)
}

func TestAntigravityCLIProviderParseCanSkipRecordedWorkspaceDiscovery(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)

	workspaceRoot := t.TempDir()
	repository := filepath.Join(workspaceRoot, "outer-repository")
	workspace := filepath.Join(repository, "recorded-project")
	mustMkdir(t, filepath.Join(repository, ".git"))
	mustMkdir(t, workspace)

	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(fmt.Sprintf(`{%q:%q}`, workspace, id)))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
	require.NoError(t, err)
	require.True(t, ok)

	ctx := WithoutFilesystemProjectDiscovery(t.Context())
	parsed, err := provider.Parse(ctx, ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	assert.Equal(t, "recorded_project", parsed.Results[0].Result.Session.Project)
	assert.Equal(t, workspace, parsed.Results[0].Result.Session.Cwd)
}

func TestAntigravityCLIProviderDiscoverEachReportsHistoryReadError(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "history.jsonl"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)

	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error { return nil })
	require.Error(t, err)
}

func TestAntigravityCLIProviderHistoryRemovalInvalidatesAllSources(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	writeAntigravityCLIProviderFixture(t, root, id)
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	historyPath := filepath.Join(root, "history.jsonl")
	require.NoError(t, os.Remove(historyPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      historyPath,
			WatchRoot: root,
			EventKind: "remove",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)
}

func TestAntigravityCLIProviderHistoryTruncationInvalidatesAllSources(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	writeAntigravityCLIProviderFixture(t, root, id)
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	historyPath := filepath.Join(root, "history.jsonl")
	mustWrite(t, historyPath, nil)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      historyPath,
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)
}

func TestAntigravityCLIProviderHistoryReadErrorInvalidatesAllSources(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	writeAntigravityCLIProviderFixture(t, root, id)
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	historyPath := filepath.Join(root, "history.jsonl")
	mustWrite(t, historyPath, []byte(strings.Repeat("x", 4*1024*1024+1)))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      historyPath,
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)
}

func TestAntigravityCLIProviderHistoryRetagInvalidatesAllSources(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	writeAntigravityCLIProviderFixture(t, root, id)
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	historyPath := filepath.Join(root, "history.jsonl")
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      historyPath,
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)

	mustWrite(t, historyPath,
		[]byte(`{"display":"retagged prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/other","conversationId":"`+otherID+`"}`))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      historyPath,
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)
}

func TestAntigravityCLIProviderUntaggedHistoryInvalidatesAllSources(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	writeAntigravityCLIProviderFixture(t, root, id)
	mustWrite(t, filepath.Join(root, "conversations", otherID+".db"), []byte("db"))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"untagged prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/fallback"}`))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "history.jsonl"),
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(t, err)
	assertAntigravityCLISourcePaths(t, changed,
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", otherID+".db"),
		filepath.Join(root, "implicit", id+".pb"),
	)
}

func TestAntigravityCLIProviderFingerprintParseAndRetry(t *testing.T) {
	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityUndecodableDB(t, dbPath, 3)
	writeAntigravityTestSidecar(t, root, id, 2)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, dbPath, before.Key)
	assert.NotEmpty(t, before.Hash)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	sidecarTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(t, os.Chtimes(sidecarPath, sidecarTime, sidecarTime))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Greater(t, after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: after,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionNeedsRetry, result.DataVersion)
	assert.NotEmpty(t, result.RetryReason)
	assert.Equal(t, "antigravity-cli:"+id, result.Result.Session.ID)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, after.Hash, result.Result.Session.File.Hash)
	assert.NotEmpty(t, result.Result.Messages)
}

func TestAntigravityProviderFingerprintTracksSideInputs(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	mustWrite(t,
		filepath.Join(root, "annotations", id+".pbtxt"),
		[]byte("last_user_view_time:{seconds:1779326599 nanos:0}\n"))
	afterAnnotation, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterAnnotation.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "plan.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterAnnotation.Hash, afterBrain.Hash)

	require.NoError(t, os.Remove(filepath.Join(root, "brain", id, "plan.md")))
	afterDelete, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterBrain.Hash, afterDelete.Hash)
}

func TestAntigravityCLIProviderFindSourceCanonicalizesStoredConversationPath(t *testing.T) {
	root := t.TempDir()
	id := "55555555-6666-7777-8888-999999999999"
	mustMkdir(t, filepath.Join(root, "conversations"))
	pbPath := filepath.Join(root, "conversations", id+".pb")
	dbPath := filepath.Join(root, "conversations", id+".db")
	mustWrite(t, pbPath, []byte("pb"))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, pbPath, found.DisplayPath)

	mustWrite(t, dbPath, []byte("db"))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, pbPath, found.DisplayPath)
}

func TestAntigravityCLIProviderStoredPathFreshness(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Remove(filepath.Join(root, "conversations", id+".pb")))
	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted Antigravity CLI source")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(t, dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: staleSource})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestAntigravityCLIProviderRejectsInvalidStoredPaths(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	dbPath := filepath.Join(root, "conversations", id+".db")
	otherDBPath := filepath.Join(root, "conversations", otherID+".db")
	writeAntigravityCLIProviderFixture(t, root, id)
	writeAntigravityCLIProviderFixture(t, root, otherID)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	for _, path := range []string{
		dbPath + "#stale",
		filepath.Join(root, "debug", id+".db"),
		filepath.Join(root, "conversations", id+".txt"),
		filepath.Join(root, "implicit", id+".db"),
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       id,
		StoredFilePath:     otherDBPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a stored path for a different session")
}

func TestAntigravityCLIProviderFingerprintTracksSideInputs(t *testing.T) {
	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	implicitPath := filepath.Join(root, "implicit", id+".pb")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	relevantHistory := `{"display":"changed prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/db-proj","conversationId":"` + id + `"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(relevantHistory))
	afterHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterHistory.Hash)

	unrelatedHistory := relevantHistory + "\n" +
		`{"display":"other prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/other","conversationId":"88888888-9999-aaaa-bbbb-cccccccccccc"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(unrelatedHistory))
	afterUnrelatedHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, afterHistory.Hash, afterUnrelatedHistory.Hash)

	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(unrelatedHistory+"\n"+
			`{"display":"untagged prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/fallback"}`))
	afterUntaggedHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterUnrelatedHistory.Hash, afterUntaggedHistory.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "task.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterUntaggedHistory.Hash, afterBrain.Hash)

	writeAntigravityTestSidecar(t, root, id, 3)
	afterSidecar, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterBrain.Hash, afterSidecar.Hash)

	implicitSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: antigravityImplicitTag + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	beforeImplicit, err := provider.Fingerprint(t.Context(), implicitSource)
	require.NoError(t, err)

	mustWrite(t,
		strings.TrimSuffix(implicitPath, ".pb")+".trajectory.json",
		[]byte(`{"trajectoryId":"implicit","steps":[]}`))
	afterImplicit, err := provider.Fingerprint(t.Context(), implicitSource)
	require.NoError(t, err)
	assert.NotEqual(t, beforeImplicit.Hash, afterImplicit.Hash)
}

func writeAntigravityIDEProviderFixture(t *testing.T, root, id string) {
	t.Helper()
	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "annotations"))
	mustMkdir(t, filepath.Join(root, "brain", id))
	createAntigravityTestDB(t, filepath.Join(root, "conversations", id+".db"))
	mustWrite(t,
		filepath.Join(root, "annotations", id+".pbtxt"),
		[]byte("last_user_view_time:{seconds:1779326586 nanos:0}\n"))
	mustWrite(t, filepath.Join(root, "brain", id, "plan.md"), []byte("# Plan"))
	mustWrite(t,
		filepath.Join(root, "brain", id, "plan.md.metadata.json"),
		[]byte(`{"summary":"Plan summary","updatedAt":"2026-05-20T22:47:27Z"}`))
}

func writeAntigravityCLIProviderFixture(t *testing.T, root, id string) {
	t.Helper()
	mustMkdir(t, filepath.Join(root, "conversations"))
	mustMkdir(t, filepath.Join(root, "implicit"))
	mustMkdir(t, filepath.Join(root, "brain", id))
	createAntigravityTestDB(t, filepath.Join(root, "conversations", id+".db"))
	mustWrite(t, filepath.Join(root, "conversations", id+".pb"),
		[]byte("old-encrypted-placeholder"))
	mustWrite(t, filepath.Join(root, "implicit", id+".pb"), []byte("implicit"))
	mustWrite(t, filepath.Join(root, "brain", id, "task.md"), []byte("# Task"))
	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(`{"display":"db prompt fallback","timestamp":1779000000000,`+
			`"workspace":"/tmp/db-proj","conversationId":"`+id+`"}`))
}

func assertAntigravityCLISourcePaths(
	t *testing.T,
	sources []SourceRef,
	want ...string,
) {
	t.Helper()
	got := make([]string, 0, len(sources))
	for _, source := range sources {
		got = append(got, source.DisplayPath)
	}
	assert.Equal(t, want, got)
}

// TestAntigravityCLIDiscoverBuildsProjectMapOncePerRoot guards against
// rebuilding the history.jsonl project map for every discovered source.
// buildAntigravityProjectMap reads and per-line-parses history.jsonl, and the
// per-source fallback fired for every project-less session, so discovery scaled
// with session count.
func TestAntigravityCLIDiscoverBuildsProjectMapOncePerRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "conversations"))
	// No history.jsonl: every session is project-less, which is exactly what
	// triggered the per-source map rebuild.
	for _, id := range []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	} {
		mustWrite(t, filepath.Join(root, "conversations", id+".pb"), []byte("x"))
	}

	var calls int
	orig := buildAntigravityProjectMap
	buildAntigravityProjectMap = func(p string) map[string]string {
		calls++
		return orig(p)
	}
	t.Cleanup(func() { buildAntigravityProjectMap = orig })

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(t, ok)

	srcs, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, srcs, 3)
	assert.Equal(t, 1, calls,
		"history.jsonl project map should be built once per root, not per source")
}

// TestAntigravityProviderRoutesTrajectorySidecar verifies the IDE
// provider watches for and routes a conversations/<id>.trajectory.json
// sidecar write back to its .db source, and that a sidecar change moves
// the composite fingerprint so the session re-syncs.
func TestAntigravityProviderRoutesTrajectorySidecar(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 4)
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.Contains(t, plan.Roots[2].IncludeGlobs, "*.trajectory.json",
		"conversations watch must include the trajectory sidecar")

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sidecarPath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath,
		"a sidecar write must route to its .db source")

	// A sidecar with no matching .db must not route to a phantom source.
	orphan := filepath.Join(root, "conversations",
		"ffffffff-ffff-ffff-ffff-ffffffffffff.trajectory.json")
	mustWrite(t, orphan, []byte("{}"))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: orphan, EventKind: "write"},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

// TestAntigravityProviderFingerprintTracksTrajectorySidecar verifies the
// composite fingerprint changes when the agy-reader sidecar appears or
// is updated, so a sidecar-only change triggers a re-sync.
func TestAntigravityProviderFingerprintTracksTrajectorySidecar(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, sidecarPath, []byte(`{"steps":[]}`))
	afterCreate, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterCreate.Hash,
		"sidecar creation must change the fingerprint")

	mustWrite(t, sidecarPath, []byte(`{"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT"}]}`))
	afterUpdate, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterCreate.Hash, afterUpdate.Hash,
		"sidecar update must change the fingerprint")
}

// TestAntigravityProviderCapabilitiesAdvertiseSidecarContent guards that
// the IDE provider advertises the richer content the trajectory sidecar
// makes available, matching the CLI provider.
func TestAntigravityProviderCapabilitiesAdvertiseSidecarContent(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentAntigravity)
	require.True(t, ok)
	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.Thinking)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(t, CapabilitySupported, caps.Content.Model)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
}

// antigravitySQLiteProviderCase describes one Antigravity provider whose
// sessions are WAL-mode SQLite files under conversations/.
type antigravitySQLiteProviderCase struct {
	agent             AgentType
	fixture           func(t *testing.T, root, id string)
	fileInfo          func(path string) (os.FileInfo, error)
	conversationGlobs []string
}

func antigravitySQLiteProviderCases() map[string]antigravitySQLiteProviderCase {
	return map[string]antigravitySQLiteProviderCase{
		"ide": {
			agent:             AgentAntigravity,
			fixture:           writeAntigravityIDEProviderFixture,
			fileInfo:          AntigravityFileInfo,
			conversationGlobs: []string{"*.db", "*.db-wal", "*.trajectory.json"},
		},
		"cli": {
			agent:             AgentAntigravityCLI,
			fixture:           writeAntigravityCLIProviderFixture,
			fileInfo:          AntigravityCLIFileInfo,
			conversationGlobs: []string{"*.db", "*.db-wal", "*.pb", "*.trajectory.json"},
		},
	}
}

// TestAntigravityProvidersIgnoreBareShmEvents pins that a -shm write
// never resolves to a session. Every parse opens the session DB, and a
// reader's open rewrites the -shm index, so honoring that event would make
// each parse schedule the next one. Committed writes land in the main file
// or the -wal and must keep resolving.
func TestAntigravityProvidersIgnoreBareShmEvents(t *testing.T) {
	for name, tc := range antigravitySQLiteProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			conversations := filepath.Join(root, "conversations")
			dbPath := filepath.Join(conversations, id+".db")
			tc.fixture(t, root, id)
			writeSourceFile(t, dbPath+"-wal", "wal")
			writeSourceFile(t, dbPath+"-shm", "shm")

			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)

			plan, err := provider.WatchPlan(t.Context())
			require.NoError(t, err)
			var globs []string
			for _, watchRoot := range plan.Roots {
				if watchRoot.Path == conversations {
					globs = watchRoot.IncludeGlobs
				}
			}
			assert.Equal(t, tc.conversationGlobs, globs)

			for _, event := range []struct {
				path string
				want []string
			}{
				{path: dbPath, want: []string{dbPath}},
				{path: dbPath + "-wal", want: []string{dbPath}},
				{path: dbPath + "-shm", want: []string{}},
			} {
				changed, err := provider.SourcesForChangedPath(
					t.Context(),
					ChangedPathRequest{
						Path:      event.path,
						EventKind: "write",
						WatchRoot: conversations,
					},
				)
				require.NoError(t, err)
				got := make([]string, 0, len(changed))
				for _, source := range changed {
					got = append(got, source.DisplayPath)
				}
				assert.Equal(t, event.want, got, "changed path %s", event.path)
			}
		})
	}
}

// TestAntigravityProvidersFingerprintIgnoresShm pins that rewriting the
// -shm index leaves both the provider fingerprint and the legacy effective
// file info unchanged, while -wal and main-file writes still move them.
func TestAntigravityProvidersFingerprintIgnoresShm(t *testing.T) {
	for name, tc := range antigravitySQLiteProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			dbPath := filepath.Join(root, "conversations", id+".db")
			walPath := dbPath + "-wal"
			shmPath := dbPath + "-shm"
			tc.fixture(t, root, id)
			writeSourceFile(t, walPath, "wal")
			writeSourceFile(t, shmPath, "shm")
			// Later than every fixture companion, so the database files
			// decide the composite mtime.
			base := time.Now().Add(time.Hour).Truncate(time.Second)
			for _, path := range []string{dbPath, walPath, shmPath} {
				require.NoError(t, os.Chtimes(path, base, base))
			}

			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: id,
			})
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, dbPath, source.DisplayPath)
			before, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			beforeInfo, err := tc.fileInfo(dbPath)
			require.NoError(t, err)

			// A reader's open rewrites the index: new bytes, newer mtime.
			shmTime := base.Add(time.Hour)
			writeSourceFile(t, shmPath, "wal-index rebuilt by a reader")
			require.NoError(t, os.Chtimes(shmPath, shmTime, shmTime))
			after, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			afterInfo, err := tc.fileInfo(dbPath)
			require.NoError(t, err)
			assert.Equal(t, beforeInfo, afterInfo)

			walTime := base.Add(2 * time.Hour)
			writeSourceFile(t, walPath, walWithFramesFixture)
			require.NoError(t, os.Chtimes(walPath, walTime, walTime))
			afterWAL, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, before.Hash, afterWAL.Hash)
			assert.Equal(t, walTime.UnixNano(), afterWAL.MTimeNS)

			dbTime := base.Add(3 * time.Hour)
			require.NoError(t, os.Chtimes(dbPath, dbTime, dbTime))
			afterDB, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, afterWAL.Hash, afterDB.Hash)
			assert.Equal(t, dbTime.UnixNano(), afterDB.MTimeNS)
		})
	}
}

// TestAntigravityProvidersParseLeavesFingerprintStable reproduces the
// resync loop end to end: a WAL-mode session DB left with -wal and -shm on
// disk and no live connection, as after the app exits without a final
// checkpoint. Each read-only open is then the first connection and rebuilds
// the -shm index, so the parse must not move the fingerprint it was
// scheduled from.
func TestAntigravityProvidersParseLeavesFingerprintStable(t *testing.T) {
	for name, tc := range antigravitySQLiteProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			dbPath := filepath.Join(root, "conversations", id+".db")
			tc.fixture(t, root, id)
			leaveAntigravityWALSidecars(t, dbPath)

			provider, ok := NewProvider(tc.agent, ProviderConfig{
				Roots:   []string{root},
				Machine: "devbox",
			})
			require.True(t, ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: id,
			})
			require.NoError(t, err)
			require.True(t, ok)
			before, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)

			for range 2 {
				outcome, err := provider.Parse(t.Context(), ParseRequest{
					Source:      source,
					Fingerprint: before,
				})
				require.NoError(t, err)
				require.Len(t, outcome.Results, 1)
				after, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
		})
	}
}

// leaveAntigravityWALSidecars converts the fixture DB at dbPath to WAL
// mode and leaves a committed frame in its -wal plus a -shm index on disk
// with no open connection. All three files are backdated so any rewrite
// moves an mtime.
func leaveAntigravityWALSidecars(t *testing.T, dbPath string) {
	t.Helper()
	staging := filepath.Join(t.TempDir(), "staging.db")
	data, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, data, 0o644))

	db, err := sql.Open("sqlite3", staging)
	require.NoError(t, err)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"PRAGMA user_version=1",
	} {
		_, err := conn.ExecContext(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	// Copy while the connection is open: closing the last connection
	// checkpoints the WAL and deletes both sidecars.
	old := time.Unix(1779000000, 0)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(staging + suffix)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dbPath+suffix, data, 0o644))
		require.NoError(t, os.Chtimes(dbPath+suffix, old, old))
	}
	require.NoError(t, conn.Close())
	require.NoError(t, db.Close())
}

func TestAntigravityProvidersReadOnlyCheckpointedWAL(t *testing.T) {
	for name, tc := range antigravitySQLiteProviderCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			dbPath := filepath.Join(root, "conversations", id+".db")
			tc.fixture(t, root, id)
			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			mustExec(t, db, "PRAGMA journal_mode=WAL; PRAGMA user_version=1;")
			require.NoError(t, db.Close())

			shmPath, convDir := dbPath+"-shm", filepath.Dir(dbPath)
			require.NoError(t, os.Mkdir(shmPath, 0o555))
			require.NoError(t, os.Chmod(convDir, 0o555))
			t.Cleanup(func() { _ = os.Chmod(convDir, 0o755); _ = os.RemoveAll(shmPath) })

			provider, _ := NewProvider(tc.agent, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
			require.NoError(t, err)
			require.True(t, ok)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
			assert.NotEmpty(t, outcome.Results[0].Result.Messages)
		})
	}
}

func TestAntigravityCLIBrainTranscriptAndFailClosed(t *testing.T) {
	const id = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	tests := []struct {
		name      string
		corruptDB bool
		wantErr   bool
	}{
		{"brain transcript wins and preserves gen_metadata usage", false, false},
		{"unreadable db without fallback fails closed", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, "conversations"))
			dbPath := filepath.Join(root, "conversations", id+".db")
			if tc.corruptDB {
				mustWrite(t, dbPath, []byte("not-a-sqlite-database"))
			} else {
				createAntigravityTestDB(t, dbPath)
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err)
				mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
				genData := createAntigravityMockGenMetadata(t, 2400, 180, 0, "Test Gemini 3.5")
				// The planner row is undecodable, so its usage event takes
				// the brain transcript's timestamp for the same step.
				mustExec(t, db, `UPDATE steps SET step_type = 15, step_payload = X'00' WHERE idx = 1`)
				mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (1, ?, ?)`, genData, len(genData))
				require.NoError(t, db.Close())
				tp := antigravityBrainTranscriptPath(root, id)
				mustMkdir(t, filepath.Dir(tp))
				mustWrite(t, tp, []byte(
					`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-10-19T10:00:00Z","content":"Check deployment"}`+"\n"+
						`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-10-19T10:00:05Z","content":"Healthy.","thinking":"Checking","tool_calls":[{"id":"c1","name":"list_jobs","args":{}}]}`+"\n",
				))
			}
			provider, _ := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
			require.NoError(t, err)
			require.True(t, ok)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			res := outcome.Results[0].Result
			require.Len(t, res.Messages, 2)
			assert.Equal(t, TranscriptFidelityFull, res.Session.TranscriptFidelity)
			assert.True(t, res.Messages[1].HasThinking)
			assert.Equal(t, "Test Gemini 3.5", res.Messages[1].Model)
			assert.Equal(t, 2400, res.Messages[1].ContextTokens)
			assert.Equal(t, 180, res.Messages[1].OutputTokens)
			require.Len(t, res.UsageEvents, 1)
			assert.Equal(t, 2400, res.UsageEvents[0].InputTokens)
			assert.Equal(t, "2026-10-19T10:00:05Z", res.UsageEvents[0].OccurredAt)
		})
	}
}

// antigravityCLIBrainFixture describes one CLI conversation. A nil setupDB
// writes an unreadable .db, and pb writes an encrypted .pb stand-in instead.
type antigravityCLIBrainFixture struct {
	pb         bool
	setupDB    func(t *testing.T, db *sql.DB)
	transcript string
	history    string
	sidecar    string
}

// write lays out the fixture under a fresh root and returns the session path.
func (f antigravityCLIBrainFixture) write(t *testing.T, id string) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "conversations"))
	path := filepath.Join(root, "conversations", id+".db")
	switch {
	case f.pb:
		path = filepath.Join(root, "conversations", id+".pb")
		mustWrite(t, path, []byte("encrypted-pb-placeholder"))
	case f.setupDB == nil:
		mustWrite(t, path, []byte("not-a-sqlite-database"))
	default:
		db, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		createAntigravityStepTables(t, db)
		f.setupDB(t, db)
		require.NoError(t, db.Close())
	}
	if f.transcript != "" {
		tp := antigravityBrainTranscriptPath(root, id)
		mustMkdir(t, filepath.Dir(tp))
		mustWrite(t, tp, []byte(f.transcript))
	}
	if f.history != "" {
		mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(f.history))
	}
	if f.sidecar != "" {
		sidecarPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".trajectory.json"
		mustWrite(t, sidecarPath, []byte(f.sidecar))
	}
	return path
}

func antigravityTestStepPayload(unix uint64, text string) []byte {
	ts := encodePB([]pbField{{num: 1, wire: pbWireVarint, varint: unix}})
	return encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes, bytes: []byte(text)},
	})
}

func TestAntigravityCLIBrainTranscriptSourceSelection(t *testing.T) {
	const id = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	const coveringSidecar = `{"steps":[
		{"type":"CORTEX_STEP_TYPE_USER_INPUT","metadata":{"createdAt":"2026-05-17T06:40:00Z"},"userInput":{"userResponse":"Check deployment"}},
		{"type":"CORTEX_STEP_TYPE_PLANNER_RESPONSE","metadata":{"createdAt":"2026-05-17T06:41:00Z"},"plannerResponse":{"response":"sidecar reply body"}}
	]}`
	undecodableSteps := func(kinds ...int) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			t.Helper()
			for idx, kind := range kinds {
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, X'00')`, idx, kind)
			}
		}
	}
	decodedSteps := func(t *testing.T, db *sql.DB) {
		t.Helper()
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`,
			antigravityTestStepPayload(1779000000, "user prompt text goes here"))
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 17, ?)`,
			antigravityTestStepPayload(1779000100, "assistant reply content body"))
	}
	decodedStepsAroundUndecodableTool := func(t *testing.T, db *sql.DB) {
		t.Helper()
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`,
			antigravityTestStepPayload(1779000000, "user prompt text goes here"))
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 21, X'00')`)
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (2, 17, ?)`,
			antigravityTestStepPayload(1779000100, "assistant reply content body"))
	}
	replyLine := func(idx int) string {
		return `{"step_index":` + strconv.Itoa(idx) + `,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-05-17T06:41:40Z","content":"assistant reply content body"}` + "\n"
	}
	userLine := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-05-17T06:40:00Z","content":"Check deployment"}` + "\n"
	assistantLine := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-05-17T06:41:40Z","content":"Healthy."}` + "\n"
	history := `{"conversationId":"` + id + `","timestamp":1779000000000,"display":"Check deployment"}` + "\n"

	tests := []struct {
		name           string
		fixture        antigravityCLIBrainFixture
		wantErr        bool
		wantNeedsRetry bool
		wantContents   []string
		wantUnixMilli  []int64
	}{
		{
			name:         "unreadable db keeps covering sidecar when a brain transcript exists",
			fixture:      antigravityCLIBrainFixture{transcript: userLine, sidecar: coveringSidecar},
			wantContents: []string{"Check deployment", "sidecar reply body"},
		},
		{
			name:    "unreadable db with a brain transcript fails closed",
			fixture: antigravityCLIBrainFixture{transcript: userLine + assistantLine, history: history},
			wantErr: true,
		},
		{
			name: "partial sidecar beats a partial brain transcript",
			fixture: antigravityCLIBrainFixture{
				setupDB: undecodableSteps(14, 15, 15), transcript: userLine, sidecar: coveringSidecar,
			},
			wantNeedsRetry: true,
			wantContents:   []string{"Check deployment", "sidecar reply body"},
		},
		{
			name: "empty brain entries do not replace a decoded db reply",
			fixture: antigravityCLIBrainFixture{
				setupDB: decodedSteps,
				transcript: userLine +
					`{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","created_at":"2026-05-17T06:40:01Z"}` + "\n" +
					`{"step_index":2,"source":"SYSTEM","type":"CONVERSATION_HISTORY","created_at":"2026-05-17T06:40:02Z"}` + "\n",
			},
			wantContents: []string{"user prompt text goes here", "assistant reply content body"},
		},
		{
			name: "undecodable middle step missing from brain transcript keeps db decode",
			fixture: antigravityCLIBrainFixture{
				setupDB:    decodedStepsAroundUndecodableTool,
				transcript: userLine + replyLine(2),
			},
			wantContents: []string{"user prompt text goes here", "assistant reply content body"},
		},
		{
			name: "empty brain line covers an undecodable middle step",
			fixture: antigravityCLIBrainFixture{
				setupDB: decodedStepsAroundUndecodableTool,
				transcript: userLine +
					`{"step_index":1,"source":"SYSTEM","type":"RUN_COMMAND","created_at":"2026-05-17T06:40:10Z"}` + "\n" +
					replyLine(2),
			},
			wantContents: []string{"Check deployment", "", "assistant reply content body"},
		},
		{
			name: "system brain step covers a decoded db step of another role",
			fixture: antigravityCLIBrainFixture{
				setupDB: decodedSteps,
				transcript: userLine +
					`{"step_index":1,"source":"SYSTEM","type":"SYSTEM_MESSAGE","created_at":"2026-05-17T06:41:40Z","content":"assistant reply content body"}` + "\n",
			},
			wantContents: []string{"Check deployment", "assistant reply content body"},
		},
		{
			name: "partial brain transcript keeps repeated history prompts within one second",
			fixture: antigravityCLIBrainFixture{
				setupDB: undecodableSteps(14, 15, 14, 15),
				transcript: `{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-05-17T06:40:00.900Z","content":"Check deployment"}` + "\n" +
					`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-05-17T06:40:30Z","content":"Healthy."}` + "\n",
				history: `{"conversationId":"` + id + `","timestamp":1779000000100,"display":"Check deployment"}` + "\n" +
					`{"conversationId":"` + id + `","timestamp":1779000000900,"display":"Check deployment"}` + "\n",
			},
			wantNeedsRetry: true,
			wantContents:   []string{"Check deployment", "Check deployment", "Healthy."},
			wantUnixMilli:  []int64{1779000000100, 1779000000900, 1779000030000},
		},
		{
			name:         "empty db with an assistant-only brain transcript keeps history prompts",
			fixture:      antigravityCLIBrainFixture{setupDB: func(*testing.T, *sql.DB) {}, transcript: assistantLine, history: history},
			wantContents: []string{"Check deployment", "Healthy."},
		},
		{
			name:         "pb assistant-only brain transcript keeps history prompts",
			fixture:      antigravityCLIBrainFixture{pb: true, transcript: assistantLine, history: history},
			wantContents: []string{"Check deployment", "Healthy."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.fixture.write(t, id)
			_, msgs, _, status, err := parseAntigravityCLITestSessionWithStatus(
				t, path, "", "devbox",
			)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantNeedsRetry, status.NeedsRetry)
			var contents []string
			var unixMilli []int64
			for _, m := range msgs {
				contents = append(contents, m.Content)
				unixMilli = append(unixMilli, m.Timestamp.UnixMilli())
			}
			assert.Equal(t, tc.wantContents, contents)
			if tc.wantUnixMilli != nil {
				assert.Equal(t, tc.wantUnixMilli, unixMilli)
			}
		})
	}
}

// A generation whose planner step has not been written yet must not attach
// its tokens or model to the decoded user step it also lists.
func TestAntigravityGenMetadataDoesNotAttachToNonPlannerStep(t *testing.T) {
	const id = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	path := antigravityCLIBrainFixture{setupDB: func(t *testing.T, db *sql.DB) {
		t.Helper()
		mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`,
			antigravityTestStepPayload(1779000000, "user prompt text goes here"))
		mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
		genData := createAntigravityMockGenMetadataForSteps(
			t, 2400, 180, 0, "Test Gemini 3.5", agChatModelMetadataModelDisplayNameField, 0, 1,
		)
		mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?)`, genData, len(genData))
	}}.write(t, id)

	_, msgs, usageEvents, _, err := parseAntigravityCLITestSessionWithStatus(
		t, path, "", "devbox",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Empty(t, msgs[0].Model)
	assert.False(t, msgs[0].HasContextTokens)
	assert.False(t, msgs[0].HasOutputTokens)
	require.Len(t, usageEvents, 1)
	assert.Equal(t, 2400, usageEvents[0].InputTokens)
}

// Generation metadata reaches a brain transcript only through the model's
// own entry. A generation that lists an undecodable user row moves to the
// missing output step; one whose last step is a known non-planner step stays
// an event.
func TestAntigravityBrainTranscriptGenMetadataTargetsModelEntry(t *testing.T) {
	const id = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	transcript := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-05-17T06:40:00Z","content":"Check deployment"}` + "\n" +
		`{"step_index":1,"source":"MODEL","type":"RUN_COMMAND","created_at":"2026-05-17T06:40:20Z","content":"ran the deployment check"}` + "\n"
	tests := []struct {
		name         string
		steps        func(t *testing.T, db *sql.DB)
		wantModelIdx int
		wantOccurred string
	}{
		{
			name: "undecodable user row hands usage to the missing output step",
			steps: func(t *testing.T, db *sql.DB) {
				t.Helper()
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, X'00')`)
			},
			wantModelIdx: 1,
			wantOccurred: "2026-05-17T06:40:20Z",
		},
		{
			name: "undecodable non-planner last step keeps usage as an event",
			steps: func(t *testing.T, db *sql.DB) {
				t.Helper()
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`,
					antigravityTestStepPayload(1779000000, "user prompt text goes here"))
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 21, X'00')`)
			},
			wantModelIdx: -1,
			wantOccurred: "",
		},
		{
			name: "known non-planner last step keeps usage as an event",
			steps: func(t *testing.T, db *sql.DB) {
				t.Helper()
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, ?)`,
					antigravityTestStepPayload(1779000000, "user prompt text goes here"))
				mustExec(t, db, `INSERT INTO steps (idx, step_type, step_payload) VALUES (1, 21, ?)`,
					antigravityTestStepPayload(1779000020, "ran the deployment check command"))
			},
			wantModelIdx: -1,
			wantOccurred: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := antigravityCLIBrainFixture{
				setupDB: func(t *testing.T, db *sql.DB) {
					t.Helper()
					tc.steps(t, db)
					mustExec(t, db, `CREATE TABLE gen_metadata (idx integer, data blob, size integer, PRIMARY KEY (idx))`)
					genData := createAntigravityMockGenMetadataForSteps(
						t, 2400, 180, 0, "Test Gemini 3.5", agChatModelMetadataModelDisplayNameField, 0, 1,
					)
					mustExec(t, db, `INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?)`, genData, len(genData))
				},
				transcript: transcript,
			}.write(t, id)

			_, msgs, usageEvents, _, err := parseAntigravityCLITestSessionWithStatus(
				t, path, "", "devbox",
			)
			require.NoError(t, err)
			require.Len(t, msgs, 2)
			assert.Equal(t, "Check deployment", msgs[0].Content, "brain transcript is selected")
			for i, m := range msgs {
				if i == tc.wantModelIdx {
					assert.Equal(t, "Test Gemini 3.5", m.Model)
					assert.Equal(t, 180, m.OutputTokens)
					continue
				}
				assert.Empty(t, m.Model, "message %d", i)
				assert.False(t, m.HasOutputTokens, "message %d", i)
			}
			require.Len(t, usageEvents, 1)
			assert.Equal(t, 2400, usageEvents[0].InputTokens)
			assert.Equal(t, tc.wantOccurred, usageEvents[0].OccurredAt)
		})
	}
}
