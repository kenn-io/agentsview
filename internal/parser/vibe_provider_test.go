package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVibeProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentVibe)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestVibeProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionDir := "session_20260613_123456_abc123def"
	messagesPath := filepath.Join(root, sessionDir, "messages.jsonl")
	metaPath := filepath.Join(root, sessionDir, "meta.json")
	writeSourceFile(t, messagesPath, vibeProviderMessagesFixture("provider question"))
	writeSourceFile(t, metaPath, vibeProviderMetaFixture("uuid-1234", "Provider title"))
	writeSourceFile(t, filepath.Join(root, "scratch", "messages.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "session_missing_messages", "meta.json"), "{}\n")
	nestedPath := filepath.Join(root, "nested", "session_20260613_123456_nested", "messages.jsonl")
	writeSourceFile(t, nestedPath, vibeProviderMessagesFixture("nested"))

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"messages.jsonl", "meta.json"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	source := discovered[0]
	assert.Equal(t, AgentVibe, source.Provider)
	assert.Equal(t, messagesPath, source.DisplayPath)
	assert.Equal(t, messagesPath, source.FingerprintKey)
	assert.Equal(t, sessionDir, source.ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~vibe:uuid-1234",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, messagesPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionDir,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, messagesPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: messagesPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, messagesPath, found.DisplayPath)

	messageInfo, err := os.Stat(messagesPath)
	require.NoError(t, err)
	metaInfo, err := os.Stat(metaPath)
	require.NoError(t, err)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, messagesPath, fingerprint.Key)
	assert.Equal(t, messageInfo.Size()+metaInfo.Size(), fingerprint.Size)
	assert.Equal(
		t,
		max(messageInfo.ModTime().UnixNano(), metaInfo.ModTime().UnixNano()),
		fingerprint.MTimeNS,
	)
	assert.NotEmpty(t, fingerprint.Hash)

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "messages", path: messagesPath, want: messagesPath},
		{name: "meta sidecar", path: metaPath, want: messagesPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: tc.path, EventKind: "write", WatchRoot: root},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, tc.want, changed[0].DisplayPath)
		})
	}

	require.NoError(t, os.Remove(metaPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: metaPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, messagesPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, "scratch", "messages.jsonl"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)

	nested, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: nestedPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	assert.Empty(t, nested)

	require.NoError(t, os.Remove(messagesPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: messagesPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, messagesPath, changed[0].DisplayPath)
	assert.Equal(t, sessionDir, changed[0].ProjectHint)

	wrongRoot, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      messagesPath,
			EventKind: "write",
			WatchRoot: filepath.Join(root, "..", "other-root"),
		},
	)
	require.NoError(t, err)
	assert.Empty(t, wrongRoot)
}

func TestVibeProviderDiscoversSymlinkedSessionDirectory(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	sessionDir := "session_20260613_123456_symlinked"
	targetDir := filepath.Join(targetRoot, sessionDir)
	sourceDir := filepath.Join(root, sessionDir)
	sourcePath := filepath.Join(sourceDir, "messages.jsonl")
	writeSourceFile(
		t,
		filepath.Join(targetDir, "messages.jsonl"),
		vibeProviderMessagesFixture("from symlink"),
	)
	if err := os.Symlink(targetDir, sourceDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionDir,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestVibeProviderParse(t *testing.T) {
	root := t.TempDir()
	sessionDir := "session_20260613_123456_abc123def"
	messagesPath := filepath.Join(root, sessionDir, "messages.jsonl")
	metaPath := filepath.Join(root, sessionDir, "meta.json")
	writeSourceFile(t, messagesPath, vibeProviderMessagesFixture("parse question"))
	writeSourceFile(t, metaPath, vibeProviderMetaFixture("uuid-1234", "Provider title"))

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "vibe:uuid-1234", result.Result.Session.ID)
	assert.Equal(t, AgentVibe, result.Result.Session.Agent)
	assert.Equal(t, "vibe", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, messagesPath, result.Result.Session.File.Path)
	assert.Equal(t, fingerprint.Size, result.Result.Session.File.Size)
	assert.Equal(t, fingerprint.MTimeNS, result.Result.Session.File.Mtime)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, "Provider title", result.Result.Session.SessionName)
	assert.Equal(t, "parse question", result.Result.Session.FirstMessage)
	assert.Contains(t, outcome.ExcludedSessionIDs, "vibe:"+sessionDir)
	assert.Len(t, result.Result.Messages, 2)
}

// TestVibeProviderParseEmitsUsageEvents locks in the usage-event and
// excluded-ID behavior the deleted shadow-baseline test asserted: when
// meta.json carries a model and token stats, Parse must surface a single
// session-level usage event and exclude the directory-name fallback ID.
func TestVibeProviderParseEmitsUsageEvents(t *testing.T) {
	root := t.TempDir()
	sessionDir := "session_20260616_083518_abc123"
	sessionID := "uuid-1234"
	messagesPath := filepath.Join(root, sessionDir, "messages.jsonl")
	metaPath := filepath.Join(root, sessionDir, "meta.json")
	writeSourceFile(t, messagesPath, vibeProviderMessagesFixture("provider question"))
	writeSourceFile(t, metaPath, vibeProviderMetaWithStatsFixture(sessionID, "Provider title"))

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, "vibe:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, []string{"vibe:" + sessionDir}, outcome.ExcludedSessionIDs)

	require.Len(t, result.Result.UsageEvents, 1)
	usageEvent := result.Result.UsageEvents[0]
	assert.Equal(t, "vibe:"+sessionID, usageEvent.SessionID)
	assert.Equal(t, "mistral-medium-3.5", usageEvent.Model)
	assert.Equal(t, 100, usageEvent.InputTokens)
	assert.Equal(t, 40, usageEvent.OutputTokens)
}

func vibeProviderMessagesFixture(firstMessage string) string {
	return `{"role":"user","content":"` + firstMessage + `"}` + "\n" +
		`{"role":"assistant","content":"Done."}` + "\n"
}

func vibeProviderMetaFixture(sessionID, title string) string {
	return `{"session_id":"` + sessionID + `","title":"` + title + `"}`
}

func vibeProviderMetaWithStatsFixture(sessionID, title string) string {
	return `{"session_id":"` + sessionID + `","title":"` + title + `",` +
		`"config":{"active_model":"mistral-medium-3.5"},` +
		`"stats":{"session_prompt_tokens":100,"session_completion_tokens":40}}`
}
