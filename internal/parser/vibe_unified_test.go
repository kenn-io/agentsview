package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vibeUnifiedSessionID = "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"

func vibeUnifiedChunkFixture() string {
	return `[
  {"content":[{"text":"unified question","type":"text"}],"createdAt":1790601805237,"id":"e1","role":"user","source":"turn_start","turnId":"turn-1","type":"message"},
  {"createdAt":1790601807938,"id":"reasoning-1","text":"thinking it through","turnId":"turn-1","type":"reasoning"},
  {"content":[{"text":"unified answer","type":"text"}],"createdAt":1790601807938,"id":"assistant-1","role":"assistant","source":"harness","turnId":"turn-1","type":"message"},
  {"createdAt":1790601808000,"id":"reasoning-2","text":"checking the files","turnId":"turn-1","type":"reasoning"},
  {"createdAt":1790601808027,"detail":{"kind":"tool","toolName":"file_system.bash","input":{"command":"ls"}},"id":"effect-1","state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"},"turnId":"turn-1","type":"effect","updatedAt":1790601808099},
  {"createdAt":1790601808100,"id":"notice-1","level":"info","message":"Running hooks","type":"notice"}
]`
}

func writeVibeUnifiedSession(t *testing.T, root, sessionID string) string {
	t.Helper()
	sessionDir := filepath.Join(root, "unified", sessionID)
	gen := "0000000000000001"
	writeSourceFile(t, filepath.Join(sessionDir, "chunks", "cafe0001.json"), vibeUnifiedChunkFixture())
	writeSourceFile(t, filepath.Join(sessionDir, "CURRENT"),
		`{"generation":"`+gen+`","session_id":"`+sessionID+`","snapshot_sequence":1,`+
			`"store_format":"mistral.vibe.unified-session-store/v1"}`)
	writeSourceFile(t, filepath.Join(sessionDir, "meta.json"),
		`{"session_id":"`+sessionID+`","parent_session_id":null,`+
			`"start_time":"2026-09-28T13:23:23.600000+00:00",`+
			`"end_time":"2026-09-28T13:32:53.039000+00:00",`+
			`"git_commit":null,"git_branch":null,"title":null,`+
			`"environment":{"working_directory":"/Users/dev/work/my-repo"}}`)
	genDir := filepath.Join(sessionDir, "generations", gen)
	writeSourceFile(t, filepath.Join(genDir, "manifest.json"),
		`{"generation":"`+gen+`","session_id":"`+sessionID+`",`+
			`"projection_state":{"chunks":["cafe0001"]},`+
			`"checkpoint":{"chunks":[]},"snapshot_sequence":1}`)
	writeSourceFile(t, filepath.Join(genDir, "projection-state.json"),
		`{"projection_state_version":1,"session_id":"`+sessionID+`","snapshot":{`+
			`"format":"harness.public-session-state/v1",`+
			`"session":{"createdAt":1790601803600,"id":"`+sessionID+`",`+
			`"parentSessionId":null,"title":null,"updatedAt":1790602373039,`+
			`"tokenUsage":{"cachedInputTokens":500,"inputTokens":1500,`+
			`"outputTokens":200,"totalTokens":1700},`+
			`"contextUsage":{"cachedInputTokens":400,"inputTokens":1400,`+
			`"outputTokens":50,"totalTokens":1450}}}}`)
	writeSourceFile(t, filepath.Join(genDir, "runtime-state.json"),
		`{"identity":{"depth":0,"kind":"root","parent_session_id":null,`+
			`"root_session_id":"`+sessionID+`","session_id":"`+sessionID+`"},`+
			`"session_metadata":{"active_model":"mistral-medium-3.5",`+
			`"cwd":"/Users/dev/work/my-repo"}}`)
	return sessionDir
}

