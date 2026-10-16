package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

const vibeUnifiedTestGeneration = "0000000000000001"

func vibeUnifiedDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// writeVibeUnifiedPointer publishes a generation. Stored documents end in a
// newline that the manifest digest excludes.
func writeVibeUnifiedPointer(t *testing.T, sessionDir, generation string) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(sessionDir, "generations", generation, "manifest.json"))
	require.NoError(t, err)
	writeSourceFile(t, filepath.Join(sessionDir, "CURRENT"), fmt.Sprintf(
		`{"generation":%q,"manifest_sha256":%q,"session_id":%q,"snapshot_sequence":1,"store_format":"mistral.vibe.unified-session-store/v1","store_format_minor":8}`+"\n",
		generation, vibeUnifiedDigest(strings.TrimSuffix(string(manifest), "\n")), filepath.Base(sessionDir)))
}

// writeVibeUnifiedManifest writes and publishes a manifest whose journal
// segment starts after snapshot sequence 1.
func writeVibeUnifiedManifest(t *testing.T, sessionDir, generation, projectionState string) {
	t.Helper()
	writeSourceFile(t, filepath.Join(sessionDir, "generations", generation, "manifest.json"),
		`{"manifest_version":1,"generation":"`+generation+`","session_id":"`+filepath.Base(sessionDir)+`",`+
			`"projection_state":`+projectionState+`,"snapshot_sequence":1,`+
			`"recovery_journal_segment":{"path":"journal/0000000000000002.jsonl","first_sequence":2}}`+"\n")
	writeVibeUnifiedPointer(t, sessionDir, generation)
}

// setVibeUnifiedTranscript pools entries into chunks named by their digests.
func setVibeUnifiedTranscript(t *testing.T, sessionDir string, chunks ...string) {
	t.Helper()
	names := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		name := vibeUnifiedDigest(chunk)
		writeSourceFile(t, filepath.Join(sessionDir, "chunks", name+".json"), chunk+"\n")
		names = append(names, `"`+name+`"`)
	}
	writeVibeUnifiedManifest(t, sessionDir, vibeUnifiedTestGeneration, `{"chunks":[`+strings.Join(names, ",")+`]}`)
}

// writeVibeUnifiedJournal writes digest-chained journal records, given as
// type and payload pairs, starting at the manifest's first sequence.
func writeVibeUnifiedJournal(t *testing.T, sessionDir string, records ...[2]string) {
	t.Helper()
	var journal strings.Builder
	previous := "null"
	for i, record := range records {
		body := fmt.Sprintf(`{"payload":%s,"previous_record_sha256":%s,"recovery_journal_record_version":1,"sequence":%d,"type":%q}`,
			record[1], previous, i+2, record[0])
		digest := vibeUnifiedDigest(body)
		journal.WriteString(strings.Replace(body, `,"recovery_journal_record_version"`,
			`,"record_sha256":"`+digest+`","recovery_journal_record_version"`, 1) + "\n")
		previous = `"` + digest + `"`
	}
	writeSourceFile(t, filepath.Join(sessionDir, "journal", "0000000000000002.jsonl"), journal.String())
}

