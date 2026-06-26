package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkBuddyProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentWorkBuddy)
	require.True(t, ok)
	require.NotNil(t, factory)

	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(t, CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(t, CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(t, CapabilitySupported, caps.Source.FindSource)
	assert.Equal(t, CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.Cwd)
	assert.Equal(t, CapabilitySupported, caps.Content.Relationships)
	assert.Equal(t, CapabilitySupported, caps.Content.Subagents)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(t, CapabilitySupported, caps.Content.PerMessageTokenUsage)
	assert.Equal(t, CapabilitySupported, caps.Content.Model)
	assert.Equal(t, CapabilitySupported, caps.Content.MalformedLineCount)

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestWorkBuddyProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionID := "11111111-1111-4111-8111-111111111111"
	subagentID := "agent-123"
	projectDir := filepath.Join(root, "proj")
	sourcePath := filepath.Join(projectDir, sessionID+".jsonl")
	subagentPath := filepath.Join(
		projectDir, sessionID, "subagents", subagentID+".jsonl",
	)
	nonIDSubagentPath := filepath.Join(
		projectDir, sessionID, "subagents", "2025.01.01.jsonl",
	)
	writeSourceFile(t, sourcePath, workBuddyProviderFixture("hello"))
	writeSourceFile(t, subagentPath, workBuddyProviderFixture("sub task"))
	writeSourceFile(t, nonIDSubagentPath, workBuddyProviderFixture("dated sub task"))
	writeSourceFile(t, filepath.Join(projectDir, "2025.01.01.jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(projectDir, sessionID, "tool-results", "tool_123.txt"), "{}\n")
	writeSourceFile(t, filepath.Join(root, sessionID+".jsonl"), "{}\n")
	writeSourceFile(t, filepath.Join(projectDir, sessionID, "subagents", "nested", "deep.jsonl"), "{}\n")

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 3)
	assert.Equal(
		t,
		[]string{sourcePath, nonIDSubagentPath, subagentPath},
		sourceDisplayPaths(discovered),
	)
	assert.Equal(t, []string{"proj", "proj", "proj"}, sourceProjects(discovered))

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~workbuddy:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.NotZero(t, fingerprint.Size)
	assert.NotZero(t, fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:" + subagentID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subagentPath, found.DisplayPath)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:../agent-123",
	})
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:2025.01.01",
	})
	require.NoError(t, err)
	assert.False(t, ok)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: subagentPath,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, subagentPath, found.DisplayPath)

	require.NoError(t, os.Remove(subagentPath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: subagentPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, subagentPath, changed[0].DisplayPath)
}

func TestWorkBuddyProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	targetDir := t.TempDir()
	sessionID := "11111111-1111-4111-8111-111111111111"
	linkDir := filepath.Join(root, "proj")
	sourcePath := filepath.Join(linkDir, sessionID+".jsonl")
	writeSourceFile(t, filepath.Join(targetDir, sessionID+".jsonl"), workBuddyProviderFixture("hello"))
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~workbuddy:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestWorkBuddyProviderParseMainAndSubagent(t *testing.T) {
	root := t.TempDir()
	sessionID := "11111111-1111-4111-8111-111111111111"
	subagentID := "agent-123"
	sourcePath := filepath.Join(root, "proj", sessionID+".jsonl")
	subagentPath := filepath.Join(root, "proj", sessionID, "subagents", subagentID+".jsonl")
	mainContent := workBuddyProviderFixture("hello")
	subContent := workBuddyProviderFixture("sub task")
	writeSourceFile(t, sourcePath, mainContent)
	writeSourceFile(t, subagentPath, subContent)

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	mainFingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	mainOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: mainFingerprint,
	})
	require.NoError(t, err)
	require.True(t, mainOutcome.ResultSetComplete)
	require.Len(t, mainOutcome.Results, 1)
	mainResult := mainOutcome.Results[0]
	assert.Equal(t, DataVersionCurrent, mainResult.DataVersion)
	assert.Equal(t, "workbuddy:"+sessionID, mainResult.Result.Session.ID)
	assert.Equal(t, "devbox", mainResult.Result.Session.Machine)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(mainContent))),
		mainResult.Result.Session.File.Hash,
	)
	assert.Len(t, mainResult.Result.Messages, 3)
	assert.Equal(t, "hello", mainResult.Result.Session.FirstMessage)
	assert.True(t, mainResult.Result.Session.HasTotalOutputTokens)

	subFingerprint, err := provider.Fingerprint(context.Background(), sources[1])
	require.NoError(t, err)
	subOutcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[1],
		Fingerprint: subFingerprint,
	})
	require.NoError(t, err)
	require.True(t, subOutcome.ResultSetComplete)
	require.Len(t, subOutcome.Results, 1)
	subResult := subOutcome.Results[0]
	assert.Equal(t, DataVersionCurrent, subResult.DataVersion)
	assert.Equal(
		t,
		"workbuddy:"+sessionID+":subagent:"+subagentID,
		subResult.Result.Session.ID,
	)
	assert.Equal(t, "workbuddy:"+sessionID, subResult.Result.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, subResult.Result.Session.RelationshipType)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(subContent))),
		subResult.Result.Session.File.Hash,
	)
}

func workBuddyProviderFixture(firstMessage string) string {
	return fmt.Sprintf(
		`{"id":"u1","timestamp":1778749186168,"type":"message","role":"user","content":[{"type":"input_text","text":%q}],"cwd":"/tmp/cwd-project"}
{"id":"a1","timestamp":1778749187168,"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"providerData":{"model":"gpt-5.5","usage":{"inputTokens":20,"outputTokens":4,"cacheReadInputTokens":5}}}
{"id":"fc1","timestamp":1778749188168,"type":"function_call","name":"Bash","callId":"call_1","arguments":"{\"command\":\"pwd\"}"}
`, firstMessage)
}
