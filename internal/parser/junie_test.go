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
	"go.kenn.io/agentsview/internal/money"
)

func parseJunieProviderSession(
	t *testing.T, path, machine string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	root := filepath.Dir(filepath.Dir(path))
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.True(t, samePath(path, sources[0].DisplayPath))
	source := sources[0]
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint, Machine: machine,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	return &result.Session, result.Messages
}

func TestParseJunieSession(t *testing.T) {
	root := t.TempDir()
	sessionID := "session-260101-120000-abcd"
	sessionDir := filepath.Join(root, sessionID)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.jsonl"), []byte(
		`{"sessionId":"other","createdAt":1}`+"\n"+
			`{"sessionId":"`+sessionID+`","createdAt":1704067100000,"updatedAt":1704067109000,"projectDir":"/work/old","taskName":"Old title","status":"in_progress"}`+"\n"+
			`{"sessionId":"`+sessionID+`","createdAt":1704067200000,"updatedAt":1704067204500,"projectDir":"/work/demo","taskName":"Build the feature","status":"completed"}`+"\n",
	), 0o600))

	events := []byte(
		`{"kind":"UserPromptEvent","requestId":"req-1","prompt":"hidden context","presentablePrompt":"Implement it","askMode":false,"thinkMore":false,"timestampMs":1704067201000}` + "\n" +
			`{"kind":"UserPromptEvent","requestId":"req-dropped","prompt":"Drop me","askMode":false,"thinkMore":false,"timestampMs":1704067202000}` + "\n" +
			`{"kind":"UserMessagesDroppedFromHistory","userMessageIds":["req-dropped"],"timestampMs":1704067202500}` + "\n" +
			`{"kind":"UserPromptEvent","requestId":"req-restored","prompt":"Restore me","timestampMs":1704067202600}` + "\n" +
			`{"kind":"UserMessagesDroppedFromHistory","userMessageIds":["req-restored"],"timestampMs":1704067202700}` + "\n" +
			`{"kind":"UserMessagesCommittedToHistory","userMessageIds":["req-restored"],"timestampMs":1704067202800}` + "\n" +
			`{"kind":"UserPromptEvent","requestId":"req-failed","prompt":"Fail me","timestampMs":1704067202850}` + "\n" +
			`{"kind":"UserMessagesFailedInHistory","userMessageIds":["req-failed"],"timestampMs":1704067202900}` + "\n" +
			`{"kind":"SessionA2uxEvent","event":{"state":"IN_PROGRESS","agentEvent":{"kind":"MarkdownBlockUpdatedEvent","stepId":"step-1","text":"thinking aloud"}},"timestampMs":1704067203000}` + "\n" +
			`{"kind":"SessionA2uxEvent","event":{"state":"IN_PROGRESS","agentEvent":{"kind":"ResultBlockUpdatedEvent","stepId":"step-1","cancelled":false,"result":"draft","changes":[]}},"timestampMs":1704067204000}` + "\n" +
			`{"kind":"SessionA2uxEvent","event":{"state":"COMPLETED","agentEvent":{"kind":"ResultBlockUpdatedEvent","stepId":"step-1","cancelled":false,"result":"<!-- ANSWER -->Done","changes":[]}},"timestampMs":1704067205000}` + "\n" +
			`{"kind":"SessionA2uxEvent","taskId":"task-1","event":{"agentEvent":{"kind":"LlmResponseMetadataEvent","agent":"JUNIE","modelUsage":[{"model":"claude-sonnet-4-6","cost":0.00125,"inputTokens":100,"cacheInputTokens":20,"cacheCreateTokens":30,"outputTokens":40,"time":1}]}},"timestampMs":1704067205100}` + "\n" +
			// Summarization cost snapshots can occur during compaction; they must not terminate the message stream.
			`{"kind":"SessionCostTrajectorySnapshotEvent","snapshot":{"attributedGroups":[{"costPurpose":"SUMMARIZATION","callPurpose":"SUMMARIZATION"}]},"timestampMs":1704067205200}` + "\n" +
			`{"kind":"SystemMessageEvent","text":"Notice","details":"Details","level":"ERROR","symbol":"!","timestampMs":1704067205500}` + "\n" +
			`{"kind":"AgentTaskFailedEvent","timestampMs":1704067205600}` + "\n" +
			`{"kind":"UserAsyncResponseEvent","entries":[{"question":"Continue?","answer":"Yes"}],"timestampMs":1704067206000}` + "\n" +
			`{"kind":"SessionA2uxEvent","event":{"state":"COMPLETED","agentEvent":{"kind":"ResultBlockUpdatedEvent","stepId":"step-2","cancelled":false,"result":"Finished","changes":[]}},"timestampMs":1704067207000}` + "\n" +
			"{\n" + `{}` + "\n",
	)
	path := filepath.Join(sessionDir, "events.jsonl")
	require.NoError(t, os.WriteFile(path, events, 0o600))

	sess, messages := parseJunieProviderSession(t, path, "local")

	assert.Equal(t, "junie:"+sessionID, sess.ID)
	assert.Equal(t, sessionID, sess.SourceSessionID)
	assert.Equal(t, AgentJunie, sess.Agent)
	assert.Equal(t, "local", sess.Machine)
	assert.Equal(t, "demo", sess.Project)
	assert.Equal(t, "/work/demo", sess.Cwd)
	assert.Equal(t, "Implement it", sess.FirstMessage)
	assert.Equal(t, "Build the feature", sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, time.UnixMilli(1704067200000), sess.StartedAt)
	assert.Equal(t, time.UnixMilli(1704067207000), sess.EndedAt)
	assert.Equal(t, 7, sess.MessageCount)
	assert.Equal(t, 3, sess.UserMessageCount)
	assert.Equal(t, 1, sess.MalformedLines)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 40, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 150, sess.PeakContextTokens)
	require.Len(t, sess.UsageEvents, 1)
	usage := sess.UsageEvents[0]
	assert.Equal(t, "junie:"+sessionID, usage.SessionID)
	assert.Equal(t, "llm-response", usage.Source)
	assert.Equal(t, "claude-sonnet-4-6", usage.Model)
	assert.Equal(t, 100, usage.InputTokens)
	assert.Equal(t, 40, usage.OutputTokens)
	assert.Equal(t, 20, usage.CacheReadInputTokens)
	assert.Equal(t, 30, usage.CacheCreationInputTokens)
	require.NotNil(t, usage.Cost)
	assert.Equal(t, money.MustParseDollars("0.00125"), *usage.Cost)
	assert.Equal(t, "exact", usage.CostStatus)
	assert.Equal(t, "junie-model-usage", usage.CostSource)
	assert.Equal(t, "2024-01-01T00:00:05.1Z", usage.OccurredAt)
	assert.Equal(t, "junie:"+sessionID+":llm-response:12:0", usage.DedupKey)

	require.Len(t, messages, 7)
	assertMessage(t, messages[0], RoleUser, "Implement it")
	assertMessage(t, messages[1], RoleUser, "Restore me")
	assertMessage(t, messages[2], RoleAssistant, "Done")
	assertMessage(t, messages[3], RoleSystem, "Notice\n\nDetails")
	assertMessage(t, messages[4], RoleSystem, "Agent task failed")
	assert.True(t, messages[3].IsSystem)
	assert.True(t, messages[4].IsSystem)
	assertMessage(t, messages[5], RoleUser, "Continue?\nYes")
	assertMessage(t, messages[6], RoleAssistant, "Finished")
	for i, message := range messages {
		assert.Equal(t, i, message.Ordinal)
	}
	assert.Equal(t, time.UnixMilli(1704067205000), messages[2].Timestamp)
}