func TestVibeUnifiedProviderParseInlineHistory(t *testing.T) {
	for _, tc := range []struct {
		name               string
		projectionManifest string
		wantMessages       int
		wantFirst          string
		wantAnswer         string
	}{
		{"null chunks", `{"chunks":null}`, 2, "inline question", "inline answer"},
		{"empty chunks", `{"chunks":[]}`, 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			genDir := filepath.Join(sessionDir, "generations", "0000000000000001")
			writeSourceFile(t, filepath.Join(genDir, "manifest.json"), `{"projection_state":`+tc.projectionManifest+`}`)
			writeSourceFile(t, filepath.Join(genDir, "projection-state.json"), `{"snapshot":{"history":{"entries":[
				{"type":"message","role":"user","content":[{"text":"inline question"}]},
				{"type":"message","role":"assistant","content":[{"text":"inline answer"}]}
			]}}}`)
			require.NoError(t, os.RemoveAll(filepath.Join(sessionDir, "chunks")))
			provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			require.Len(t, result.Messages, tc.wantMessages)
			assert.Equal(t, tc.wantMessages, result.Session.MessageCount)
			if tc.wantMessages > 0 {
				assert.Equal(t, tc.wantFirst, result.Messages[0].Content)
				assert.Equal(t, tc.wantAnswer, result.Messages[1].Content)
				assert.Equal(t, tc.wantFirst, result.Session.FirstMessage)
				assert.Equal(t, 1, result.Session.UserMessageCount)
			}
		})
	}
}

func TestVibeUnifiedProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
	anchor := filepath.Join(sessionDir, "CURRENT")

	provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, anchor, discovered[0].DisplayPath)
	assert.Equal(t, vibeUnifiedSessionID, discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: vibeUnifiedSessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, anchor, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	assert.Equal(t, anchor, fingerprint.Key)
	assert.NotEmpty(t, fingerprint.Hash)
	assert.Positive(t, fingerprint.Size)

	for _, changedPath := range []string{
		anchor,
		filepath.Join(sessionDir, "meta.json"),
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
			Path: changedPath, EventKind: "write", WatchRoot: root,
		})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, anchor, changed[0].DisplayPath)
	}

	require.NoError(t, os.Remove(filepath.Join(sessionDir, "meta.json")))
	_, err = provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	require.NoError(t, os.Remove(anchor))
	_, err = provider.Fingerprint(t.Context(), found)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.RemoveAll(sessionDir))
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path:      anchor,
		EventKind: "remove",
		WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, anchor, changed[0].DisplayPath)
}

