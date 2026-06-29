package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentClaude)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestClaudeProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	subagentPath := filepath.Join(
		root,
		projectDir,
		sessionID,
		"subagents",
		"workflows",
		"wf-123",
		"agent-worker.jsonl",
	)
	writeSourceFile(t, sourcePath, claudeProviderFixture("main question"))
	writeSourceFile(t, subagentPath, claudeProviderFixture("subagent question"))
	writeSourceFile(
		t,
		filepath.Join(root, projectDir, sessionID, "subagents", "not-agent.jsonl"),
		claudeProviderFixture("ignored"),
	)
	writeSourceFile(t, filepath.Join(root, projectDir, "agent-root.jsonl"), claudeProviderFixture("ignored"))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{sourcePath, subagentPath}, []string{
		discovered[0].DisplayPath,
		discovered[1].DisplayPath,
	})
	for _, source := range discovered {
		assert.Equal(t, AgentClaude, source.Provider)
		assert.Equal(t, projectDir, source.ProjectHint)
	}

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "remote~" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "agent-worker",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subagentPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, subagentPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: subagentPath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subagentPath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	require.NoError(t, os.Remove(subagentPath))
	changed, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: subagentPath, EventKind: "rename", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subagentPath, changed[0].DisplayPath)

	ignored, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      filepath.Join(root, projectDir, "agent-root.jsonl"),
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, ignored)
}

func TestClaudeProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	targetProject := filepath.Join(targetRoot, projectDir)
	sourceProject := filepath.Join(root, projectDir)
	sourcePath := filepath.Join(sourceProject, sessionID+".jsonl")
	subagentPath := filepath.Join(
		sourceProject,
		sessionID,
		"subagents",
		"jobs",
		"job-1",
		"agent-linked.jsonl",
	)
	writeSourceFile(
		t,
		filepath.Join(targetProject, sessionID+".jsonl"),
		claudeProviderFixture("from symlink"),
	)
	writeSourceFile(
		t,
		filepath.Join(targetProject, sessionID, "subagents", "jobs", "job-1", "agent-linked.jsonl"),
		claudeProviderFixture("from symlink subagent"),
	)
	if err := os.Symlink(targetProject, sourceProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{sourcePath, subagentPath}, sourceDisplayPaths(discovered))

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "agent-linked",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subagentPath, found.DisplayPath)
}

func TestClaudeProviderParse(t *testing.T) {
	root := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	sessionID := "session-main"
	sourcePath := filepath.Join(root, projectDir, sessionID+".jsonl")
	writeSourceFile(t, sourcePath, claudeProviderFixture("parse question"))

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.False(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, sessionID, result.Result.Session.ID)
	assert.Equal(t, AgentClaude, result.Result.Session.Agent)
	assert.Equal(t, "demo", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, sourcePath, result.Result.Session.File.Path)
	assert.Equal(t, "abc123", result.Result.Session.File.Hash)
	assert.Equal(t, "parse question", result.Result.Session.FirstMessage)
	assert.Len(t, result.Result.Messages, 2)
}

func TestClaudeProviderParseIncremental(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "inc.jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello world", tsEarly),
		testjsonl.ClaudeAssistantJSON("hi there", tsEarlyS1),
	)
	writeSourceFile(t, sourcePath, initial)
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	appended := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("follow up", tsEarlyS5),
		testjsonl.ClaudeAssistantJSON("got it", tsLate),
	)
	f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	currentInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "inc",
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  SourceFingerprint{Key: sourcePath, Size: currentInfo.Size()},
			SessionID:    "inc",
			Offset:       info.Size(),
			StartOrdinal: 2,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.Equal(t, "inc", outcome.SessionID)
	assert.Equal(t, int64(len(appended)), outcome.ConsumedBytes)
	require.Len(t, outcome.Messages, 2)
	assert.Equal(t, 2, outcome.Messages[0].Ordinal)
	assert.Equal(t, RoleUser, outcome.Messages[0].Role)
	assert.Contains(t, outcome.Messages[0].Content, "follow up")
	assert.Equal(t, 3, outcome.Messages[1].Ordinal)
	assert.Equal(t, RoleAssistant, outcome.Messages[1].Role)
	assert.Contains(t, outcome.Messages[1].Content, "got it")
}

func TestClaudeProviderParseIncrementalTruncatedNeedsFullParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "truncated.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "truncated",
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: SourceFingerprint{Key: sourcePath, Size: int64(len(initial) / 2)},
			SessionID:   "truncated",
			Offset:      int64(len(initial)),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
}

func TestClaudeProviderParseIncrementalEmptyTruncationNeedsFullParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "empty-truncated.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "empty-truncated",
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: SourceFingerprint{Key: sourcePath, Size: 0},
			SessionID:   "empty-truncated",
			Offset:      int64(len(initial)),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
}

func claudeProviderFixture(firstMessage string) string {
	return testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON(firstMessage, tsEarly),
		testjsonl.ClaudeAssistantJSON("Done.", tsEarlyS1),
	)
}
