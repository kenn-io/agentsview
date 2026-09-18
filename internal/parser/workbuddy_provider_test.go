package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkBuddyProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentWorkBuddy)
	require.True(ok)
	require.NotNil(factory)

	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(CapabilitySupported, caps.Source.WatchSources)
	assert.Equal(CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(CapabilitySupported, caps.Source.FindSource)
	assert.Equal(CapabilitySupported, caps.Source.CompositeFingerprint)
	assert.Equal(CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(CapabilitySupported, caps.Content.Cwd)
	assert.Equal(CapabilitySupported, caps.Content.Relationships)
	assert.Equal(CapabilitySupported, caps.Content.Subagents)
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(CapabilitySupported, caps.Content.PerMessageTokenUsage)
	assert.Equal(CapabilitySupported, caps.Content.Model)
	assert.Equal(CapabilitySupported, caps.Content.MalformedLineCount)

	provider, ok := NewProvider(AgentWorkBuddy, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestWorkBuddyProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 3)
	assert.Equal(
		[]string{sourcePath, nonIDSubagentPath, subagentPath},
		sourceDisplayPaths(discovered),
	)
	assert.Equal([]string{"proj", "proj", "proj"}, sourceProjects(discovered))

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal([]string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~workbuddy:" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.NotZero(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:" + subagentID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(subagentPath, found.DisplayPath)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:../agent-123",
	})
	require.NoError(err)
	assert.False(ok)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: sessionID + ":subagent:2025.01.01",
	})
	require.NoError(err)
	assert.False(ok)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: subagentPath,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(subagentPath, found.DisplayPath)

	require.NoError(os.Remove(subagentPath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: subagentPath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(subagentPath, changed[0].DisplayPath)
}

func TestWorkBuddyProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~workbuddy:" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestWorkBuddyProviderParseMainAndSubagent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)

	mainFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	mainOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: mainFingerprint,
	})
	require.NoError(err)
	require.True(mainOutcome.ResultSetComplete)
	require.Len(mainOutcome.Results, 1)
	mainResult := mainOutcome.Results[0]
	assert.Equal(DataVersionCurrent, mainResult.DataVersion)
	assert.Equal("workbuddy:"+sessionID, mainResult.Result.Session.ID)
	assert.Equal("devbox", mainResult.Result.Session.Machine)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(mainContent))),
		mainResult.Result.Session.File.Hash,
	)
	assert.Len(mainResult.Result.Messages, 3)
	assert.Equal("hello", mainResult.Result.Session.FirstMessage)
	assert.True(mainResult.Result.Session.HasTotalOutputTokens)

	subFingerprint, err := provider.Fingerprint(t.Context(), sources[1])
	require.NoError(err)
	subOutcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[1],
		Fingerprint: subFingerprint,
	})
	require.NoError(err)
	require.True(subOutcome.ResultSetComplete)
	require.Len(subOutcome.Results, 1)
	subResult := subOutcome.Results[0]
	assert.Equal(DataVersionCurrent, subResult.DataVersion)
	assert.Equal(
		"workbuddy:"+sessionID+":subagent:"+subagentID,
		subResult.Result.Session.ID,
	)
	assert.Equal("workbuddy:"+sessionID, subResult.Result.Session.ParentSessionID)
	assert.Equal(RelSubagent, subResult.Result.Session.RelationshipType)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(subContent))),
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