func TestVibeUnifiedProviderParse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs", "session")
	writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)

	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]

	assert.Equal(t, "vibe:"+vibeUnifiedSessionID, result.Result.Session.ID)
	assert.Equal(t, AgentVibe, result.Result.Session.Agent)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, "my_repo", result.Result.Session.Project)
	assert.Equal(t, "/Users/dev/work/my-repo", result.Result.Session.Cwd)
	assert.Equal(t, "unified question", result.Result.Session.FirstMessage)
	assert.Equal(t, 1450, result.Result.Session.PeakContextTokens)
	assert.True(t, result.Result.Session.HasPeakContextTokens)
	assert.Equal(t, 200, result.Result.Session.TotalOutputTokens)
	assert.True(t, result.Result.Session.HasTotalOutputTokens)
	assert.Equal(
		t, "2026-09-28T13:23:23Z", result.Result.Session.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	)

	messages := result.Result.Messages
	require.Len(t, messages, 3)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "unified question", messages[0].Content)
	assert.Equal(t, RoleAssistant, messages[1].Role)
	assert.Equal(t, "unified answer", messages[1].Content)
	assert.True(t, messages[1].HasThinking)
	assert.Equal(t, "thinking it through", messages[1].ThinkingText)
	assert.Equal(t, "mistral-medium-3.5", messages[1].Model)
	assert.Equal(t, RoleAssistant, messages[2].Role)
	assert.Equal(t, "effect-1", messages[2].SourceUUID)
	assert.Equal(t, "mistral-medium-3.5", messages[2].Model)
	assert.True(t, messages[2].HasThinking)
	assert.Equal(t, "checking the files", messages[2].ThinkingText)
	assert.Equal(t, "2026-09-28T13:23:28.027Z", messages[2].Timestamp.UTC().Format(time.RFC3339Nano))
	require.Len(t, messages[2].ToolCalls, 1)
	assert.Equal(t, "effect-1", messages[2].ToolCalls[0].ToolUseID)
	assert.Equal(t, "file_system.bash", messages[2].ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", messages[2].ToolCalls[0].Category)
	require.Len(t, messages[2].ToolCalls[0].ResultEvents, 2)
	assert.Equal(t, "file-a\nfile-b", messages[2].ToolCalls[0].ResultEvents[1].Content)
	require.Len(t, result.Result.UsageEvents, 1)
	assert.Equal(t, "mistral-medium-3.5", result.Result.UsageEvents[0].Model)
	assert.Equal(t, 1000, result.Result.UsageEvents[0].InputTokens)
	assert.Equal(t, 200, result.Result.UsageEvents[0].OutputTokens)
	assert.Equal(t, 500, result.Result.UsageEvents[0].CacheReadInputTokens)
	for _, tc := range []struct {
		name, entry, want string
	}{
		{"image only", `{"content":[{"type":"image","attachment":{"source":{"kind":"inline","data":"AA=="},"alias":"image","mimeType":"image/png"}}]}`, "[image]"},
		{"text and image", `{"content":[{"type":"text","text":"Describe "},{"type":"image","attachment":{"source":{"kind":"inline","data":"AA=="}}}]}`, "Describe [image]"},
		{"resource only", `{"content":[{"type":"resource","resource":{"kind":"blob","uri":"file:///workspace/example.bin","blob":"AA=="}}]}`, "[resource]"},
		{"literal skill prompt", `{"content":[{"type":"text","text":"expanded skill instructions","_meta":{"vibe.userDisplayContent":{"version":"1","host":"cli","content":[{"type":"text","text":"/review literal prompt"}]}}}]}`, "/review literal prompt"},
		{"audio only", `{"content":[{"type":"audio","data":"AA==","mimeType":"audio/wav"}]}`, "[audio]"},
		{"projected literal prompt", `{"content":[{"type":"text","text":"expanded skill instructions"}],"userDisplayContent":{"version":"1","host":"cli","content":[{"type":"text","text":"/review literal prompt"}]}}`, "/review literal prompt"},
		{"literal empty prompt", `{"content":[{"type":"text","text":"expanded skill instructions"}],"userDisplayContent":{"version":"1","host":"cli","content":[]}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := strings.TrimSuffix(tc.entry, "}") + `,"type":"message","role":"user","id":"prompt"}`
			writeSourceFile(t, filepath.Join(root, "unified", vibeUnifiedSessionID, "chunks", "cafe0001.json"), "["+entry+"]")
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			parsed := outcome.Results[0].Result
			require.Len(t, parsed.Messages, 1)
			assert.Equal(t, tc.want, parsed.Messages[0].Content)
			assert.Equal(t, len(tc.want), parsed.Messages[0].ContentLength)
			assert.Equal(t, tc.want, parsed.Session.FirstMessage)
			assert.Equal(t, 1, parsed.Session.UserMessageCount)
		})
	}
	for _, tc := range []struct {
		name         string
		before       string
		after        string
		wantResult   string
		wantChild    string
		wantInput    string
		wantCategory string
		wantThinking string
		wantMessages int
		wantZeroTime bool
		wantStatus   string
	}{
		{
			name:   "effect without timestamp",
			before: `"createdAt":1790601808027`, after: `"createdAt":0`,
			wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3, wantZeroTime: true,
		},
		{
			name:       "skipped effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"skipped","reason":"Permission denied"}`,
			wantResult: "Permission denied", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
			wantStatus: "errored",
		},
		{
			name:       "failed effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"failed","error":{"message":"Command failed"},"output":{"content":[]},"outputText":""}`,
			wantResult: "Command failed", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
			wantStatus: "errored",
		},
		{
			name:       "failure output takes precedence",
			before:     `"status":"completed"`,
			after:      `"status":"failed","reason":"fallback reason","error":{"message":"fallback error"}`,
			wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
			wantStatus: "errored",
		},
		{
			name:       "reason takes precedence over error",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"failed","reason":"Stop requested","error":{"message":"fallback error"}}`,
			wantResult: "Stop requested", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
			wantStatus: "errored",
		},
		{
			name:       "running effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"running","outputText":"partial output"}`,
			wantResult: "partial output", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3, wantStatus: "running",
		},
		{
			name:       "cancelled effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"cancelled","reason":"Stop requested"}`,
			wantResult: "Stop requested", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3, wantStatus: "cancelled",
		},
		{
			name:         "text and image result",
			before:       `"content":[{"text":"file-a\nfile-b","type":"text"}]`,
			after:        `"content":[{"type":"text","text":"screenshot"},{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"resource","resource":{"kind":"text","uri":"file:///workspace/example.txt","text":"resource output"}}]`,
			wantResult:   `[{"type":"text","text":"screenshot"},{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"text","text":"resource output"}]`,
			wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "resource-only result",
			before:     `"content":[{"text":"file-a\nfile-b","type":"text"}]`,
			after:      `"content":[{"type":"resource","resource":{"kind":"text","uri":"file:///workspace/example.txt","text":"resource output"}}]`,
			wantResult: "resource output", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "skill.read",
			before:     `"toolName":"file_system.bash"`,
			after:      `"toolName":"skill.read"`,
			wantResult: "file-a\nfile-b", wantCategory: "Other", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:      "null input",
			before:    `"input":{"command":"ls"}`,
			after:     `"input":null`,
			wantInput: "{}", wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:      "absent input",
			before:    `,"input":{"command":"ls"}`,
			after:     ``,
			wantInput: "{}", wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "output content takes precedence",
			before:     `"state":{"output":`,
			after:      `"state":{"outputText":"direct output","output":`,
			wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "text and resource",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"output":{"content":[{"type":"text","text":"text output"},{"type":"resource","resource":{"kind":"text","uri":"file:///workspace/example.txt","text":"resource output"}}],"type":"success"},"outputText":"text output","status":"completed"}`,
			wantResult: "text output\nresource output", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "output text without output",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"outputText":"direct output","status":"completed"}`,
			wantResult: "direct output", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:       "subagent effect",
			before:     `"detail":{"kind":"tool","toolName":"file_system.bash","input":{"command":"ls"}}`,
			after:      `"detail":{"kind":"subagent","toolName":"subagent.spawn","input":{"task":"inspect files","agent":"generic"},"childSessionId":"child-session"}`,
			wantResult: "file-a\nfile-b", wantChild: "vibe:child-session", wantCategory: "Task", wantThinking: "checking the files", wantMessages: 3,
		},
		{
			name:   "user and steering preserve thinking for effect",
			before: `{"createdAt":1790601808027,"detail"`,
			after: `{"type":"message","role":"user","turnId":"turn-1","content":[{"type":"text","text":"extra question"}]},
			{"type":"message","role":"user","source":"turn_steer","turnId":"turn-1","content":[{"type":"text","text":"steering instruction"}]},
			{"createdAt":1790601808027,"detail"`,
			wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 5,
		},
		{
			name:   "user and steering preserve thinking for assistant",
			before: `{"content":[{"text":"unified answer"`,
			after: `{"type":"message","role":"user","turnId":"turn-1","content":[{"type":"text","text":"extra question"}]},
			{"type":"message","role":"user","source":"turn_steer","turnId":"turn-1","content":[{"type":"text","text":"steering instruction"}]},
			{"content":[{"text":"unified answer"`,
			wantResult: "file-a\nfile-b", wantCategory: "Bash", wantThinking: "checking the files", wantMessages: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunk := strings.Replace(vibeUnifiedChunkFixture(), tc.before, tc.after, 1)
			writeSourceFile(t, filepath.Join(root, "unified", vibeUnifiedSessionID, "chunks", "cafe0001.json"), chunk)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			parsed := outcome.Results[0].Result
			require.Len(t, parsed.Messages, tc.wantMessages)
			call := parsed.Messages[tc.wantMessages-1]
			assert.Equal(t, "effect-1", call.SourceUUID)
			assert.Equal(t, "mistral-medium-3.5", call.Model)
			assert.Equal(t, tc.wantThinking, call.ThinkingText)
			assert.Equal(t, tc.wantZeroTime, call.Timestamp.IsZero())
			require.Len(t, call.ToolCalls, 1)
			assert.Equal(t, tc.wantCategory, call.ToolCalls[0].Category)
			assert.Equal(t, tc.wantChild, call.ToolCalls[0].SubagentSessionID)
			if tc.wantInput != "" {
				assert.Equal(t, tc.wantInput, call.ToolCalls[0].InputJSON)
			}
			require.Len(t, call.ToolCalls[0].ResultEvents, 2)
			start := call.ToolCalls[0].ResultEvents[0]
			assert.Equal(t, "tool_execution", start.Source)
			assert.Equal(t, "effect-1", start.ToolUseID)
			assert.Equal(t, "started", start.Status)
			assert.Equal(t, call.Timestamp, start.Timestamp)
			event := call.ToolCalls[0].ResultEvents[1]
			assert.Equal(t, "tool_execution", event.Source)
			assert.Equal(t, "effect-1", event.ToolUseID)
			wantStatus := tc.wantStatus
			if wantStatus == "" {
				wantStatus = "completed"
			}
			assert.Equal(t, wantStatus, event.Status)
			if tc.name == "text and image result" {
				assert.JSONEq(t, tc.wantResult, event.Content)
			} else {
				assert.Equal(t, tc.wantResult, event.Content)
			}
			assert.Equal(t, int64(1790601808099), event.Timestamp.UnixMilli())
			for _, msg := range parsed.Messages {
				if msg.SourceUUID == "assistant-1" {
					assert.Equal(t, "mistral-medium-3.5", msg.Model)
					assert.Equal(t, "thinking it through", msg.ThinkingText)
				}
				if msg.Role != RoleAssistant {
					assert.Empty(t, msg.ThinkingText)
				}
			}
			require.Len(t, parsed.UsageEvents, 1)
			assert.Equal(t, "mistral-medium-3.5", parsed.UsageEvents[0].Model)
		})
	}

	for _, tc := range []struct {
		name          string
		entries       string
		wantContent   string
		wantThinking  string
		wantMessages  int
		wantTimestamp int64
	}{
		{
			"summary-only reasoning",
			`[{"type":"reasoning","turnId":"turn-1","text":"","summary":["checking", " the files"],"createdAt":1790601807938},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"answer"}]}]`,
			"answer", "checking the files", 1, 0,
		},
		{
			"discarded stream tails",
			`[{"type":"reasoning","turnId":"turn-1","text":"discarded thinking","outcome":{"type":"discarded"}},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"discarded answer"}],"outcome":{"type":"discarded"}},
			{"type":"reasoning","turnId":"turn-1","text":"committed thinking","outcome":{"type":"committed"}},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"committed answer"}],"outcome":{"type":"committed"}}]`,
			"discarded answer", "discarded thinking", 2, 0,
		},
		{
			"notice and checkpoint preserve turn thinking",
			`[{"type":"reasoning","turnId":"turn-1","text":"thought"},
			{"type":"notice","turnId":null,"message":"hook"},
			{"type":"checkpoint","turnId":null},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"answer"}]}]`,
			"answer", "thought", 1, 0,
		},
		{
			"trailing reasoning",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":1790601807938},
			{"type":"reasoning","turnId":"turn-1","text":" continued","createdAt":1790601808000}]`,
			"", "unfinished thought continued", 1, 1790601808000,
		},
		{
			"trailing reasoning without timestamp",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":1790601807938},
			{"type":"reasoning","turnId":"turn-1","text":" continued"}]`,
			"", "unfinished thought continued", 1, 0,
		},
		{
			"trailing reasoning with negative timestamp",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":-1}]`,
			"", "unfinished thought", 1, 0,
		},
		{
			"reasoning before another turn",
			`[{"type":"reasoning","turnId":"turn-1","text":"earlier thought","createdAt":1790601807938},
			{"type":"message","role":"assistant","turnId":"turn-2","content":[{"text":"later answer"}]}]`,
			"", "earlier thought", 2, 1790601807938,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeSourceFile(t, filepath.Join(root, "unified", vibeUnifiedSessionID, "chunks", "cafe0001.json"), tc.entries)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			messages := outcome.Results[0].Result.Messages
			require.Len(t, messages, tc.wantMessages)
			assert.Equal(t, RoleAssistant, messages[0].Role)
			assert.Equal(t, tc.wantContent, messages[0].Content)
			assert.Equal(t, tc.wantThinking, messages[0].ThinkingText)
			assert.True(t, messages[0].HasThinking)
			assert.Equal(t, "mistral-medium-3.5", messages[0].Model)
			assert.Equal(t, 0, messages[0].Ordinal)
			if tc.wantTimestamp > 0 {
				assert.Equal(t, tc.wantTimestamp, messages[0].Timestamp.UnixMilli())
			} else {
				assert.True(t, messages[0].Timestamp.IsZero())
			}
			if tc.wantMessages == 2 {
				if tc.name == "discarded stream tails" {
					assert.Equal(t, "committed answer", messages[1].Content)
					assert.Equal(t, "committed thinking", messages[1].ThinkingText)
				} else {
					assert.Equal(t, "later answer", messages[1].Content)
					assert.Empty(t, messages[1].ThinkingText)
				}
				assert.Equal(t, 1, messages[1].Ordinal)
			}
		})
	}

	t.Run("missing context does not use cumulative usage", func(t *testing.T) {
		path := filepath.Join(root, "unified", vibeUnifiedSessionID, "generations", "0000000000000001", "projection-state.json")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		writeSourceFile(t, path, strings.Replace(string(raw), `"contextUsage"`, `"unusedContext"`, 1))
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		assert.False(t, outcome.Results[0].Result.Session.HasPeakContextTokens)
		assert.Zero(t, outcome.Results[0].Result.Session.PeakContextTokens)
	})

	for _, tc := range []struct {
		name, path, document, wantError string
	}{
		{"unsafe generation", "CURRENT", `{"generation":"../x"}`, "invalid generation"},
		{"unsafe chunk", "generations/0000000000000001/manifest.json", `{"projection_state":{"chunks":["../x"]}}`, "invalid Vibe unified chunk hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			writeSourceFile(t, filepath.Join(sessionDir, tc.path), tc.document)
			provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: vibeUnifiedSessionID})
			require.NoError(t, err)
			require.True(t, found)
			_, err = provider.Parse(t.Context(), ParseRequest{Source: source})
			require.ErrorContains(t, err, tc.wantError)
		})
	}

	t.Run("runtime state errors", func(t *testing.T) {
		path := filepath.Join(root, "unified", vibeUnifiedSessionID, "generations", "0000000000000001", "runtime-state.json")
		writeSourceFile(t, path, `{`)
		_, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
		require.ErrorContains(t, err, "parsing Vibe unified runtime-state.json")
		require.NoError(t, os.Remove(path))
		_, err = provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
		require.ErrorContains(t, err, "reading Vibe unified runtime-state.json")
		writeSourceFile(t, path, `{"session_metadata":{"active_model":"mistral-medium-3.5"}}`)
	})

	t.Run("malformed optional metadata", func(t *testing.T) {
		writeSourceFile(t, filepath.Join(root, "unified", vibeUnifiedSessionID, "meta.json"), `{"session_id":"other-id","start_time":"invalid","environment":{"working_directory":"/workspace/project-a"},"git_branch":"main"}`)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		session := outcome.Results[0].Result.Session
		assert.Equal(t, "vibe:"+vibeUnifiedSessionID, session.ID)
		assert.Equal(t, vibeUnifiedSessionID, session.SourceSessionID)
		assert.Equal(t, "/workspace/project-a", session.Cwd)
		assert.Equal(t, "main", session.GitBranch)
		assert.Equal(t, "2026-09-28T13:23:23.6Z", session.StartedAt.UTC().Format(time.RFC3339Nano))
	})
}

