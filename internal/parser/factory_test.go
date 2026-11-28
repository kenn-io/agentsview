package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const factoryFixtureID = "0f9c1c2e-6f7d-4b3a-9c2e-1a2b3c4d5e6f"

func factoryFixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "factory"))
	require.NoError(t, err)
	return root
}

func TestDiscoverFactorySessions(t *testing.T) {
	root := factoryFixtureRoot(t)
	provider, ok := NewProvider(AgentFactory, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "settings.json sidecar must not be discovered as a session")
	assert.Equal(t, AgentFactory, sources[0].Provider)
	assert.Equal(t, "-home-dev-workspace-demo-app", sources[0].ProjectHint)
	assert.Equal(t, factoryFixtureID+".jsonl", filepath.Base(sources[0].DisplayPath))

	found, ok, err := provider.FindSource(
		t.Context(), FindSourceRequest{RawSessionID: factoryFixtureID},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sources[0].DisplayPath, found.DisplayPath)
}

func TestParseFactorySession(t *testing.T) {
	path := filepath.Join(
		factoryFixtureRoot(t), "-home-dev-workspace-demo-app", factoryFixtureID+".jsonl",
	)
	results, err := parseFactorySession(t.Context(), path, "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1)
	sess := results[0].Session
	msgs := results[0].Messages

	assert.Equal(t, "factory:"+factoryFixtureID, sess.ID)
	assert.Equal(t, AgentFactory, sess.Agent)
	assert.Equal(t, "demo_app", sess.Project, "project comes from session_start.cwd")
	assert.Equal(t, "/home/dev/workspace/demo-app", sess.Cwd)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, "add retry to fetch helper", sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, "Add exponential backoff to fetchWithRetry in src/http.ts", sess.FirstMessage)
	assert.Equal(t, 1, sess.MalformedLines)
	assert.Equal(t, "2026-07-31T21:03:29Z", sess.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
	assert.Equal(t, "2026-07-31T21:04:00Z", sess.EndedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"session_end timestamp extends the session bounds")

	// llm_only system-reminder and the empty hook row are dropped; 2 human +
	// 2 tool_result users, 3 assistants, and 1 user_only UI notice.
	require.Len(t, msgs, 8)
	assert.Equal(t, 2, sess.UserMessageCount, "UI notices are not typed prompts")
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.False(t, msgs[0].IsSystem)
	assert.Equal(t, "m1", msgs[0].SourceUUID)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "Need to read the file first.", msgs[1].ThinkingText)
	require.NotNil(t, msgs[1].DialogueText)
	assert.Equal(t, "Let me look at the current implementation.", *msgs[1].DialogueText)
	assert.True(t, msgs[1].HasToolUse)
	assert.Equal(t, "claude-fable-5", msgs[1].Model)
	assert.Equal(t, "max", msgs[1].ReasoningEffort)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "toolu_01", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "Edit", msgs[3].ToolCalls[0].ToolName)
	assert.Contains(t, msgs[5].Content, "exponential backoff")
	assert.Equal(t, RoleUser, msgs[6].Role)
	assert.True(t, msgs[6].IsSystem, "user_only text is a Droid UI notice")
	assert.Equal(t, "MCP server \"docs\" is connected", msgs[6].Content)
	assert.Equal(t, RoleUser, msgs[7].Role)
	assert.False(t, msgs[7].IsSystem)
	assert.Equal(t, "The retry log in this screenshot looks wrong\n[image]", msgs[7].Content,
		"the attached-image reminder is stripped and the image is marked")
	require.NotNil(t, msgs[7].DialogueText)
	assert.Equal(t, msgs[7].Content, *msgs[7].DialogueText)

	require.Len(t, results[0].UsageEvents, 2, "session and subagent events from settings.json")
	ev := results[0].UsageEvents[0]
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 2530+900, sess.TotalOutputTokens, "subagent usage counts toward the session")
	assert.False(t, sess.HasPeakContextTokens,
		"cumulative totals say nothing about the largest single request")
	assert.Zero(t, sess.PeakContextTokens)
	assert.Equal(t, "session", ev.Source)
	assert.Equal(t, "claude-fable-5", ev.Model)
	assert.Equal(t, "anthropic", ev.ProviderID)
	assert.Equal(t, 678, ev.InputTokens)
	assert.Equal(t, 2530, ev.OutputTokens)
	assert.Equal(t, 33164, ev.CacheCreationInputTokens)
	assert.Equal(t, 3424747, ev.CacheReadInputTokens)
	assert.Equal(t, 427, ev.ReasoningTokens)
	assert.Equal(t, "session:factory:"+factoryFixtureID+":settings:claude-fable-5", ev.DedupKey)
	assert.Equal(t, "2026-07-31T21:04:00Z", ev.OccurredAt, "dated at the session end")

	child := results[0].UsageEvents[1]
	assert.Equal(t, "session", child.Source, "child totals are cumulative summaries too")
	assert.Equal(t, sess.ID, child.SessionID)
	assert.Equal(t, "claude-fable-5", child.Model)
	assert.Equal(t, "anthropic", child.ProviderID)
	assert.Equal(t, 40, child.InputTokens)
	assert.Equal(t, 900, child.OutputTokens)
	assert.Equal(t, 1200, child.CacheCreationInputTokens)
	assert.Equal(t, 2500000, child.CacheReadInputTokens)
	assert.Equal(t, 30, child.ReasoningTokens)
	assert.Equal(t, ev.OccurredAt, child.OccurredAt)
	assert.Equal(t,
		"session:factory:"+factoryFixtureID+":subagent:22222222-3333-4444-8555-666666666666",
		child.DedupKey)
}

