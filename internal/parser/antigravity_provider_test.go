package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAntigravityProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 3)
	assert.Equal(filepath.Join(root, "annotations"), plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal(filepath.Join(root, "brain"), plan.Roots[1].Path)
	assert.True(plan.Roots[1].Recursive)
	assert.Equal(filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.False(plan.Roots[2].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(dbPath, discovered[0].DisplayPath)
	assert.Equal(dbPath, discovered[0].FingerprintKey)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~antigravity:" + id,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(dbPath, found.DisplayPath)

	for _, changedPath := range []string{
		dbPath + "-wal",
		filepath.Join(root, "annotations", id+".pbtxt"),
		filepath.Join(root, "brain", id, "plan.md"),
	} {
		changed, err := provider.SourcesForChangedPath(
			t.Context(),
			ChangedPathRequest{Path: changedPath, EventKind: "write"},
		)
		require.NoError(err)
		require.Len(changed, 1)
		assert.Equal(dbPath, changed[0].DisplayPath)
	}
}

func TestAntigravityProviderFingerprintAndParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(dbPath, before.Key)
	assert.NotEmpty(before.Hash)

	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, "wal")
	walTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(os.Chtimes(walPath, walTime, walTime))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Greater(after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: after,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("antigravity:"+id, result.Result.Session.ID)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(after.Hash, result.Result.Session.File.Hash)
	assert.Len(result.Result.Messages, 3)
}

func TestAntigravityProviderStoredPathFreshness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(dbPath, found.DisplayPath)

	require.NoError(os.Remove(dbPath))
	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a deleted Antigravity DB")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(err)
	require.True(ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: staleSource})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestAntigravityProviderRejectsInvalidStoredPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	otherID := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	dbPath := filepath.Join(root, "conversations", id+".db")
	otherDBPath := filepath.Join(root, "conversations", otherID+".db")
	writeAntigravityIDEProviderFixture(t, root, id)
	writeAntigravityIDEProviderFixture(t, root, otherID)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	for _, path := range []string{
		dbPath + "#stale",
		filepath.Join(root, "debug", id+".db"),
		filepath.Join(root, "conversations", id+".txt"),
	} {
		_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(err)
		assert.False(ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       id,
		StoredFilePath:     otherDBPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a stored path for a different session")
}

func TestAntigravityCLIProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	dbPath := filepath.Join(root, "conversations", id+".db")
	implicitPath := filepath.Join(root, "implicit", id+".pb")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 5)
	assert.Equal(filepath.Join(root, "brain"), plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal(filepath.Join(root, "conversations"), plan.Roots[1].Path)
	assert.False(plan.Roots[1].Recursive)
	assert.Equal(root, plan.Roots[2].Path)
	assert.False(plan.Roots[2].Recursive)
	assert.Equal([]string{"history.jsonl"}, plan.Roots[2].IncludeGlobs)
	assert.Equal(filepath.Join(root, "cache"), plan.Roots[3].Path)
	assert.False(plan.Roots[3].Recursive)
	assert.Equal([]string{"last_conversations.json"}, plan.Roots[3].IncludeGlobs)
	assert.Equal(filepath.Join(root, "implicit"), plan.Roots[4].Path)
	assert.False(plan.Roots[4].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.Equal(dbPath, discovered[0].DisplayPath)
	assert.Equal("/tmp/db-proj", discovered[0].ProjectHint)
	assert.Equal(implicitPath, discovered[1].DisplayPath)

	foundConversation, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~antigravity-cli:" + id,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(dbPath, foundConversation.DisplayPath)

	foundImplicit, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "implicit-" + id,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(implicitPath, foundImplicit.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "brain", id, "task.md"),
			EventKind: "write",
		},
	)
	require.NoError(err)
	require.Len(changed, 2)
	assert.Equal(dbPath, changed[0].DisplayPath)
	assert.Equal(implicitPath, changed[1].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "history.jsonl"),
			WatchRoot: root,
			EventKind: "write",
		},
	)
	require.NoError(err)
	require.Len(changed, 2)
	assert.Equal(dbPath, changed[0].DisplayPath)
	assert.Equal(implicitPath, changed[1].DisplayPath)

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
	require.NoError(err)
	assertAntigravityCLISourcePaths(t, changed,
		dbPath,
		filepath.Join(root, "conversations", otherID+".db"),
		implicitPath,
	)
}