func TestParseJunieSessionRejectsInvalidReportedCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"kind":"SessionA2uxEvent","event":{"agentEvent":{"kind":"LlmResponseMetadataEvent","modelUsage":[{"model":"test","cost":-1}]}}}`+"\n",
	), 0o600))

	_, _, _, err := parseJunieSessionWithSummary(
		t.Context(), path, "session-one", junieSessionSummary{}, false, (&junieIndexCache{}).openRoot,
	)
	require.ErrorContains(t, err, "invalid model usage cost")
}

func TestJunieSourceSetDiscoversOnlyEventStreams(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "session-one", "events.jsonl"),
		filepath.Join(root, "session-two", "events.jsonl"),
		filepath.Join(root, "session-one", "state.json"),
		filepath.Join(root, "session-two", "transcript.md"),
		filepath.Join(root, "index.jsonl"),
		filepath.Join(root, "nested", "session-three", "events.jsonl"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	indexPath := filepath.Join(root, "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","projectDir":"/old-one"}`+"\n"+
			`{"sessionId":"session-two","projectDir":"/old-two"}`+"\n",
	), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Content.AggregateUsageEvents)
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Source.ForceReplaceOnParse)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, AgentJunie, sources[0].Provider)
	assert.Equal(t, "session-one", sources[0].ProjectHint)
	assert.Equal(t, filepath.Join(root, "session-one", "events.jsonl"), sources[0].DisplayPath)
	assert.Equal(t, "session-two", sources[1].ProjectHint)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	unchangedFingerprint, err := provider.Fingerprint(t.Context(), sources[1])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","projectDir":"/new-one"}`+"\n"+
			`{"sessionId":"session-two","projectDir":"/old-two"}`+"\n",
	), 0o600))

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      indexPath,
		WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sources[0].Key, changed[0].Key)

	updatedFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint.Hash, updatedFingerprint.Hash)
	stillUnchangedFingerprint, err := provider.Fingerprint(t.Context(), sources[1])
	require.NoError(t, err)
	assert.Equal(t, unchangedFingerprint, stillUnchangedFingerprint)

	require.NoError(t, os.Remove(indexPath))
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      indexPath,
		WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1, "a missing index must not create destructive removal work")
	assert.Equal(t, "session-one", changed[0].ProjectHint)
}

