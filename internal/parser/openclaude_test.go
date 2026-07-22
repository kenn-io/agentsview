package parser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenClaudeProviderCapabilities(t *testing.T) {
	caps := openClaudeProviderCapabilities()
	require.True(t, caps.Source.DiscoverSources == CapabilitySupported)
	require.True(t, caps.Source.WatchSources == CapabilitySupported)
	require.True(t, caps.Source.ClassifyChangedPath == CapabilitySupported)
	require.True(t, caps.Source.FindSource == CapabilitySupported)
	require.True(t, caps.Source.ForceReplaceOnParse == CapabilitySupported)

	def, ok := AgentByType(AgentOpenClaude)
	require.True(t, ok, "AgentOpenClaude missing from Registry")
	assert.True(t, def.FileBased)
	assert.Equal(t, "OPENCLAUDE_PROJECTS_DIR", def.EnvVar)
	assert.Equal(t, "OPENCLAUDE_CONFIG_DIR", def.DefaultRootEnvVar)
	assert.Equal(t, "openclaude_project_dirs", def.ConfigKey)
	assert.Equal(t, []string{".openclaude/projects"}, def.DefaultDirs)
	assert.Equal(t, "openclaude:", def.IDPrefix)
	assert.Empty(t, def.WatchSubdirs)
	assert.Nil(t, def.WatchRootsFunc)
}

func TestOpenClaudeDiscoverParseAndFindSource(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "my-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-123.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type":       "user",
			"timestamp":  tsEarly,
			"uuid":       "u1",
			"parentUuid": "",
			"cwd":        "/workspace/my-project",
			"gitBranch":  "main",
			"message": map[string]any{
				"role":    "user",
				"content": "hello from openclaude",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":       "assistant",
			"timestamp":  tsEarlyS1,
			"uuid":       "u2",
			"parentUuid": "u1",
			"message": map[string]any{
				"role":        "assistant",
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":                12,
					"cache_creation_input_tokens": 3,
					"cache_read_input_tokens":     2,
					"output_tokens":               7,
				},
				"content": []map[string]any{
					{"type": "text", "text": "reply"},
				},
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "ai-title",
			"timestamp": tsEarlyS1,
			"aiTitle":   "AI title",
		}),
		buildMetadataLine(map[string]any{
			"type":        "custom-title",
			"timestamp":   tsEarlyS5,
			"customTitle": "User title",
		}),
		buildMetadataLine(map[string]any{
			"type":       "system",
			"subtype":    "compact_boundary",
			"timestamp":  tsEarlyS5,
			"uuid":       "u3",
			"parentUuid": "u2",
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "Compact summary"},
				},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, path, discovered[0].Key)
	assert.Equal(t, "my-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "openclaude:session-123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.Key)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: SourceFingerprint{Hash: "hash-123"},
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal(t, AgentOpenClaude, result.Session.Agent)
	assert.Equal(t, "openclaude:session-123", result.Session.ID)
	assert.Equal(t, "my_project", result.Session.Project)
	assert.Equal(t, "hello from openclaude", result.Session.FirstMessage)
	assert.Equal(t, "User title", result.Session.SessionName)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	assert.Equal(t, TerminationAwaitingUser, result.Session.TerminationStatus)
	assert.Equal(t, 7, result.Session.TotalOutputTokens)
	assert.Equal(t, 17, result.Session.PeakContextTokens)
	assert.True(t, result.Session.HasTotalOutputTokens)
	assert.True(t, result.Session.HasPeakContextTokens)
	assert.Equal(t, "hash-123", result.Session.File.Hash)
	require.Len(t, result.Messages, 3)

	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.Equal(t, "end_turn", result.Messages[1].StopReason)
	assert.Equal(t, 7, result.Messages[1].OutputTokens)
	assert.Equal(t, 17, result.Messages[1].ContextTokens)
	assert.True(t, result.Messages[1].HasOutputTokens)
	assert.True(t, result.Messages[1].HasContextTokens)
	assert.Equal(t, RoleAssistant, result.Messages[2].Role)
	assert.True(t, result.Messages[2].IsSystem)
	assert.True(t, result.Messages[2].IsCompactBoundary)
	assert.Equal(t, "compact_boundary", result.Messages[2].SourceSubtype)
	assert.Contains(t, result.Messages[2].Content, "Compact summary")
}