func TestAntigravityCLIProviderUsesLastConversationsWorkspace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)
	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(`{"/tmp/cache-proj":"`+id+`"}`))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	conversation := discovered[0]
	assert.Equal("/tmp/cache-proj", conversation.ProjectHint)
	assert.Equal(SourceCwdResolved, conversation.CwdResolution.State)
	assert.Equal("/tmp/cache-proj", conversation.CwdResolution.Path)
	parsed, err := provider.Parse(t.Context(), ParseRequest{
		Source: conversation, Machine: "devbox",
	})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal("cache_proj", parsed.Results[0].Result.Session.Project)
	assert.Equal("/tmp/cache-proj", parsed.Results[0].Result.Session.Cwd)
	before, err := provider.Fingerprint(t.Context(), conversation)
	require.NoError(err)

	mustWrite(t, cachePath, []byte(`{"/tmp/cache-proj-2":"`+id+`"}`))
	changed, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{
			Path:      cachePath,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "cache"),
		},
	)
	require.NoError(err)
	require.Len(changed, 2)
	conversation = changed[0]
	assert.Equal("/tmp/cache-proj-2", conversation.CwdResolution.Path)
	after, err := provider.Fingerprint(t.Context(), conversation)
	require.NoError(err)
	assert.NotEqual(before.Hash, after.Hash)
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
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			id := "44444444-5555-6666-7777-888888888888"
			writeAntigravityCLIProviderFixture(t, root, id)

			provider, ok := NewProvider(AgentAntigravityCLI, config(root))
			require.True(ok)
			source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: id,
			})
			require.NoError(err)
			require.True(ok)
			assert.Equal(SourceCwdRemote, source.CwdResolution.State)
			assert.Empty(source.CwdResolution.Path)
			assert.Equal("/tmp/db-proj", source.ProjectHint)
		})
	}
}

func TestAntigravityCLIProviderForeignMachineSkipsLocalProjectDiscovery(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)

	workspaceRoot := t.TempDir()
	localRepo := filepath.Join(workspaceRoot, "conflicting-local-repo")
	workspace := filepath.Join(localRepo, "recorded-project")
	mustMkdir(t, filepath.Join(localRepo, ".git"))
	mustMkdir(t, workspace)
	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(fmt.Sprintf(`{%q:%q}`, workspace, id)))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "localbox",
		SourceMachines: map[string]string{
			root: "archivebox",
		},
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)
	parsed, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal(t, "recorded_project", parsed.Results[0].Result.Session.Project)
}

func TestAntigravityCLIProviderParseCanSkipRecordedWorkspaceDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "44444444-5555-6666-7777-888888888888"
	writeAntigravityCLIProviderFixture(t, root, id)

	workspaceRoot := t.TempDir()
	repository := filepath.Join(workspaceRoot, "outer-repository")
	workspace := filepath.Join(repository, "recorded-project")
	mustMkdir(t, filepath.Join(repository, ".git"))
	mustMkdir(t, workspace)

	cachePath := filepath.Join(root, "cache", "last_conversations.json")
	require.NoError(os.MkdirAll(filepath.Dir(cachePath), 0o755))
	mustWrite(t, cachePath, []byte(fmt.Sprintf(`{%q:%q}`, workspace, id)))

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: id})
	require.NoError(err)
	require.True(ok)

	ctx := WithoutFilesystemProjectDiscovery(t.Context())
	parsed, err := provider.Parse(ctx, ParseRequest{Source: source})
	require.NoError(err)
	require.Len(parsed.Results, 1)
	assert.Equal("recorded_project", parsed.Results[0].Result.Session.Project)
	assert.Equal(workspace, parsed.Results[0].Result.Session.Cwd)
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(dbPath, before.Key)
	assert.NotEmpty(before.Hash)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	sidecarTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(os.Chtimes(sidecarPath, sidecarTime, sidecarTime))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Greater(after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: after,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionNeedsRetry, result.DataVersion)
	assert.NotEmpty(result.RetryReason)
	assert.Equal("antigravity-cli:"+id, result.Result.Session.ID)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(after.Hash, result.Result.Session.File.Hash)
	assert.NotEmpty(result.Result.Messages)
}

func TestAntigravityProviderFingerprintTracksSideInputs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	mustWrite(t,
		filepath.Join(root, "annotations", id+".pbtxt"),
		[]byte("last_user_view_time:{seconds:1779326599 nanos:0}\n"))
	afterAnnotation, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(before.Hash, afterAnnotation.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "plan.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterAnnotation.Hash, afterBrain.Hash)

	require.NoError(os.Remove(filepath.Join(root, "brain", id, "plan.md")))
	afterDelete, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterBrain.Hash, afterDelete.Hash)
}

func TestAntigravityCLIProviderFindSourceCanonicalizesStoredConversationPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(pbPath, found.DisplayPath)

	mustWrite(t, dbPath, []byte("db"))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(dbPath, found.DisplayPath)

	require.NoError(os.Remove(dbPath))
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(pbPath, found.DisplayPath)
}