func TestJunieSourceSetReusesIndexSnapshotWhileParsing(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-one")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","prompt":"Hello","timestampMs":1704067201000}`+"\n",
	), 0o600))
	indexPath := filepath.Join(root, "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","taskName":"Cached title"}`+"\n",
	), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	require.NoError(t, os.Remove(indexPath))

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "Cached title", outcome.Results[0].Result.Session.SessionName)
	assert.Equal(t, fingerprint.Size, outcome.Results[0].Result.Session.File.Size)
	assert.Equal(t, fingerprint.MTimeNS, outcome.Results[0].Result.Session.File.Mtime)
	assert.Equal(t, fingerprint.Hash, outcome.Results[0].Result.Session.File.Hash)
}

func TestJunieIndexChangeWorkIsBoundedByChangedSessions(t *testing.T) {
	for _, sessionCount := range []int{2, 128} {
		t.Run(fmt.Sprintf("sessions-%d", sessionCount), func(t *testing.T) {
			root := t.TempDir()
			changedID := fmt.Sprintf("session-%03d", sessionCount/2)
			var before, after strings.Builder
			for i := range sessionCount {
				sessionID := fmt.Sprintf("session-%03d", i)
				path := filepath.Join(root, sessionID, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
				fmt.Fprintf(&before, `{"sessionId":"%s","taskName":"old"}`+"\n", sessionID)
				title := "old"
				if sessionID == changedID {
					title = "new"
				}
				fmt.Fprintf(&after, `{"sessionId":"%s","taskName":"%s"}`+"\n", sessionID, title)
			}

			indexPath := filepath.Join(root, "index.jsonl")
			require.NoError(t, os.WriteFile(indexPath, []byte(before.String()), 0o600))
			def, ok := AgentByType(AgentJunie)
			require.True(t, ok)
			factory := newJunieProviderFactory(def)
			cfg := ProviderConfig{Roots: []string{root}, Machine: "local"}
			provider := factory.NewProvider(cfg)
			_, err := provider.WatchPlan(t.Context())
			require.NoError(t, err)
			discovered, err := provider.Discover(t.Context())
			require.NoError(t, err)
			var changedSource *SourceRef
			for i := range discovered {
				if discovered[i].ProjectHint == changedID {
					changedSource = &discovered[i]
					break
				}
			}
			require.NotNil(t, changedSource)
			beforeFingerprint, err := provider.Fingerprint(t.Context(), *changedSource)
			require.NoError(t, err)
			eventInfo, err := os.Stat(changedSource.DisplayPath)
			require.NoError(t, err)
			assert.Equal(t, eventInfo.Size(), beforeFingerprint.Size,
				"metadata must affect the hash without inflating the transcript size")
			statusOnly := strings.ReplaceAll(before.String(), "}\n", `,"status":"RUNNING"}`+"\n")
			require.NoError(t, os.WriteFile(indexPath, []byte(statusOnly), 0o600))
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      indexPath,
				WatchRoot: root,
			})
			require.NoError(t, err)
			assert.Empty(t, changed, "unused index fields must not invalidate a session")
			require.NoError(t, os.WriteFile(indexPath, []byte(after.String()), 0o600))
			provider = factory.NewProvider(cfg)
			_, err = provider.WatchPlan(t.Context())
			require.NoError(t, err)

			changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      indexPath,
				WatchRoot: root,
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, changedID, changed[0].ProjectHint)
			afterFingerprint, err := provider.Fingerprint(t.Context(), changed[0])
			require.NoError(t, err)
			assert.Equal(t, beforeFingerprint.Size, afterFingerprint.Size)
			assert.NotEqual(t, beforeFingerprint.Hash, afterFingerprint.Hash)

			retry, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				Path:      indexPath,
				WatchRoot: root,
			})
			require.NoError(t, err)
			require.Len(t, retry, 1, "repeated planning must preserve work until persistence can retry")
			assert.Equal(t, changedID, retry[0].ProjectHint)
		})
	}
}