func TestOpenClaudeTerminationUsesSystemToolResults(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "tool-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-tools.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type": "user", "timestamp": tsEarly, "uuid": "u1",
			"message": map[string]any{"role": "user", "content": "read file"},
		}),
		buildMetadataLine(map[string]any{
			"type": "assistant", "timestamp": tsEarlyS1, "uuid": "a1",
			"message": map[string]any{
				"role": "assistant", "stop_reason": "tool_use",
				"content": []map[string]any{{
					"type": "tool_use", "id": "toolu_sys", "name": "Read", "input": map[string]any{},
				}},
			},
		}),
		buildMetadataLine(map[string]any{
			"type": "system", "subtype": "tool_result", "timestamp": tsEarlyS5, "uuid": "s1",
			"message": map[string]any{
				"role": "system",
				"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": "toolu_sys", "content": "file contents",
				}},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result

	assert.Equal(t, TerminationClean, result.Session.TerminationStatus)
	require.Len(t, result.Messages, 3)
	assert.True(t, result.Messages[2].IsSystem)
	require.Len(t, result.Messages[2].ToolResults, 1)
	assert.Equal(t, "toolu_sys", result.Messages[2].ToolResults[0].ToolUseID)
}

func TestOpenClaudeTerminationCompactBoundaryDoesNotResolveToolCall(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "compact-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-compact.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type": "user", "timestamp": tsEarly, "uuid": "u1",
			"message": map[string]any{"role": "user", "content": "read file"},
		}),
		buildMetadataLine(map[string]any{
			"type": "assistant", "timestamp": tsEarlyS1, "uuid": "a1",
			"message": map[string]any{
				"role": "assistant", "stop_reason": "tool_use",
				"content": []map[string]any{{
					"type": "tool_use", "id": "toolu_boundary", "name": "Read", "input": map[string]any{},
				}},
			},
		}),
		buildMetadataLine(map[string]any{
			"type": "system", "subtype": "compact_boundary", "timestamp": tsEarlyS5, "uuid": "s1",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "text", "text": "Compacted earlier turns",
				}},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result

	assert.Equal(t, TerminationToolCallPending, result.Session.TerminationStatus)
	require.Len(t, result.Messages, 3)
	assert.True(t, result.Messages[2].IsSystem)
	assert.True(t, result.Messages[2].IsCompactBoundary)
	assert.Equal(t, RoleAssistant, result.Messages[2].Role)
}

func TestOpenClaudeQueuedCommandAttachment(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "queue-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-queue.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type":      "user",
			"timestamp": tsEarly,
			"uuid":      "u1",
			"message": map[string]any{
				"role":    "user",
				"content": "first prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "attachment",
			"timestamp": tsEarlyS1,
			"attachment": map[string]any{
				"type":        "queued_command",
				"commandMode": "prompt",
				"prompt": []map[string]any{
					{"type": "text", "text": "/resume next step"},
					{"type": "text", "text": "with context"},
				},
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "attachment",
			"timestamp": "2024-01-01T10:00:01.250Z",
			"attachment": map[string]any{
				"type":        "queued_command",
				"commandMode": "prompt",
				"prompt":      "<system-reminder>remember this</system-reminder>\n\nactual prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "attachment",
			"timestamp": "2024-01-01T10:00:01.500Z",
			"attachment": map[string]any{
				"type":        "queued_command",
				"commandMode": "task-notification",
				"prompt":      "ignored non-prompt queued command",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "attachment",
			"timestamp": "2024-01-01T10:00:01.750Z",
			"attachment": map[string]any{
				"type":        "queued_command",
				"commandMode": "prompt",
				"isMeta":      true,
				"prompt":      "ignored meta queued command",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "attachment",
			"timestamp": "2024-01-01T10:00:02Z",
			"attachment": map[string]any{
				"type":        "queued_command",
				"commandMode": "prompt",
				"origin":      "task-notification",
				"prompt":      "ignored queued prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":       "assistant",
			"timestamp":  tsEarlyS5,
			"uuid":       "u2",
			"parentUuid": "u1",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "reply"},
				},
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	require.Len(t, result.Messages, 4)
	assert.Equal(t, "/resume next step\nwith context", result.Messages[1].Content)
	assert.Equal(t, "queued_command", result.Messages[1].SourceSubtype)
	assert.Equal(t, RoleUser, result.Messages[1].Role)
	assert.Equal(t, "<system-reminder>remember this</system-reminder>\n\nactual prompt", result.Messages[2].Content)
	assert.Equal(t, "queued_command", result.Messages[2].SourceSubtype)
	assert.False(t, result.Messages[2].IsSystem)
	assert.Equal(t, 3, result.Session.UserMessageCount)
	assert.Equal(t, "first prompt", result.Session.FirstMessage)
}

func TestOpenClaudeSkipsMetaUserMessages(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "meta-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, "session-meta.jsonl")
	content := strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type":      "user",
			"timestamp": tsEarly,
			"isMeta":    true,
			"uuid":      "m1",
			"message": map[string]any{
				"role":    "user",
				"content": "hidden meta prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "user",
			"timestamp": tsEarlyS1,
			"uuid":      "u1",
			"message": map[string]any{
				"role":    "user",
				"content": "real prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "assistant",
			"timestamp": "2024-01-01T10:00:02Z",
			"uuid":      "a1",
			"message": map[string]any{
				"role":        "assistant",
				"stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "text", "text": "real reply"},
				},
			},
		}),
		buildMetadataLine(map[string]any{
			"type":      "user",
			"timestamp": "2024-01-01T10:00:03Z",
			"isMeta":    true,
			"uuid":      "m2",
			"message": map[string]any{
				"role":    "user",
				"content": "hidden trailing meta",
			},
		}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "real prompt", result.Session.FirstMessage)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	assert.Equal(t, TerminationAwaitingUser, result.Session.TerminationStatus)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "real prompt", result.Messages[0].Content)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
}

func TestOpenClaudeDiscoverParseSubagentRelationship(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(
		root,
		"proj-name",
		"parent-123",
		"subagents",
		"tasks",
		"agent-worker.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		buildMetadataLine(map[string]any{
			"type":      "user",
			"timestamp": tsEarly,
			"uuid":      "u1",
			"message": map[string]any{
				"role":    "user",
				"content": "subagent prompt",
			},
		}),
		buildMetadataLine(map[string]any{
			"type":       "assistant",
			"timestamp":  tsEarlyS1,
			"uuid":       "u2",
			"parentUuid": "u1",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "subagent reply"},
				},
			},
		}),
	}, "\n")+"\n"), 0o644))

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  discovered[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	result := outcome.Results[0].Result
	assert.Equal(t, "openclaude:agent-worker", result.Session.ID)
	assert.Equal(t, "openclaude:parent-123", result.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, result.Session.RelationshipType)
}