func TestAntigravityCLIProviderStoredPathFreshness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(dbPath, found.DisplayPath)

	require.NoError(os.Remove(dbPath))
	require.NoError(os.Remove(filepath.Join(root, "conversations", id+".pb")))
	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a deleted Antigravity CLI source")

	staleSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(err)
	require.True(ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: staleSource})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestAntigravityCLIProviderRejectsInvalidStoredPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	otherID := "88888888-9999-aaaa-bbbb-cccccccccccc"
	dbPath := filepath.Join(root, "conversations", id+".db")
	otherDBPath := filepath.Join(root, "conversations", otherID+".db")
	writeAntigravityCLIProviderFixture(t, root, id)
	writeAntigravityCLIProviderFixture(t, root, otherID)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{Roots: []string{root}})
	require.True(ok)
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
		require.NoError(err)
		assert.False(ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       id,
		StoredFilePath:     otherDBPath,
		RequireFreshSource: true,
	})
	require.NoError(err)
	assert.False(ok, "fresh lookup must reject a stored path for a different session")
}

func TestAntigravityCLIProviderFingerprintTracksSideInputs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "33333333-4444-5555-6666-777777777777"
	implicitPath := filepath.Join(root, "implicit", id+".pb")
	writeAntigravityCLIProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravityCLI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	relevantHistory := `{"display":"changed prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/db-proj","conversationId":"` + id + `"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(relevantHistory))
	afterHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(before.Hash, afterHistory.Hash)

	unrelatedHistory := relevantHistory + "\n" +
		`{"display":"other prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/other","conversationId":"88888888-9999-aaaa-bbbb-cccccccccccc"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(unrelatedHistory))
	afterUnrelatedHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(afterHistory.Hash, afterUnrelatedHistory.Hash)

	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(unrelatedHistory+"\n"+
			`{"display":"untagged prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/fallback"}`))
	afterUntaggedHistory, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterUnrelatedHistory.Hash, afterUntaggedHistory.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "task.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterUntaggedHistory.Hash, afterBrain.Hash)

	writeAntigravityTestSidecar(t, root, id, 3)
	afterSidecar, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterBrain.Hash, afterSidecar.Hash)

	implicitSource, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: antigravityImplicitTag + id,
	})
	require.NoError(err)
	require.True(ok)
	beforeImplicit, err := provider.Fingerprint(t.Context(), implicitSource)
	require.NoError(err)

	mustWrite(t,
		strings.TrimSuffix(implicitPath, ".pb")+".trajectory.json",
		[]byte(`{"trajectoryId":"implicit","steps":[]}`))
	afterImplicit, err := provider.Fingerprint(t.Context(), implicitSource)
	require.NoError(err)
	assert.NotEqual(beforeImplicit.Hash, afterImplicit.Hash)
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
	require := require.New(t)

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
	require.True(ok)

	srcs, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(srcs, 3)
	assert.Equal(t, 1, calls,
		"history.jsonl project map should be built once per root, not per source")
}

// TestAntigravityProviderRoutesTrajectorySidecar verifies the IDE
// provider watches for and routes a conversations/<id>.trajectory.json
// sidecar write back to its .db source, and that a sidecar change moves
// the composite fingerprint so the session re-syncs.
func TestAntigravityProviderRoutesTrajectorySidecar(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 3)
	assert.Equal(filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.Contains(plan.Roots[2].IncludeGlobs, "*.trajectory.json",
		"conversations watch must include the trajectory sidecar")

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sidecarPath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(dbPath, changed[0].DisplayPath,
		"a sidecar write must route to its .db source")

	// A sidecar with no matching .db must not route to a phantom source.
	orphan := filepath.Join(root, "conversations",
		"ffffffff-ffff-ffff-ffff-ffffffffffff.trajectory.json")
	mustWrite(t, orphan, []byte("{}"))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: orphan, EventKind: "write"},
	)
	require.NoError(err)
	assert.Empty(changed)
}

// TestAntigravityProviderFingerprintTracksTrajectorySidecar verifies the
// composite fingerprint changes when the agy-reader sidecar appears or
// is updated, so a sidecar-only change triggers a re-sync.
func TestAntigravityProviderFingerprintTracksTrajectorySidecar(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(err)
	require.True(ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, sidecarPath, []byte(`{"steps":[]}`))
	afterCreate, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(before.Hash, afterCreate.Hash,
		"sidecar creation must change the fingerprint")

	mustWrite(t, sidecarPath, []byte(`{"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT"}]}`))
	afterUpdate, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.NotEqual(afterCreate.Hash, afterUpdate.Hash,
		"sidecar update must change the fingerprint")
}

// TestAntigravityProviderCapabilitiesAdvertiseSidecarContent guards that
// the IDE provider advertises the richer content the trajectory sidecar
// makes available, matching the CLI provider.
func TestAntigravityProviderCapabilitiesAdvertiseSidecarContent(t *testing.T) {
	assert := assert.New(t)

	factory, ok := ProviderFactoryByType(AgentAntigravity)
	require.True(t, ok)
	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Content.Thinking)
	assert.Equal(CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(CapabilitySupported, caps.Content.Model)
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
}