func writeVibeUnifiedSession(t *testing.T, root, sessionID string) string {
	t.Helper()
	sessionDir := filepath.Join(root, "unified", sessionID)
	gen := vibeUnifiedTestGeneration
	writeSourceFile(t, filepath.Join(sessionDir, "meta.json"),
		`{"session_id":"`+sessionID+`","parent_session_id":null,`+
			`"start_time":"2026-09-28T13:23:23.600000+00:00",`+
			`"end_time":"2026-09-28T13:32:53.039000+00:00",`+
			`"git_commit":null,"git_branch":null,"title":null,`+
			`"environment":{"working_directory":"/workspace/my-repo"}}`)
	genDir := filepath.Join(sessionDir, "generations", gen)
	writeSourceFile(t, filepath.Join(genDir, "projection-state.json"),
		`{"projection_state_version":1,"session_id":"`+sessionID+`","snapshot":{`+
			`"format":"harness.public-session-state/v1",`+
			`"session":{"createdAt":1790601803600,"id":"`+sessionID+`",`+
			`"parentSessionId":null,"title":null,"updatedAt":1790602373039,`+
			`"tokenUsage":{"cachedInputTokens":500,"inputTokens":1500,`+
			`"outputTokens":200,"totalTokens":1700},`+
			`"contextUsage":{"cachedInputTokens":400,"inputTokens":1400,`+
			`"outputTokens":50,"totalTokens":1450}}}}`+"\n")
	writeSourceFile(t, filepath.Join(genDir, "runtime-state.json"),
		`{"runtime_state_version":3,"identity":{"depth":0,"kind":"root","parent_session_id":null,`+
			`"root_session_id":"`+sessionID+`","session_id":"`+sessionID+`"},`+
			`"session_metadata":{"active_model":"mistral-medium-3.5",`+
			`"cwd":"/workspace/my-repo"}}`+"\n")
	writeVibeUnifiedJournal(t, sessionDir)
	setVibeUnifiedTranscript(t, sessionDir, vibeUnifiedChunkFixture())
	return sessionDir
}