func openClaudeDiscoverEach(t *testing.T, root string) ([]string, error) {
	t.Helper()
	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
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

// Discover follows symlinked project directories (ClaudeProjectSessionFiles
// resolves entries through isDirOrSymlink), so streaming discovery must too:
// reconciliation treats a clean DiscoverEach as authoritative, and skipping a
// symlinked project would tombstone every session beneath it.
func TestOpenClaudeDiscoverEachFollowsSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	projectDir := "-Users-dev-code-demo"
	targetProject := filepath.Join(targetRoot, projectDir)
	linkedProject := filepath.Join(root, projectDir)
	linkedPath := filepath.Join(linkedProject, "session-linked.jsonl")
	regularPath := filepath.Join(
		root, "regular-project", "session-regular.jsonl",
	)
	writeSourceFile(
		t,
		filepath.Join(targetProject, "session-linked.jsonl"),
		claudeProviderFixture("from symlink"),
	)
	writeSourceFile(t, regularPath, claudeProviderFixture("regular"))
	if err := os.Symlink(targetProject, linkedProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentOpenClaude, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(
		t, []string{linkedPath, regularPath}, sourceDisplayPaths(discovered),
	)

	streamed, err := openClaudeDiscoverEach(t, root)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{linkedPath, regularPath}, streamed,
		"DiscoverEach must find the same sessions as Discover "+
			"for symlinked project directories")
}

// A followed project-directory symlink whose target cannot be resolved must
// surface incomplete streaming discovery rather than reading as absent:
// reconciliation treats a clean DiscoverEach as authoritative and would
// tombstone every session beneath the symlink.
func TestOpenClaudeStreamingDiscoveryPropagatesProjectSymlinkErrors(
	t *testing.T,
) {
	healthyPath := func(root string) string {
		return filepath.Join(root, "-Users-dev-code-demo", "session-main.jsonl")
	}

	t.Run("dangling project symlink", func(t *testing.T) {
		root := t.TempDir()
		writeSourceFile(
			t, healthyPath(root), claudeProviderFixture("hello openclaude"),
		)
		target := filepath.Join(t.TempDir(), "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "linked")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.RemoveAll(target))

		yielded, err := openClaudeDiscoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		// The walker records the failure and continues with healthy siblings.
		assert.Equal(t, []string{healthyPath(root)}, yielded)

		require.NoError(t, os.Remove(link))
		yielded, err = openClaudeDiscoverEach(t, root)
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
		writeSourceFile(
			t, healthyPath(root), claudeProviderFixture("hello openclaude"),
		)
		targetParent := t.TempDir()
		target := filepath.Join(targetParent, "linked-project")
		require.NoError(t, os.MkdirAll(target, 0o755))
		if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.NoError(t, os.Chmod(targetParent, 0o000))
		t.Cleanup(func() { _ = os.Chmod(targetParent, 0o755) })

		yielded, err := openClaudeDiscoverEach(t, root)

		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrPermission)
		var incomplete DiscoveryIncompleteError
		assert.ErrorAs(t, err, &incomplete)
		assert.Equal(t, []string{healthyPath(root)}, yielded)

		require.NoError(t, os.Chmod(targetParent, 0o755))
		yielded, err = openClaudeDiscoverEach(t, root)
		require.NoError(t, err)
		assert.Equal(t, []string{healthyPath(root)}, yielded)
	})
}