// Subagent sessions recover identity from runtime-state when meta.json is absent.
func TestVibeUnifiedProviderParseSubagentSession(t *testing.T) {
	for _, tc := range []struct {
		name, parentRuntime, wantModel string
	}{
		{"parent recorded model", `{"session_metadata":{"active_model":"mistral-large-2411"}}`, "mistral-large-2411"},
		{"parent without model", `{"session_metadata":{}}`, ""},
		{"missing parent", "", ""},
		{"unreadable parent metadata", "{", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			childID := "6f3c8f5f-244f-ec85-b81f-d2a200000000"
			parentID := "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
			sessionDir := writeVibeUnifiedSession(t, root, childID)
			require.NoError(t, os.Remove(filepath.Join(sessionDir, "meta.json")))
			genDir := filepath.Join(sessionDir, "generations", "0000000000000001")
			writeSourceFile(t, filepath.Join(genDir, "runtime-state.json"),
				`{"identity":{"depth":1,"kind":"subagent",`+
					`"parent_session_id":"`+parentID+`",`+
					`"root_session_id":"`+parentID+`","session_id":"`+childID+`"},`+
					`"session_metadata":{`+
					`"cwd":"/Users/dev/work/my-repo"}}`)

			if tc.parentRuntime != "" {
				parentDir := writeVibeUnifiedSession(t, root, parentID)
				writeSourceFile(t, filepath.Join(parentDir, "CURRENT"), `{"generation":"0000000000000002"}`)
				writeSourceFile(t, filepath.Join(parentDir, "generations", "0000000000000002", "runtime-state.json"), tc.parentRuntime)
			}

			provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: childID})
			require.NoError(t, err)
			require.True(t, found)
			fingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			outcome, err := provider.Parse(t.Context(), ParseRequest{
				Source:      source,
				Fingerprint: fingerprint,
			})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			session := result.Session

			assert.Equal(t, "vibe:"+childID, session.ID)
			assert.Equal(t, "vibe:"+parentID, session.ParentSessionID)
			assert.Equal(t, RelSubagent, session.RelationshipType)
			assert.Equal(t, "my_repo", session.Project)
			assert.True(t, time.UnixMilli(1790601803600).Equal(session.StartedAt))
			require.Len(t, result.Messages, 3)
			assert.Equal(t, tc.wantModel, result.Messages[1].Model)
			assert.Equal(t, tc.wantModel, result.Messages[2].Model)
			require.Len(t, result.UsageEvents, 1)
			assert.Equal(t, tc.wantModel, result.UsageEvents[0].Model)

			parentDir := filepath.Join(root, "unified", parentID)
			writeSourceFile(t, filepath.Join(parentDir, "CURRENT"), `{"generation":"0000000000000003"}`)
			writeSourceFile(t, filepath.Join(parentDir, "generations", "0000000000000003", "runtime-state.json"), `{"session_metadata":{"active_model":"mistral-medium-3.5"}}`)
			parentTime := time.Unix(2_000_000_000, 0)
			require.NoError(t, os.Chtimes(filepath.Join(parentDir, "CURRENT"), parentTime, parentTime))
			updatedFingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, fingerprint.Hash, updatedFingerprint.Hash)
			assert.Equal(t, fingerprint.Size, updatedFingerprint.Size)
			assert.Equal(t, int64(2_000_000_000_000_000_000), updatedFingerprint.MTimeNS)
			outcome, err = provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: updatedFingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result = outcome.Results[0].Result
			assert.Equal(t, "mistral-medium-3.5", result.Messages[1].Model)
			require.Len(t, result.UsageEvents, 1)
			assert.Equal(t, "mistral-medium-3.5", result.UsageEvents[0].Model)
		})
	}
}