func TestParseJunieTitleOnlySessionWithoutIndex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-fallback", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"kind":"SessionTitleSetEvent","name":"Fallback title","timestampMs":1704067200000}`+"\n",
	), 0o600))

	sess, messages := parseJunieProviderSession(t, path, "local")
	assert.Equal(t, "junie", sess.Project)
	assert.Empty(t, sess.Cwd)
	assert.Equal(t, "Fallback title", sess.SessionName)
	assert.Equal(t, "Fallback title", sess.FirstMessage)
	assert.Equal(t, time.UnixMilli(1704067200000), sess.StartedAt)
	assert.Equal(t, time.UnixMilli(1704067200000), sess.EndedAt)
	assert.Empty(t, messages)
}

func TestParseJunieSessionRejectsSymlinkedIndex(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "outside-index.jsonl")
	require.NoError(t, os.WriteFile(target, []byte(
		`{"sessionId":"session-safe","projectDir":"/private/secret","taskName":"Secret"}`+"\n",
	), 0o600))
	if err := os.Symlink(target, filepath.Join(root, "index.jsonl")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	path := filepath.Join(root, "session-safe", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"kind":"UserResponseEvent","prompt":"Safe","timestampMs":1704067201000}`+"\n",
	), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.ErrorContains(t, err, "is not a regular file")
}

func TestJunieIndexChangeIgnoresSymlinkedSessionDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","requestId":"outside","prompt":"Outside"}`+"\n",
	), 0o600))
	if err := os.Symlink(outside, filepath.Join(root, "session-outside")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	indexPath := filepath.Join(root, "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, nil, 0o600))
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-outside","taskName":"Outside"}`+"\n",
	), 0o600))
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      indexPath,
		WatchRoot: root,
	})
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestJunieSessionDirectorySwapCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-safe")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","requestId":"safe","prompt":"Safe"}`+"\n",
	), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","requestId":"outside","prompt":"Outside"}`+"\n",
	), 0o600))
	require.NoError(t, os.RemoveAll(sessionDir))
	if err := os.Symlink(outside, sessionDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err = provider.Fingerprint(t.Context(), sources[0])
	require.ErrorContains(t, err, "session directory is not a directory")
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "local",
	})
	require.ErrorContains(t, err, "session directory is not a directory")
}

func TestJunieConfiguredRootSwapCannotEscapeRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "configured")
	sessionDir := filepath.Join(root, "session-safe")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","requestId":"safe","prompt":"Safe"}`+"\n",
	), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)

	outside := filepath.Join(parent, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "session-safe"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "session-safe", "events.jsonl"), []byte(
		`{"kind":"UserPromptEvent","requestId":"outside","prompt":"Outside"}`+"\n",
	), 0o600))
	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err = provider.Fingerprint(t.Context(), sources[0])
	require.ErrorContains(t, err, "junie root is not a directory")
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "local",
	})
	require.ErrorContains(t, err, "junie root is not a directory")

	freshProvider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err = freshProvider.Discover(t.Context())
	require.ErrorContains(t, err, "junie root is not a directory")
}

func TestJunieConfiguredRootIdentityIsPinned(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "configured")
	sessionDir := filepath.Join(root, "session-safe")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte("{}\n"), 0o600))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "session-safe"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "session-safe", "events.jsonl"), []byte("{}\n"), 0o600))

	_, err = provider.Fingerprint(t.Context(), sources[0])
	require.ErrorContains(t, err, "junie root identity changed")
}

func TestJunieDiscoveryRepinsRecreatedConfiguredRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "configured")
	writeSession := func(prompt string) {
		t.Helper()
		path := filepath.Join(root, "session-safe", "events.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(
			`{"kind":"UserPromptEvent","requestId":"request","prompt":%q}`+"\n", prompt,
		)), 0o600))
	}
	writeSession("Before")
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	_, err = provider.Discover(t.Context())
	require.ErrorContains(t, err, "temporarily unavailable")
	writeSession("After")
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "local",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "After", outcome.Results[0].Result.Session.FirstMessage)
}

func TestJunieRecreatedRootDiscardsPreviousIndexMetadata(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "configured")
	sessionDir := filepath.Join(root, "session-safe")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "events.jsonl"),
		[]byte(`{"kind":"UserPromptEvent","requestId":"request","prompt":"Before"}`+"\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "index.jsonl"),
		[]byte(`{"sessionId":"session-safe","taskName":"Stale title"}`+"\n"),
		0o600,
	))

	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	// Replace the configured root at the same path with an index-free store.
	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "events.jsonl"),
		[]byte(`{"kind":"UserPromptEvent","requestId":"request","prompt":"After"}`+"\n"),
		0o600,
	))

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "local",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Empty(t, outcome.Results[0].Result.Session.SessionName)
	assert.Equal(t, "After", outcome.Results[0].Result.Session.FirstMessage)
}

func TestJunieFindSourceRefreshesIndexSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-one", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"kind":"UserPromptEvent","requestId":"request","prompt":"Prompt"}`+"\n",
	), 0o600))
	indexPath := filepath.Join(root, "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","taskName":"Before"}`+"\n",
	), 0o600))
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","taskName":"After"}`+"\n",
	), 0o600))
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: path})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint, Machine: "local",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "After", outcome.Results[0].Result.Session.SessionName)
}

func TestJunieIncompleteIndexDoesNotReplaceCachedSnapshot(t *testing.T) {
	root := t.TempDir()
	for _, sessionID := range []string{"session-a", "session-b"} {
		path := filepath.Join(root, sessionID, "events.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	indexPath := filepath.Join(root, "index.jsonl")
	before := `{"sessionId":"session-a","taskName":"Before"}` + "\n" +
		`{"sessionId":"session-b","taskName":"Stable"}` + "\n"
	require.NoError(t, os.WriteFile(indexPath, []byte(before), 0o600))
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-a","taskName":"After"}`+"\n"+"{\n",
	), 0o600))
	_, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.ErrorContains(t, err, "invalid JSON")
	require.NoError(t, os.Remove(indexPath))
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.NoError(t, err)
	assert.Empty(t, changed, "a missing shared index must not erase the last complete snapshot")

	after := `{"sessionId":"session-a","taskName":"After"}` + "\n" +
		`{"sessionId":"session-b","taskName":"Stable"}` + "\n"
	require.NoError(t, os.WriteFile(indexPath, []byte(after), 0o600))
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, "session-a", changed[0].ProjectHint)
}

func TestJunieIndexRetriesAccumulateUntilAcknowledged(t *testing.T) {
	root := t.TempDir()
	for _, sessionID := range []string{"session-a", "session-b"} {
		path := filepath.Join(root, sessionID, "events.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	indexPath := filepath.Join(root, "index.jsonl")
	writeIndex := func(a, b string) {
		t.Helper()
		require.NoError(t, os.WriteFile(indexPath, []byte(
			fmt.Sprintf(`{"sessionId":"session-a","taskName":%q}`+"\n"+`{"sessionId":"session-b","taskName":%q}`+"\n", a, b),
		), 0o600))
	}
	writeIndex("Before", "Before")
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	writeIndex("After", "Before")
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	writeIndex("After", "After")
	changed, err = provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.NoError(t, err)
	require.Len(t, changed, 2)
	assert.Equal(t, []string{"session-a", "session-b"}, []string{changed[0].ProjectHint, changed[1].ProjectHint})

	acknowledger := provider.(SourceSyncAcknowledger)
	acknowledger.AcknowledgeSourceSync(changed[0])
	retry, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: indexPath})
	require.NoError(t, err)
	require.Len(t, retry, 1)
	assert.Equal(t, "session-b", retry[0].ProjectHint)
}

func TestJunieUnavailableIndexSessionDoesNotStayPending(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index.jsonl")
	provider, ok := NewProvider(AgentJunie, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	// The index now lists a session whose transcript does not exist, so nothing
	// can produce or acknowledge a source for it.
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-gone","taskName":"Gone"}`+"\n",
	), 0o600))

	req := ChangedPathRequest{Path: indexPath, WatchRoot: root}
	relevance, err := ResolveChangedPathRelevance(t.Context(), provider, req)
	require.NoError(t, err)
	require.Equal(t, ChangedPathDataBearing, relevance)

	sources, err := provider.SourcesForChangedPath(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, sources)

	// The unavailable session must not stay pending, so an unchanged index is
	// classified as non-data instead of driving archive-wide fallback forever.
	relevance, err = ResolveChangedPathRelevance(t.Context(), provider, req)
	require.NoError(t, err)
	assert.Equal(t, ChangedPathNonData, relevance)
}

func TestOpenJuniePinnedFileRejectsReplacement(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "events.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	expected, err := os.Lstat(path)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, filepath.Join(rootPath, "old-events.jsonl")))
	require.NoError(t, os.WriteFile(path, []byte("after"), 0o600))
	root, err := os.OpenRoot(rootPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	_, err = openJuniePinnedFile(root, "events.jsonl", expected)
	require.ErrorContains(t, err, "changed while opening")
}
