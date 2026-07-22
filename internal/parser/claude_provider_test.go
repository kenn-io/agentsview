package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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

// A followed project-directory symlink whose target cannot be resolved must
// surface incomplete streaming discovery rather than reading as absent:
// reconciliation treats a clean DiscoverEach as authoritative and would
// tombstone every session beneath the symlink.
func TestClaudeProviderStreamingDiscoveryPropagatesProjectSymlinkErrors(t *testing.T) {
	discoverEach := func(t *testing.T, root string) ([]string, error) {
		t.Helper()
		provider, ok := NewProvider(AgentClaude, ProviderConfig{
			Roots: []string{root},
		})
		require.True(t, ok)
		discoverer, ok := provider.(StreamingDiscoverer)
		require.True(t, ok)
		var yielded []string
		err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
			yielded = append(yielded, source.DisplayPath)
			return nil
		})
		return yielded, err
	}
	healthyPath := func(root string) string {
		return filepath.Join(root, "-Users-dev-code-demo", "session-main.jsonl")
	}

	t.Run("dangling project symlink", func(t *testing.T) {
		root := t.TempDir()
		writeSourceFile(t, healthyPath(root), claudeProviderFixture("hello claude"))
		target := filepath.Join(t.TempDir(), "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.RemoveAll(target))

		yielded, err := discoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		// The walker records the failure and continues with healthy siblings.
		assert.Equal(t, []string{healthyPath(root)}, yielded)

		require.NoError(t, os.Remove(link))
		yielded, err = discoverEach(t, root)
		require.NoError(t, err)
		assert.Equal(t, []string{healthyPath(root)}, yielded)
	})

	t.Run("unstatable project symlink target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory read permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		root := t.TempDir()
		writeSourceFile(t, healthyPath(root), claudeProviderFixture("hello claude"))
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.Chmod(targetParent, 0o000))
		t.Cleanup(func() { _ = os.Chmod(targetParent, 0o755) })

		yielded, err := discoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrPermission)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{healthyPath(root)}, yielded)

		require.NoError(t, os.Chmod(targetParent, 0o755))
		yielded, err = discoverEach(t, root)
		require.NoError(t, err)
		assert.Equal(t, []string{healthyPath(root)}, yielded)
	})
}

func TestClaudeProviderStreamingDiscoveryStopsAfterYieldError(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"-Users-dev-code-one", "-Users-dev-code-two"} {
		writeSourceFile(
			t, filepath.Join(root, project, "session.jsonl"),
			claudeProviderFixture("streamed"),
		)
	}
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)

	stop := errors.New("stop discovery")
	calls := 0
	err := discoverer.DiscoverEach(t.Context(), func(SourceRef) error {
		calls++
		return stop
	})
	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, calls)
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

func TestClaudeProviderParseIncrementalPreservesLinkWithoutMessage(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "inc-link-only.jsonl")
	initial := claudeProviderFixture("hello world")
	writeSourceFile(t, sourcePath, initial)

	appended := `{"type":"user","isMeta":true,"timestamp":"2024-01-01T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_link_only","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"linkonly"}}` + "\n"
	f, err := os.OpenFile(sourcePath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	provider, ok := NewProvider(AgentClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "inc-link-only",
	})
	require.NoError(t, err)
	require.True(t, ok)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source: source,
			Fingerprint: SourceFingerprint{
				Key:  sourcePath,
				Size: int64(len(initial) + len(appended)),
			},
			SessionID:    "inc-link-only",
			Offset:       int64(len(initial)),
			StartOrdinal: 2,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.Empty(t, outcome.Messages)
	assert.Equal(t, int64(len(appended)), outcome.ConsumedBytes)
	assert.Equal(t, []ClaudeSubagentLink{{
		ToolUseID:         "toolu_link_only",
		SubagentSessionID: "agent-linkonly",
	}}, outcome.SubagentLinks)
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