func TestVibeUnifiedParseSourceSize(t *testing.T) {
	root := t.TempDir()
	sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
	parentDir := writeVibeUnifiedSession(t, root, "parent")
	gen := vibeUnifiedTestGeneration
	writeSourceFile(t, filepath.Join(sessionDir, "generations", gen, "runtime-state.json"), `{"runtime_state_version":3,"identity":{"parent_session_id":"parent"},"session_metadata":{}}`)
	second := `[{"type":"message","role":"assistant","content":[{"text":"second chunk answer"}]}]`
	setVibeUnifiedTranscript(t, sessionDir, vibeUnifiedChunkFixture(), second)
	writeVibeUnifiedJournal(t, sessionDir, [2]string{"projection_delta", `{"delta":[],"watermark":1}`})
	provider, ok := NewProvider(AgentVibe, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: filepath.Join(sessionDir, "CURRENT")})
	require.NoError(t, err)
	require.True(t, found)

	var want int64
	for _, path := range []string{
		filepath.Join(sessionDir, "CURRENT"), filepath.Join(sessionDir, "meta.json"),
		filepath.Join(sessionDir, "generations", gen, "manifest.json"),
		filepath.Join(sessionDir, "generations", gen, "projection-state.json"),
		filepath.Join(sessionDir, "generations", gen, "runtime-state.json"),
		filepath.Join(sessionDir, "chunks", vibeUnifiedDigest(vibeUnifiedChunkFixture())+".json"),
		filepath.Join(sessionDir, "chunks", vibeUnifiedDigest(second)+".json"),
		filepath.Join(sessionDir, "journal", "0000000000000002.jsonl"),
		filepath.Join(parentDir, "CURRENT"),
		filepath.Join(parentDir, "generations", gen, "manifest.json"),
		filepath.Join(parentDir, "generations", gen, "runtime-state.json"),
	} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		want += info.Size()
	}
	require.Implements(t, (*ParseSourceSizer)(nil), provider)
	size, err := provider.(ParseSourceSizer).ParseSourceSize(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, want, size)
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
			genDir := filepath.Join(sessionDir, "generations", vibeUnifiedTestGeneration)
			writeVibeUnifiedManifest(t, sessionDir, vibeUnifiedTestGeneration, tc.projectionManifest)
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
		filepath.Join(sessionDir, "journal", "0000000000000002.jsonl"),
	} {
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
			Path: changedPath, EventKind: "write", WatchRoot: root,
		})
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, anchor, changed[0].DisplayPath)
	}

	// Every publication also writes temporary files and generation
	// directories; none of them may fall back to a full provider scan.
	for _, tc := range []struct {
		path string
		want ChangedPathRelevance
	}{
		{"unified/" + vibeUnifiedSessionID + "/CURRENT", ChangedPathDataBearing},
		{"unified/" + vibeUnifiedSessionID + "/meta.json", ChangedPathDataBearing},
		{"unified/" + vibeUnifiedSessionID + "/journal/0000000000000002.jsonl", ChangedPathDataBearing},
		{"unified/" + vibeUnifiedSessionID + "/.CURRENT.0123456789abcdef.tmp", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/tmpa1b2c3d4.json.tmp", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/generations/0000000000000001", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/generations/0000000000000001/manifest.json", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/generations/.staging-0000000000000002-0123456789abcdef", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/chunks", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/chunks/" + strings.Repeat("a", 64) + ".json", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/chunks/." + strings.Repeat("a", 64) + ".json.0123456789abcdef.tmp", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/journal", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID + "/quarantine/0000000000000001.jsonl", ChangedPathNonData},
		{"active/" + vibeUnifiedSessionID + ".lock", ChangedPathNonData},
		{"unified/" + vibeUnifiedSessionID, ChangedPathUnclassified},
		{"unknown.json", ChangedPathUnclassified},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, kind := range []string{"write", "remove"} {
				relevance, err := ResolveChangedPathRelevance(t.Context(), provider, ChangedPathRequest{Path: filepath.Join(root, filepath.FromSlash(tc.path)), EventKind: kind, WatchRoot: root})
				require.NoError(t, err)
				assert.Equal(t, tc.want, relevance)
			}
		})
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
	fixture := func(t *testing.T) (string, Provider, SourceRef, SourceFingerprint) {
		t.Helper()

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

		return root, provider, sources[0], fingerprint
	}
	_, provider, source, fingerprint := fixture(t)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
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
	assert.Equal(t, "/workspace/my-repo", result.Result.Session.Cwd)
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
	start := messages[2].ToolCalls[0].ResultEvents[0]
	assert.Equal(t, "tool_execution", start.Source)
	assert.Equal(t, "effect-1", start.ToolUseID)
	assert.Equal(t, "started", start.Status)
	assert.Equal(t, messages[2].Timestamp, start.Timestamp)
	event := messages[2].ToolCalls[0].ResultEvents[1]
	assert.Equal(t, "tool_execution", event.Source)
	assert.Equal(t, "effect-1", event.ToolUseID)
	assert.Equal(t, "completed", event.Status)
	assert.Equal(t, int64(1790601808099), event.Timestamp.UnixMilli())
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
			root, provider, source, fingerprint := fixture(t)
			entry := strings.TrimSuffix(tc.entry, "}") + `,"type":"message","role":"user","id":"prompt"}`
			setVibeUnifiedTranscript(t, filepath.Join(root, "unified", vibeUnifiedSessionID), "["+entry+"]")
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
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
		name                  string
		before                string
		after                 string
		wantResult            string
		wantChild             string
		wantInput             string
		wantCategory          string
		wantThinking          string
		wantAssistantThinking string
		wantMessages          int
		wantZeroTime          bool
		wantStatus            string
		wantJSON              bool
	}{
		{
			name:         "search_replace effect",
			before:       `"toolName":"file_system.bash"`,
			after:        `"toolName":"file_system.search_replace"`,
			wantCategory: "Edit",
		},
		{
			name:   "effect without timestamp",
			before: `"createdAt":1790601808027`, after: `"createdAt":0`,
			wantZeroTime: true,
		},
		{
			name:       "skipped effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"skipped","reason":"Permission denied"}`,
			wantResult: "Permission denied",
			wantStatus: "errored",
		},
		{
			name:       "failed effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"failed","error":{"message":"Command failed"},"output":{"content":[]},"outputText":""}`,
			wantResult: "Command failed",
			wantStatus: "errored",
		},
		{
			name:       "failure output takes precedence",
			before:     `"status":"completed"`,
			after:      `"status":"failed","reason":"fallback reason","error":{"message":"fallback error"}`,
			wantResult: "file-a\nfile-b",
			wantStatus: "errored",
		},
		{
			name:       "reason takes precedence over error",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"failed","reason":"Stop requested","error":{"message":"fallback error"}}`,
			wantResult: "Stop requested",
			wantStatus: "errored",
		},
		{
			name:       "running effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"running","outputText":"partial output"}`,
			wantResult: "partial output", wantStatus: "running",
		},
		{
			name:       "cancelled effect",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"status":"cancelled","reason":"Stop requested"}`,
			wantResult: "Stop requested", wantStatus: "cancelled",
		},
		{
			name:       "text and image result",
			wantJSON:   true,
			before:     `"content":[{"text":"file-a\nfile-b","type":"text"}]`,
			after:      `"content":[{"type":"text","text":"screenshot"},{"type":"image","data":"AA==","mimeType":"image/png"},{"type":"resource","resource":{"kind":"text","uri":"file:///workspace/example.txt","text":"resource output"}}]`,
			wantResult: `[{"type":"text","text":"screenshot"},{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"text","text":"resource output"}]`,
		},
		{
			name:       "bash structured output",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"output":{"content":[],"structured_content":{"command":"ls","returncode":1,"stderr":"ls: missing","stdout":"file-a\n","was_truncated":false},"type":"success"},"outputText":"","status":"completed"}`,
			wantResult: "file-a\nls: missing",
		},
		{
			name:       "read_file structured output",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"output":{"content":[],"structured_content":{"content":"package main\n","file_size_bytes":13,"lines_read":1,"offset":0,"path":"main.go","returned_bytes":13,"was_truncated":false},"type":"success"},"outputText":"","status":"completed"}`,
			wantResult: "package main\n",
		},
		{
			name:       "write_file structured output",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"output":{"content":[],"structured_content":{"bytes_written":5,"file_existed":false,"path":"notes.txt"},"type":"success"},"outputText":"","status":"completed"}`,
			wantResult: `{"bytes_written":5,"file_existed":false,"path":"notes.txt"}`,
		},
		{
			name:       "content blocks precede structured output",
			before:     `"type":"success"},"status":"completed"`,
			after:      `"structured_content":{"stdout":"unused"},"type":"success"},"status":"completed"`,
			wantResult: "file-a\nfile-b",
		},
		{
			name:      "null input",
			before:    `"input":{"command":"ls"}`,
			after:     `"input":null`,
			wantInput: "{}",
		},
		{
			name:      "absent input",
			before:    `,"input":{"command":"ls"}`,
			after:     ``,
			wantInput: "{}",
		},
		{
			name:       "text and resource",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"output":{"content":[{"type":"text","text":"text output"},{"type":"resource","resource":{"kind":"text","uri":"file:///workspace/example.txt","text":"resource output"}}],"type":"success"},"outputText":"text output","status":"completed"}`,
			wantResult: "text output\nresource output",
		},
		{
			name:       "output text without output",
			before:     `"state":{"output":{"content":[{"text":"file-a\nfile-b","type":"text"}],"type":"success"},"status":"completed"}`,
			after:      `"state":{"outputText":"direct output","status":"completed"}`,
			wantResult: "direct output",
		},
		{
			name:      "subagent effect",
			before:    `"detail":{"kind":"tool","toolName":"file_system.bash","input":{"command":"ls"}}`,
			after:     `"detail":{"kind":"subagent","toolName":"subagent.spawn","input":{"task":"inspect files","agent":"generic"},"childSessionId":"child-session"}`,
			wantChild: "vibe:child-session",
		},
		{
			name:   "user and steering preserve thinking for effect",
			before: `{"createdAt":1790601808027,"detail"`,
			after: `{"type":"message","role":"user","turnId":"turn-1","content":[{"type":"text","text":"extra question"}]},
			{"type":"message","role":"user","source":"turn_steer","turnId":"turn-1","content":[{"type":"text","text":"steering instruction"}]},
			{"createdAt":1790601808027,"detail"`,
			wantThinking: "checking the files", wantMessages: 5,
		},
		{
			name:   "user and steering preserve thinking for assistant",
			before: `{"content":[{"text":"unified answer"`,
			after: `{"type":"message","role":"user","turnId":"turn-1","content":[{"type":"text","text":"extra question"}]},
			{"type":"message","role":"user","source":"turn_steer","turnId":"turn-1","content":[{"type":"text","text":"steering instruction"}]},
			{"content":[{"text":"unified answer"`,
			wantAssistantThinking: "thinking it through", wantMessages: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, provider, source, fingerprint := fixture(t)
			chunk := strings.Replace(vibeUnifiedChunkFixture(), tc.before, tc.after, 1)
			setVibeUnifiedTranscript(t, filepath.Join(root, "unified", vibeUnifiedSessionID), chunk)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			parsed := outcome.Results[0].Result
			if tc.wantMessages != 0 {
				require.Len(t, parsed.Messages, tc.wantMessages)
			}
			require.NotEmpty(t, parsed.Messages)
			call := parsed.Messages[len(parsed.Messages)-1]
			if tc.wantThinking != "" {
				assert.Equal(t, tc.wantThinking, call.ThinkingText)
			}
			if tc.wantAssistantThinking != "" {
				for _, msg := range parsed.Messages {
					if msg.SourceUUID == "assistant-1" {
						assert.Equal(t, tc.wantAssistantThinking, msg.ThinkingText)
					}
					if msg.Role != RoleAssistant {
						assert.Empty(t, msg.ThinkingText)
					}
				}
			}
			if tc.wantZeroTime {
				assert.True(t, call.Timestamp.IsZero())
			}
			require.Len(t, call.ToolCalls, 1)
			if tc.wantCategory != "" {
				assert.Equal(t, tc.wantCategory, call.ToolCalls[0].Category)
			}
			if tc.wantChild != "" {
				assert.Equal(t, tc.wantChild, call.ToolCalls[0].SubagentSessionID)
			}
			if tc.wantInput != "" {
				assert.Equal(t, tc.wantInput, call.ToolCalls[0].InputJSON)
			}
			require.Len(t, call.ToolCalls[0].ResultEvents, 2)
			event := call.ToolCalls[0].ResultEvents[1]
			if tc.wantStatus != "" {
				assert.Equal(t, tc.wantStatus, event.Status)
			}
			if tc.wantJSON {
				assert.JSONEq(t, tc.wantResult, event.Content)
			} else if tc.wantResult != "" {
				assert.Equal(t, tc.wantResult, event.Content)
			}
		})
	}

	for _, tc := range []struct {
		name             string
		entries          string
		wantContent      string
		wantThinking     string
		wantMessages     int
		wantTimestamp    int64
		wantNextContent  string
		wantNextThinking string
	}{
		{
			"summary-only reasoning",
			`[{"type":"reasoning","turnId":"turn-1","text":"","summary":["checking", " the files"],"createdAt":1790601807938},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"answer"}]}]`,
			"answer", "checking the files", 1, 0,
			"", "",
		},
		{
			"discarded stream tails",
			`[{"type":"reasoning","turnId":"turn-1","text":"discarded thinking","outcome":{"type":"discarded"}},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"discarded answer"}],"outcome":{"type":"discarded"}},
			{"type":"reasoning","turnId":"turn-1","text":"committed thinking","outcome":{"type":"committed"}},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"committed answer"}],"outcome":{"type":"committed"}}]`,
			"discarded answer", "discarded thinking", 2, 0,
			"committed answer", "committed thinking",
		},
		{
			"notice and checkpoint preserve turn thinking",
			`[{"type":"reasoning","turnId":"turn-1","text":"thought"},
			{"type":"notice","turnId":null,"message":"hook"},
			{"type":"checkpoint","turnId":null},
			{"type":"message","role":"assistant","turnId":"turn-1","content":[{"text":"answer"}]}]`,
			"answer", "thought", 1, 0,
			"", "",
		},
		{
			"trailing reasoning",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":1790601807938},
			{"type":"reasoning","turnId":"turn-1","text":" continued","createdAt":1790601808000}]`,
			"", "unfinished thought continued", 1, 1790601808000,
			"", "",
		},
		{
			"trailing reasoning without timestamp",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":1790601807938},
			{"type":"reasoning","turnId":"turn-1","text":" continued"}]`,
			"", "unfinished thought continued", 1, 0,
			"", "",
		},
		{
			"trailing reasoning with negative timestamp",
			`[{"type":"reasoning","turnId":"turn-1","text":"unfinished thought","createdAt":-1}]`,
			"", "unfinished thought", 1, 0,
			"", "",
		},
		{
			"reasoning before another turn",
			`[{"type":"reasoning","turnId":"turn-1","text":"earlier thought","createdAt":1790601807938},
			{"type":"message","role":"assistant","turnId":"turn-2","content":[{"text":"later answer"}]}]`,
			"", "earlier thought", 2, 1790601807938,
			"later answer", "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, provider, source, fingerprint := fixture(t)
			setVibeUnifiedTranscript(t, filepath.Join(root, "unified", vibeUnifiedSessionID), tc.entries)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
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
				assert.Equal(t, tc.wantNextContent, messages[1].Content)
				assert.Equal(t, tc.wantNextThinking, messages[1].ThinkingText)
				assert.Equal(t, 1, messages[1].Ordinal)
			}
		})
	}

	t.Run("missing context does not use cumulative usage", func(t *testing.T) {
		root, provider, source, fingerprint := fixture(t)
		path := filepath.Join(root, "unified", vibeUnifiedSessionID, "generations", "0000000000000001", "projection-state.json")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		writeSourceFile(t, path, strings.Replace(string(raw), `"contextUsage"`, `"unusedContext"`, 1))
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		assert.False(t, outcome.Results[0].Result.Session.HasPeakContextTokens)
		assert.Zero(t, outcome.Results[0].Result.Session.PeakContextTokens)
	})

	manifestPath := "generations/" + vibeUnifiedTestGeneration + "/manifest.json"
	recordPrefix := `{"payload":{"delta":[],"watermark":1},"previous_record_sha256":null,"record_sha256":"`
	for _, tc := range []struct {
		name, path, document string
		republish            bool
		wantError            string
	}{
		{"unsafe generation", "CURRENT", `{"generation":"../x"}`, false, "invalid generation"},
		{"unsafe chunk", manifestPath, `{"projection_state":{"chunks":["../x"]}}`, true, "invalid Vibe unified chunk hash"},
		{"short chunk name", manifestPath, `{"projection_state":{"chunks":["cafe0001"]}}`, true, "invalid Vibe unified chunk hash"},
		{"unsafe journal", manifestPath, `{"projection_state":{"chunks":null},"recovery_journal_segment":{"path":"../x.jsonl","first_sequence":2}}`, true, "invalid Vibe unified journal segment"},
		{"manifest not published", manifestPath, `{"projection_state":{"chunks":null}}`, false, "manifest digest does not match"},
		{"journal digest mismatch", "journal/0000000000000002.jsonl", recordPrefix + strings.Repeat("0", 64) + `","recovery_journal_record_version":1,"sequence":2,"type":"projection_delta"}` + "\n", false, "breaks the digest chain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			writeSourceFile(t, filepath.Join(sessionDir, tc.path), tc.document)
			if tc.republish {
				writeVibeUnifiedPointer(t, sessionDir, vibeUnifiedTestGeneration)
			}
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
		root, provider, source, fingerprint := fixture(t)
		path := filepath.Join(root, "unified", vibeUnifiedSessionID, "generations", "0000000000000001", "runtime-state.json")
		writeSourceFile(t, path, `{`)
		_, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
		require.ErrorContains(t, err, "parsing Vibe unified runtime-state.json")
		require.NoError(t, os.Remove(path))
		_, err = provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
		require.ErrorContains(t, err, "reading Vibe unified runtime-state.json")
	})

	t.Run("metadata overrides project fields while directory supplies session ID", func(t *testing.T) {
		root, provider, source, fingerprint := fixture(t)
		writeSourceFile(t, filepath.Join(root, "unified", vibeUnifiedSessionID, "meta.json"), `{"session_id":"other-id","start_time":"invalid","environment":{"working_directory":"/workspace/project-a"},"git_branch":"main"}`)
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
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
		{"parent recorded model", `{"runtime_state_version":3,"session_metadata":{"active_model":"mistral-large-2411"}}`, "mistral-large-2411"},
		{"parent without model", `{"runtime_state_version":3,"session_metadata":{}}`, ""},
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
				`{"runtime_state_version":3,"identity":{"depth":1,"kind":"subagent",`+
					`"parent_session_id":"`+parentID+`",`+
					`"root_session_id":"`+parentID+`","session_id":"`+childID+`"},`+
					`"session_metadata":{`+
					`"cwd":"/workspace/my-repo"}}`)

			if tc.parentRuntime != "" {
				parentDir := writeVibeUnifiedSession(t, root, parentID)
				writeSourceFile(t, filepath.Join(parentDir, "generations", "0000000000000002", "runtime-state.json"), tc.parentRuntime)
				writeVibeUnifiedManifest(t, parentDir, "0000000000000002", `{"chunks":null}`)
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
		})
	}
}

