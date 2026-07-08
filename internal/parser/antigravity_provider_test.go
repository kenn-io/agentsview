package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 3)
	assert.Equal(t, filepath.Join(root, "annotations"), plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, filepath.Join(root, "brain"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.False(t, plan.Roots[2].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)
	assert.Equal(t, dbPath, discovered[0].FingerprintKey)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
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
			context.Background(),
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

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, dbPath, before.Key)
	assert.NotEmpty(t, before.Hash)

	walPath := dbPath + "-wal"
	writeSourceFile(t, walPath, "wal")
	walTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))
	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Greater(t, after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
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
}

func TestAntigravityProviderStoredPathFreshness(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeAntigravityIDEProviderFixture(t, root, id)

	provider, ok := NewProvider(AgentAntigravity, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted Antigravity DB")

	staleSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(t, dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: staleSource})
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
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
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

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 4)
	assert.Equal(t, filepath.Join(root, "brain"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[1].Path)
	assert.False(t, plan.Roots[1].Recursive)
	assert.Equal(t, root, plan.Roots[2].Path)
	assert.False(t, plan.Roots[2].Recursive)
	assert.Equal(t, []string{"history.jsonl"}, plan.Roots[2].IncludeGlobs)
	assert.Equal(t, filepath.Join(root, "implicit"), plan.Roots[3].Path)
	assert.False(t, plan.Roots[3].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)
	assert.Equal(t, "/tmp/db-proj", discovered[0].ProjectHint)
	assert.Equal(t, implicitPath, discovered[1].DisplayPath)

	foundConversation, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~antigravity-cli:" + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, foundConversation.DisplayPath)

	foundImplicit, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "implicit-" + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, implicitPath, foundImplicit.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-wal", EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, dbPath, changed[0].DisplayPath)

	changed, err = provider.SourcesForChangedPath(
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, dbPath, before.Key)
	assert.NotEmpty(t, before.Hash)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	sidecarTime := time.Unix(0, before.MTimeNS+int64(time.Second))
	require.NoError(t, os.Chtimes(sidecarPath, sidecarTime, sidecarTime))
	after, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Greater(t, after.MTimeNS, before.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
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
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	mustWrite(t,
		filepath.Join(root, "annotations", id+".pbtxt"),
		[]byte("last_user_view_time:{seconds:1779326599 nanos:0}\n"))
	afterAnnotation, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterAnnotation.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "plan.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterAnnotation.Hash, afterBrain.Hash)

	require.NoError(t, os.Remove(filepath.Join(root, "brain", id, "plan.md")))
	afterDelete, err := provider.Fingerprint(context.Background(), source)
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

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, pbPath, found.DisplayPath)

	mustWrite(t, dbPath, []byte("db"))
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: pbPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
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
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, dbPath, found.DisplayPath)

	require.NoError(t, os.Remove(dbPath))
	require.NoError(t, os.Remove(filepath.Join(root, "conversations", id+".pb")))
	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     dbPath,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "fresh lookup must reject a deleted Antigravity CLI source")

	staleSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: dbPath,
	})
	require.NoError(t, err)
	require.True(t, ok, "non-fresh lookup keeps tombstone source identity")
	assert.Equal(t, dbPath, staleSource.DisplayPath)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: staleSource})
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
		_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			StoredFilePath:     path,
			RequireFreshSource: true,
		})
		require.NoError(t, err)
		assert.False(t, ok, "stored path %q", path)
	}

	_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
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
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	relevantHistory := `{"display":"changed prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/db-proj","conversationId":"` + id + `"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(relevantHistory))
	afterHistory, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterHistory.Hash)

	unrelatedHistory := relevantHistory + "\n" +
		`{"display":"other prompt","timestamp":1779000000000,` +
		`"workspace":"/tmp/other","conversationId":"88888888-9999-aaaa-bbbb-cccccccccccc"}`
	mustWrite(t, filepath.Join(root, "history.jsonl"), []byte(unrelatedHistory))
	afterUnrelatedHistory, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, afterHistory.Hash, afterUnrelatedHistory.Hash)

	mustWrite(t, filepath.Join(root, "history.jsonl"),
		[]byte(unrelatedHistory+"\n"+
			`{"display":"untagged prompt","timestamp":1779000000000,`+
			`"workspace":"/tmp/fallback"}`))
	afterUntaggedHistory, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterUnrelatedHistory.Hash, afterUntaggedHistory.Hash)

	mustWrite(t, filepath.Join(root, "brain", id, "task.md"), []byte("# Changed"))
	afterBrain, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterUntaggedHistory.Hash, afterBrain.Hash)

	writeAntigravityTestSidecar(t, root, id, 3)
	afterSidecar, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterBrain.Hash, afterSidecar.Hash)

	implicitSource, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: antigravityImplicitTag + id,
	})
	require.NoError(t, err)
	require.True(t, ok)
	beforeImplicit, err := provider.Fingerprint(context.Background(), implicitSource)
	require.NoError(t, err)

	mustWrite(t,
		strings.TrimSuffix(implicitPath, ".pb")+".trajectory.json",
		[]byte(`{"trajectoryId":"implicit","steps":[]}`))
	afterImplicit, err := provider.Fingerprint(context.Background(), implicitSource)
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

	srcs, err := provider.Discover(context.Background())
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

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 3)
	assert.Equal(t, filepath.Join(root, "conversations"), plan.Roots[2].Path)
	assert.Contains(t, plan.Roots[2].IncludeGlobs, "*.trajectory.json",
		"conversations watch must include the trajectory sidecar")

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
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
		context.Background(),
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
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: id,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	sidecarPath := filepath.Join(root, "conversations", id+".trajectory.json")
	mustWrite(t, sidecarPath, []byte(`{"steps":[]}`))
	afterCreate, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterCreate.Hash,
		"sidecar creation must change the fingerprint")

	mustWrite(t, sidecarPath, []byte(`{"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT"}]}`))
	afterUpdate, err := provider.Fingerprint(context.Background(), source)
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