func TestFactorySubagentUsageCountedOnceOnParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-home-dev-demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	const (
		parentID   = "aaaaaaaa-1111-4111-8111-111111111111"
		recordedID = "bbbbbbbb-2222-4222-8222-222222222222"
		missingID  = "cccccccc-3333-4333-8333-333333333333"
	)
	parent := filepath.Join(dir, parentID+".jsonl")
	require.NoError(t, os.WriteFile(parent, []byte(
		`{"type":"session_start","id":"`+parentID+`","title":"lead","cwd":"/home/dev/demo"}
{"type":"message","id":"p1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Split the work"}]}}
`), 0o644))
	require.NoError(t, os.WriteFile(factorySettingsPath(parent), []byte(`{
		"model":"claude-fable-5",
		"tokenUsage":{"inputTokens":10,"outputTokens":5},
		"childInclusiveTokenUsageBySessionId":{
			"`+recordedID+`":{"inputTokens":7,"outputTokens":70},
			"`+missingID+`":{"inputTokens":3,"outputTokens":30},
			"../escape":{"inputTokens":1,"outputTokens":1},
			"dddddddd-4444-4444-8444-444444444444":{"inputTokens":0,"outputTokens":0}
		}
	}`), 0o644))
	child := filepath.Join(dir, recordedID+".jsonl")
	require.NoError(t, os.WriteFile(child, []byte(
		`{"type":"session_start","id":"`+recordedID+`","title":"worker","callingSessionId":"`+parentID+`"}
{"type":"message","id":"c1","timestamp":"2026-07-31T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Investigate the flaky test"}]}}
`), 0o644))
	require.NoError(t, os.WriteFile(factorySettingsPath(child), []byte(
		`{"model":"claude-fable-5","tokenUsage":{"inputTokens":7,"outputTokens":70}}`,
	), 0o644))

	parentEvents := func() []ParsedUsageEvent {
		t.Helper()
		results, err := parseFactorySession(t.Context(), parent, "fallback", "mac")
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, 105, results[0].Session.TotalOutputTokens)
		return results[0].UsageEvents
	}
	withChildFile := parentEvents()
	keys := make([]string, 0, len(withChildFile))
	for _, ev := range withChildFile {
		assert.Equal(t, "session", ev.Source)
		keys = append(keys, ev.DedupKey)
	}
	assert.ElementsMatch(t, []string{
		"session:factory:" + parentID + ":settings:claude-fable-5",
		"session:factory:" + parentID + ":subagent:" + recordedID,
		"session:factory:" + parentID + ":subagent:" + missingID,
	}, keys, "every valid child with tokens is counted; invalid and empty ids are skipped")

	results, err := parseFactorySession(t.Context(), child, "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "factory:"+parentID, results[0].Session.ParentSessionID)
	assert.Equal(t, RelSubagent, results[0].Session.RelationshipType)
	assert.Empty(t, results[0].UsageEvents, "the parent already counts this child")
	assert.False(t, results[0].Session.HasTotalOutputTokens)

	require.NoError(t, os.Remove(child))
	assert.Equal(t, withChildFile, parentEvents(),
		"the parent's events do not depend on which child transcripts exist")
}

func TestParseFactoryLegacyProviderKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-home-dev-demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	const id = "11111111-2222-4333-8444-555555555555"
	transcript := filepath.Join(dir, id+".jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"type":"session_start","id":"`+id+`","title":"solo","cwd":"/home/dev/demo"}
{"type":"message","id":"s1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Investigate the flaky test"}]}}
`), 0o644))
	require.NoError(t, os.WriteFile(factorySettingsPath(transcript), []byte(
		`{"model":"claude-fable-5","apiProviderLock":"anthropic","tokenUsage":{"inputTokens":10,"outputTokens":5}}`,
	), 0o644))

	results, err := parseFactorySession(t.Context(), transcript, "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].UsageEvents, 1)
	assert.Equal(t, "anthropic", results[0].UsageEvents[0].ProviderID,
		"earlier Droid releases wrote apiProviderLock")
}

func TestFactoryCompanionMapping(t *testing.T) {
	transcript, ok := factoryTranscriptForCompanion("/x/-p/abc-123.settings.json")
	require.True(t, ok)
	assert.Equal(t, filepath.Clean("/x/-p/abc-123.jsonl"), transcript)
	_, ok = factoryTranscriptForCompanion("/x/-p/abc-123.settings.json.bak")
	assert.False(t, ok)
	assert.False(t, isFactorySourcePath("/x", "/x/-p/abc-123.settings.json"))
	assert.True(t, isFactorySourcePath("/x", "/x/-p/abc-123.jsonl"))
}

func TestFactoryProviderParseHonorsFilesystemProjectDiscoveryPolicy(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	cwd := filepath.Join(repo, "nested")
	// A plain .git directory exercises the filesystem walker without invoking Git.
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	sessions := filepath.Join(root, "sessions")
	path := filepath.Join(sessions, "-proj", factoryFixtureID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := fmt.Sprintf(`{"type":"session_start","id":%q,"title":"t","cwd":%q}
{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
`, factoryFixtureID, cwd)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	for _, tc := range []struct {
		name    string
		remote  bool
		project string
	}{
		{"local", false, "repository"},
		{"remote", true, "nested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := NewProvider(AgentFactory, ProviderConfig{Roots: []string{sessions}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)

			ctx := t.Context()
			if tc.remote {
				ctx = WithoutFilesystemProjectDiscovery(ctx)
			}
			out, err := provider.Parse(ctx, ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, out.Results, 1)
			assert.Equal(t, tc.project, out.Results[0].Result.Session.Project)
		})
	}
}

func TestParseFactorySessionStopsWhenCanceled(t *testing.T) {
	path := filepath.Join(
		factoryFixtureRoot(t), "-home-dev-workspace-demo-app", factoryFixtureID+".jsonl",
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	results, err := parseFactorySession(ctx, path, "fallback", "mac")
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, results)
}

func TestParseFactorySessionIdentityAndSidecarBounds(t *testing.T) {
	const (
		id    = "eeeeeeee-5555-4555-8555-555555555555"
		other = "ffffffff-6666-4666-8666-666666666666"
	)
	writeSession := func(t *testing.T, startLine, settings string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "-home-dev-demo")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(startLine+"\n"+
			`{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`+"\n",
		), 0o644))
		if settings != "" {
			require.NoError(t, os.WriteFile(factorySettingsPath(path), []byte(settings), 0o644))
		}
		return path
	}
	parse := func(t *testing.T, path string) ParseResult {
		t.Helper()
		results, err := parseFactorySession(t.Context(), path, "fallback", "mac")
		require.NoError(t, err)
		require.Len(t, results, 1)
		return results[0]
	}

	tests := []struct {
		name       string
		start      string
		settings   string
		wantParent string
		wantEvents int
		check      func(t *testing.T, r ParseResult)
	}{
		{
			name:  "file name wins over session_start id",
			start: `{"type":"session_start","id":"` + other + `","title":"t"}`,
			check: func(t *testing.T, r ParseResult) {
				t.Helper()
				assert.Equal(t, "factory:"+id, r.Session.ID)
			},
		},
		{
			name:  "invalid calling session id is not a parent",
			start: `{"type":"session_start","id":"` + id + `","callingSessionId":"../x"}`,
		},
		{
			name:  "self-referencing calling session id is not a parent",
			start: `{"type":"session_start","id":"` + id + `","callingSessionId":"` + id + `"}`,
		},
		{
			name:       "valid calling session id links the parent",
			start:      `{"type":"session_start","id":"` + id + `","callingSessionId":"` + other + `"}`,
			settings:   `{"model":"m","tokenUsage":{"inputTokens":5,"outputTokens":5}}`,
			wantParent: "factory:" + other,
		},
		{
			name:       "negative counters are floored",
			start:      `{"type":"session_start","id":"` + id + `"}`,
			settings:   `{"model":"m","tokenUsage":{"inputTokens":-50,"outputTokens":7,"cacheReadTokens":-1}}`,
			wantEvents: 1,
			check: func(t *testing.T, r ParseResult) {
				t.Helper()
				ev := r.UsageEvents[0]
				assert.Zero(t, ev.InputTokens)
				assert.Equal(t, 7, ev.OutputTokens)
				assert.Zero(t, ev.CacheReadInputTokens)
			},
		},
		{
			name:  "all-negative counters yield no event",
			start: `{"type":"session_start","id":"` + id + `"}`,
			settings: `{"model":"m","tokenUsage":{"inputTokens":-5,"outputTokens":-5},` +
				`"childInclusiveTokenUsageBySessionId":{"` + other + `":{"outputTokens":-9}}}`,
		},
		{
			name:  "oversized sidecar is skipped",
			start: `{"type":"session_start","id":"` + id + `"}`,
			settings: `{"model":"m","tokenUsage":{"outputTokens":5},"pad":"` +
				strings.Repeat("x", maxFactorySettingsBytes) + `"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := parse(t, writeSession(t, tc.start, tc.settings))
			assert.Equal(t, "factory:"+id, r.Session.ID)
			assert.Equal(t, tc.wantParent, r.Session.ParentSessionID)
			require.Len(t, r.UsageEvents, tc.wantEvents)
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

func TestParseFactorySessionEmptyLaunchesAndUnterminatedReminders(t *testing.T) {
	const id = "abababab-7777-4777-8777-777777777777"
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "-home-dev-demo")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		return path
	}
	start := `{"type":"session_start","id":"` + id + `","title":"t","cwd":"/home/dev/demo"}` + "\n"

	results, err := parseFactorySession(t.Context(), write(t, start), "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1, "a launch stays a session so its stored state can be replaced")
	assert.Empty(t, results[0].Messages)
	assert.Zero(t, results[0].Session.MessageCount)

	results, err = parseFactorySession(t.Context(), write(t, "{not json\n"), "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1, "a transcript that lost its content still replaces the stored session")
	assert.Empty(t, results[0].Messages)
	assert.Equal(t, 1, results[0].Session.MalformedLines)

	results, err = parseFactorySession(t.Context(), write(t, start+
		`{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>never closed"}]}}`+"\n"+
		`{"type":"message","id":"m2","timestamp":"2026-07-31T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"real prompt"}]}}`+"\n",
	), "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "real prompt", results[0].Messages[0].Content)
	assert.Equal(t, "real prompt", results[0].Session.FirstMessage)

	imageOnly := write(t, start+
		`{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},{"type":"text","text":"<system-reminder>Attached image local file paths:\n- Image 1: /tmp/a.png\n</system-reminder>"}]}}`+"\n",
	)
	results, err = parseFactorySession(t.Context(), imageOnly, "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1, "an image-only prompt is a real prompt")
	require.Len(t, results[0].Messages, 1)
	assert.Equal(t, "[image]", results[0].Messages[0].Content)
	assert.Equal(t, 1, results[0].Session.UserMessageCount)

	usageOnly := write(t, start+
		`{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","visibility":"llm_only","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"}]}}`+"\n",
	)
	require.NoError(t, os.WriteFile(factorySettingsPath(usageOnly), []byte(
		`{"model":"m","tokenUsage":{"inputTokens":4,"outputTokens":120}}`,
	), 0o644))
	results, err = parseFactorySession(t.Context(), usageOnly, "fallback", "mac")
	require.NoError(t, err)
	require.Len(t, results, 1, "a session with recorded usage is kept even without visible messages")
	assert.Empty(t, results[0].Messages)
	require.Len(t, results[0].UsageEvents, 1)
	assert.Equal(t, 120, results[0].Session.TotalOutputTokens)
}