// A running turn exists only in the journal until the session settles.
func TestVibeUnifiedProviderParseJournal(t *testing.T) {
	envelope := `{"op":"set_envelope","state":{"history":{"entries":[]},"session":{"createdAt":1790601803600,"title":"Journaled title","updatedAt":1790602400000,` +
		`"tokenUsage":{"cachedInputTokens":600,"inputTokens":3000,"outputTokens":400},"contextUsage":{"inputTokens":2000,"outputTokens":100}}}}`
	running := `{"type":"effect","id":"effect-2","turnId":"turn-2","createdAt":1790602390000,"detail":{"kind":"tool","toolName":"file_system.bash","input":{"command":"make"}},"state":{"status":"running","outputText":""}}`
	for _, tc := range []struct {
		name    string
		records [][2]string
		check   func(t *testing.T, result ParseResult)
	}{
		{
			name: "deltas advance the generation",
			records: [][2]string{
				{"projection_delta", `{"delta":[` +
					`{"op":"append_entry","entry":{"type":"message","id":"user-2","role":"user","turnId":"turn-2","content":[{"type":"text","text":"follow-up"}]}},` +
					`{"op":"append_entry","entry":` + running + `},` +
					`{"op":"append_entry","entry":{"type":"effect","id":"effect-3","turnId":"turn-2","detail":{"kind":"tool","toolName":"file_system.read_file"},"state":{"status":"running"}}},` +
					envelope + `],"watermark":2}`},
				{"command_reserved", `{"client_command_id":"command-1"}`},
				{"projection_delta", `{"delta":[` +
					`{"op":"replace_entry","id":"effect-2","entry":` + strings.Replace(running, `"state":{"status":"running","outputText":""}`, `"state":{"status":"completed","output":{"content":[],"structured_content":{"stdout":"built"},"type":"success"},"outputText":""}`, 1) + `},` +
					`{"op":"remove_entry","id":"effect-3"}],"watermark":3}`},
			},
			check: func(t *testing.T, result ParseResult) {
				t.Helper()
				messages := result.Messages
				require.Len(t, messages, 5)
				assert.Equal(t, "follow-up", messages[3].Content)
				require.Len(t, messages[4].ToolCalls, 1)
				call := messages[4].ToolCalls[0]
				assert.Equal(t, "effect-2", call.ToolUseID)
				require.Len(t, call.ResultEvents, 2)
				assert.Equal(t, "completed", call.ResultEvents[1].Status)
				assert.Equal(t, "built", call.ResultEvents[1].Content)
				assert.Equal(t, 2, result.Session.UserMessageCount)
				assert.Equal(t, "unified question", result.Session.FirstMessage)
				assert.Equal(t, "Journaled title", result.Session.SessionName)
				assert.Equal(t, 2100, result.Session.PeakContextTokens)
				require.Len(t, result.UsageEvents, 1)
				assert.Equal(t, 2400, result.UsageEvents[0].InputTokens)
				assert.Equal(t, 600, result.UsageEvents[0].CacheReadInputTokens)
				assert.Equal(t, 400, result.UsageEvents[0].OutputTokens)
			},
		},
		{
			name: "full snapshot record",
			records: [][2]string{{"projection_advanced", `{"snapshot":{"history":{"entries":[` +
				`{"type":"message","id":"only","role":"user","content":[{"type":"text","text":"replaced history"}]}]},"session":{}},"watermark":2}`}},
			check: func(t *testing.T, result ParseResult) {
				t.Helper()
				require.Len(t, result.Messages, 1)
				assert.Equal(t, "replaced history", result.Messages[0].Content)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			writeVibeUnifiedJournal(t, sessionDir, tc.records...)
			journalPath := filepath.Join(sessionDir, "journal", "0000000000000002.jsonl")
			// An append still being written has no newline yet.
			journal, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = journal.WriteString(`{"payload":{"delta":[{"op":"remove_entry","id":"e1"}]`)
			require.NoError(t, err)
			require.NoError(t, journal.Close())

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
			tc.check(t, outcome.Results[0].Result)

			// Completing the append changes the fingerprint.
			journal, err = os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = journal.WriteString("\n")
			require.NoError(t, err)
			require.NoError(t, journal.Close())
			updated, err := provider.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, fingerprint.Hash, updated.Hash)
		})
	}
}