func TestVibeUnifiedProviderParseLineage(t *testing.T) {
	for _, tc := range []struct {
		name, kind, provenance, model string
		wantRelationship              RelationshipType
	}{
		{"import", "root", `,"import_provenance":{"source":{"backend":"legacy","session_id":"legacy-parent"}}`, "", RelContinuation},
		{"fork", "fork", `,"import_provenance":{"source":{"backend":"unified","session_id":"legacy-parent"}}`, "", RelFork},
		{"pinned model", "fork", `,"import_provenance":{"source":{"backend":"unified","session_id":"legacy-parent"}}`, "mistral-large-2411", RelFork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			genDir := filepath.Join(sessionDir, "generations", "0000000000000001")
			writeSourceFile(t, filepath.Join(genDir, "runtime-state.json"), `{"identity":{"kind":"`+tc.kind+`","parent_session_id":"legacy-parent"},"session_metadata":{"active_model":"`+tc.model+`"}`+tc.provenance+`}`)
			writeSourceFile(t, filepath.Join(sessionDir, "chunks", "cafe0001.json"), `[
				{"type":"message","id":"imported-1","role":"user","content":[{"text":"old question"}]},
				{"type":"message","id":"imported-2","role":"assistant","content":[{"text":"old answer"}]},
				{"type":"message","id":"new-1","role":"user","content":[{"text":"new question"}]},
				{"type":"message","id":"new-2","role":"assistant","content":[{"text":"new answer"}]}
			]`)
			provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: vibeUnifiedSessionID})
			require.NoError(t, err)
			require.True(t, found)
			fingerprint, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Equal(t, "vibe:legacy-parent", result.Session.ParentSessionID)
			assert.Equal(t, tc.wantRelationship, result.Session.RelationshipType)
			require.Len(t, result.Messages, 4)
			assert.Equal(t, "old question", result.Session.FirstMessage)
			assert.Equal(t, 2, result.Session.UserMessageCount)
			assert.Equal(t, "old answer", result.Messages[1].Content)
			assert.Equal(t, "new question", result.Messages[2].Content)
			assert.Equal(t, "new answer", result.Messages[3].Content)
			assert.Equal(t, tc.model, result.Messages[1].Model)
			assert.Equal(t, tc.model, result.Messages[3].Model)
			require.Len(t, result.UsageEvents, 1)
			assert.Equal(t, tc.model, result.UsageEvents[0].Model)
			assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
			assert.Empty(t, outcome.Results[0].RetryReason)
		})
	}
}