func TestVibeUnifiedProviderParseLineage(t *testing.T) {
	for _, tc := range []struct {
		name, kind, provenance string
		wantRelationship       RelationshipType
	}{
		{"import", "root", `,"import_provenance":{"source":{"backend":"legacy","session_id":"legacy-parent"}}`, RelContinuation},
		{"fork", "fork", `,"import_provenance":{"source":{"backend":"unified","session_id":"legacy-parent"}}`, RelFork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessionDir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			genDir := filepath.Join(sessionDir, "generations", "0000000000000001")
			writeSourceFile(t, filepath.Join(genDir, "runtime-state.json"), `{"runtime_state_version":3,"identity":{"kind":"`+tc.kind+`","parent_session_id":"legacy-parent"}`+tc.provenance+`}`)
			setVibeUnifiedTranscript(t, sessionDir, `[
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
		})
	}
}

func TestVibeUnifiedVersions(t *testing.T) {
	for _, tc := range []struct {
		name, file, prefix string
		valid              bool
		parent             bool
	}{
		{"default store", "CURRENT", ``, true, false},
		{"minor one", "CURRENT", `"store_format_minor":1,`, true, false},
		{"minor eight", "CURRENT", `"store_format_minor":8,`, true, false},
		{"new store", "CURRENT", `"store_format":"mistral.vibe.unified-session-store/v2",`, false, false},
		{"new minor", "CURRENT", `"store_format_minor":9,`, false, false},
		{"invalid minor", "CURRENT", `"store_format_minor":0,`, false, false},
		{"new manifest", "manifest.json", `"manifest_version":2,`, false, false},
		{"new projection", "projection-state.json", `"projection_state_version":2,`, false, false},
		{"new runtime", "runtime-state.json", `"runtime_state_version":4,`, false, false},
		{"missing runtime", "runtime-state.json", ``, false, false},
		{"parent new minor", "CURRENT", `"store_format_minor":9,`, false, true},
		{"parent new runtime", "runtime-state.json", `"runtime_state_version":4,`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeVibeUnifiedSession(t, root, vibeUnifiedSessionID)
			parseDir := dir
			if tc.parent {
				writeSourceFile(t, filepath.Join(dir, "generations", "0000000000000001", "runtime-state.json"), `{"runtime_state_version":3,"identity":{"parent_session_id":"parent"}}`)
				dir = writeVibeUnifiedSession(t, root, "parent")
			}
			path := filepath.Join(dir, "generations", "0000000000000001", tc.file)
			if tc.file == "CURRENT" {
				path = filepath.Join(dir, tc.file)
			}
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(raw)
			for _, key := range []string{`"store_format":"mistral.vibe.unified-session-store/v1"`, `"store_format_minor":8`, `"manifest_version":1`, `"projection_state_version":1`, `"runtime_state_version":3`} {
				content = strings.ReplaceAll(strings.ReplaceAll(content, key+",", ""), ","+key, "")
			}
			writeSourceFile(t, path, "{"+tc.prefix+content[1:])
			if tc.file == "manifest.json" {
				writeVibeUnifiedPointer(t, dir, "0000000000000001")
			}
			_, err = parseVibeUnifiedResultFile(filepath.Join(parseDir, "CURRENT"), FileInfo{})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errVibeUnsupportedVersion)
			}
		})
	}
}
