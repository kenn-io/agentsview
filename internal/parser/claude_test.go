package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMetadataLine builds a single Claude JSONL line with all
// metadata fields used by the metadata extraction tests.
func buildMetadataLine(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestClaudeSessionIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "identity.jsonl")
	content := strings.Join([]string{
		`{"type":"agent-setting","agentSetting":" ","entrypoint":" "}`,
		`{"type":"agent-setting","agentSetting":"triage","entrypoint":"sdk-cli"}`,
		`{"type":"user","sessionId":"identity-shaped-noise","uuid":"u1","message":{"content":"hello"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "triage", results[0].Session.AgentLabel)
	assert.Equal(t, "sdk-cli", results[0].Session.Entrypoint)
	assert.Equal(t, AgentClaude, results[0].Session.Agent)
}

func TestClaudeSessionIdentityPreservesRawValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "identity-raw.jsonl")
	content := strings.Join([]string{
		`{"type":"agent-setting","agentSetting":"  Claude Code  ","entrypoint":"\tsdk-cli "}`,
		`{"type":"user","sessionId":"identity-shaped-noise","uuid":"u1","message":{"content":"hello"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "  Claude Code  ", results[0].Session.AgentLabel)
	assert.Equal(t, "\tsdk-cli ", results[0].Session.Entrypoint)
}

func TestClaudeSessionIdentityAbsent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "identity-absent.jsonl")
	content := strings.Join([]string{
		`{"type":"user","sessionId":"identity-shaped-noise","uuid":"u1","message":{"content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "", results[0].Session.AgentLabel)
	assert.Equal(t, "", results[0].Session.Entrypoint)
	assert.Equal(t, AgentClaude, results[0].Session.Agent)
}

func TestClaudeSessionKindAndPromptSource(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kind-prompt-source.jsonl")
	content := strings.Join([]string{
		`{"type":"user","sessionId":"kind-prompt-source","uuid":"u1","entrypoint":"cli","sessionKind":"bg","promptSource":"typed","message":{"content":"first"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","promptSource":"queued","message":{"content":"second"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	// sessionKind is a session-level field, first-non-empty-wins like
	// entrypoint.
	assert.Equal(t, "bg", results[0].Session.SessionKind)
	assert.Equal(t, "cli", results[0].Session.Entrypoint)

	// promptSource is captured per user turn.
	bySource := map[string]string{}
	for _, m := range results[0].Messages {
		if m.Role == RoleUser {
			bySource[m.Content] = m.PromptSource
		}
	}
	assert.Equal(t, "typed", bySource["first"])
	assert.Equal(t, "queued", bySource["second"])
}

func TestClaudeSessionKindAndPromptSourceAbsent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kind-prompt-source-absent.jsonl")
	// Older transcripts predate sessionKind/promptSource; both must
	// default to empty rather than a fabricated value.
	content := strings.Join([]string{
		`{"type":"user","sessionId":"kind-absent","uuid":"u1","message":{"content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, _, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "", results[0].Session.SessionKind)
	for _, m := range results[0].Messages {
		assert.Equal(t, "", m.PromptSource, "ordinal %d", m.Ordinal)
	}
}

func TestClaudeSessionIdentityLineage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lineage.jsonl")
	content := strings.Join([]string{
		`{"type":"agent-setting","agentSetting":"triage","entrypoint":"sdk-cli"}`,
		`{"type":"user","sessionId":"agent-setting-lineage","uuid":"u1","message":{"content":"identity-shaped-noise"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"content":[{"type":"text","text":"root-reply"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","message":{"content":"main-continue"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","message":{"content":[{"type":"text","text":"main-reply"}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"a2","message":{"content":"main-continue-2"}}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"u3","message":{"content":[{"type":"text","text":"main-reply-2"}]}}`,
		`{"type":"user","uuid":"u4","parentUuid":"a3","message":{"content":"main-continue-3"}}`,
		`{"type":"assistant","uuid":"a4","parentUuid":"u4","message":{"content":[{"type":"text","text":"main-reply-3"}]}}`,
		`{"type":"user","uuid":"u5","parentUuid":"a4","message":{"content":"main-continue-4"}}`,
		`{"type":"assistant","uuid":"a5","parentUuid":"u5","message":{"content":[{"type":"text","text":"main-reply-4"}]}}`,
		`{"type":"user","uuid":"fork-u1","parentUuid":"a1","message":{"content":"fork-question"}}`,
		`{"type":"assistant","uuid":"fork-a1","parentUuid":"fork-u1","message":{"content":[{"type":"text","text":"fork-reply"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	results, err := parseClaudeSession(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)
	forks := 0
	for _, result := range results {
		assert.Equal(t, "triage", result.Session.AgentLabel)
		assert.Equal(t, "sdk-cli", result.Session.Entrypoint)
		assert.Equal(t, "agent-setting-lineage", result.Session.SourceSessionID)
		if result.Session.RelationshipType == RelFork {
			forks++
		}
	}
	assert.Equal(t, 1, forks)
}

func TestParseClaudeSession_Metadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// lines builds JSONL content; last bool controls
		// trailing newline (true = well-formed, false = truncated).
		lines         []map[string]any
		badLines      []string // raw malformed lines to insert
		trailingLine  string   // if set, appended without newline
		wantSession   func(*testing.T, ParsedSession)
		wantMessages  func(*testing.T, []ParsedMessage)
		wantResultLen int // expected number of ParseResults
	}{
		{
			name: "session metadata extracted from JSONL",
			lines: []map[string]any{
				{
					"type":        "user",
					"timestamp":   tsZero,
					"sessionId":   "session-001",
					"version":     "1.0.42",
					"cwd":         "/home/user/project",
					"gitBranch":   "feat/cool-feature",
					"uuid":        "uuid-1",
					"parentUuid":  "",
					"isSidechain": false,
					"message": map[string]any{
						"content": "hello",
					},
				},
				{
					"type":        "assistant",
					"timestamp":   tsZeroS1,
					"sessionId":   "session-001",
					"uuid":        "uuid-2",
					"parentUuid":  "uuid-1",
					"isSidechain": false,
					"message": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "hi there"},
						},
					},
				},
			},
			wantResultLen: 1,
			wantSession: func(t *testing.T, s ParsedSession) {
				t.Helper()
				assert.Equal(t, "/home/user/project", s.Cwd)
				assert.Equal(t, "feat/cool-feature", s.GitBranch)
				assert.Equal(t, "session-001", s.SourceSessionID)
				assert.Equal(t, "1.0.42", s.SourceVersion)
				assert.Equal(t, 0, s.MalformedLines)
				assert.False(t, s.IsTruncated)
			},
			wantMessages: func(t *testing.T, msgs []ParsedMessage) {
				t.Helper()
				require.Len(t, msgs, 2)

				assert.Equal(t, "user", msgs[0].SourceType)
				assert.Equal(t, "uuid-1", msgs[0].SourceUUID)
				assert.Equal(t, "", msgs[0].SourceParentUUID)
				assert.False(t, msgs[0].IsSidechain)

				assert.Equal(t, "assistant", msgs[1].SourceType)
				assert.Equal(t, "uuid-2", msgs[1].SourceUUID)
				assert.Equal(t, "uuid-1", msgs[1].SourceParentUUID)
				assert.False(t, msgs[1].IsSidechain)
			},
		},
		{
			name: "sidechain flag carried through",
			lines: []map[string]any{
				{
					"type":        "user",
					"timestamp":   tsZero,
					"uuid":        "u1",
					"parentUuid":  "",
					"isSidechain": true,
					"message": map[string]any{
						"content": "sidechain msg",
					},
				},
				{
					"type":        "assistant",
					"timestamp":   tsZeroS1,
					"uuid":        "u2",
					"parentUuid":  "u1",
					"isSidechain": true,
					"message": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "reply"},
						},
					},
				},
			},
			wantResultLen: 1,
			wantMessages: func(t *testing.T, msgs []ParsedMessage) {
				t.Helper()
				require.Len(t, msgs, 2)
				assert.True(t, msgs[0].IsSidechain)
				assert.True(t, msgs[1].IsSidechain)
			},
		},
		{
			name: "malformed lines counted",
			lines: []map[string]any{
				{
					"type":      "user",
					"timestamp": tsZero,
					"message": map[string]any{
						"content": "hello",
					},
				},
			},
			badLines:      []string{"not valid json", "{also bad"},
			wantResultLen: 1,
			wantSession: func(t *testing.T, s ParsedSession) {
				t.Helper()
				assert.Equal(t, 2, s.MalformedLines)
				assert.False(t, s.IsTruncated)
			},
		},
		{
			name: "truncation detected from bad last line",
			lines: []map[string]any{
				{
					"type":      "user",
					"timestamp": tsZero,
					"message": map[string]any{
						"content": "hello",
					},
				},
			},
			trailingLine:  `{"type":"user","trunca`,
			wantResultLen: 1,
			wantSession: func(t *testing.T, s ParsedSession) {
				t.Helper()
				assert.Equal(t, 1, s.MalformedLines)
				assert.True(t, s.IsTruncated)
			},
		},
		{
			name: "version from non-user line",
			lines: []map[string]any{
				{
					"type":      "system",
					"timestamp": tsZero,
					"version":   "2.0.0",
				},
				{
					"type":      "user",
					"timestamp": tsZeroS1,
					"message": map[string]any{
						"content": "hello",
					},
				},
			},
			wantResultLen: 1,
			wantSession: func(t *testing.T, s ParsedSession) {
				t.Helper()
				assert.Equal(t, "2.0.0", s.SourceVersion)
			},
		},
		{
			name: "cwd and gitBranch from first user entry only",
			lines: []map[string]any{
				{
					"type":      "user",
					"timestamp": tsZero,
					"cwd":       "/first/cwd",
					"gitBranch": "main",
					"message": map[string]any{
						"content": "first",
					},
				},
				{
					"type":      "user",
					"timestamp": tsZeroS1,
					"cwd":       "/second/cwd",
					"gitBranch": "develop",
					"message": map[string]any{
						"content": "second",
					},
				},
			},
			wantResultLen: 1,
			wantSession: func(t *testing.T, s ParsedSession) {
				t.Helper()
				assert.Equal(t, "/first/cwd", s.Cwd)
				assert.Equal(t, "main", s.GitBranch)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "test-meta.jsonl")

			var content strings.Builder
			for _, bad := range tc.badLines {
				content.WriteString(bad + "\n")
			}
			for _, m := range tc.lines {
				content.WriteString(buildMetadataLine(m) + "\n")
			}
			if tc.trailingLine != "" {
				content.WriteString(tc.trailingLine)
			}

			err := os.WriteFile(
				path, []byte(content.String()), 0o644,
			)
			require.NoError(t, err)

			results, err := parseClaudeSession(
				path, "proj", "local",
			)
			require.NoError(t, err)
			if tc.wantResultLen > 0 {
				require.Len(t, results, tc.wantResultLen)
			}

			if tc.wantSession != nil {
				tc.wantSession(t, results[0].Session)
			}
			if tc.wantMessages != nil {
				tc.wantMessages(t, results[0].Messages)
			}
		})
	}
}

func TestParseClaudeSession_MetadataOnForkSessions(
	t *testing.T,
) {
	t.Parallel()

	// Build a DAG with a large-gap fork to verify metadata
	// propagates to all fork sessions.
	dir := t.TempDir()
	path := filepath.Join(dir, "fork-meta.jsonl")

	var content strings.Builder
	base := map[string]any{
		"type":      "user",
		"timestamp": "2024-01-01T10:00:00Z",
		"sessionId": "sess-orig",
		"version":   "3.5.0",
		"cwd":       "/workspace",
		"gitBranch": "feat/forks",
		"uuid":      "a",
		"message": map[string]any{
			"content": "start",
		},
	}
	content.WriteString(buildMetadataLine(base) + "\n")

	// Main branch with enough user turns (>3) from fork point.
	mainMsgs := []map[string]any{
		{"type": "assistant", "timestamp": "2024-01-01T10:00:01Z", "uuid": "b", "parentUuid": "a", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}},
		{"type": "user", "timestamp": "2024-01-01T10:00:02Z", "uuid": "c", "parentUuid": "b", "message": map[string]any{"content": "q1"}},
		{"type": "assistant", "timestamp": "2024-01-01T10:00:03Z", "uuid": "d", "parentUuid": "c", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a1"}}}},
		{"type": "user", "timestamp": "2024-01-01T10:00:04Z", "uuid": "e", "parentUuid": "d", "message": map[string]any{"content": "q2"}},
		{"type": "assistant", "timestamp": "2024-01-01T10:00:05Z", "uuid": "f", "parentUuid": "e", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a2"}}}},
		{"type": "user", "timestamp": "2024-01-01T10:00:06Z", "uuid": "g", "parentUuid": "f", "message": map[string]any{"content": "q3"}},
		{"type": "assistant", "timestamp": "2024-01-01T10:00:07Z", "uuid": "h", "parentUuid": "g", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a3"}}}},
		{"type": "user", "timestamp": "2024-01-01T10:00:08Z", "uuid": "k", "parentUuid": "h", "message": map[string]any{"content": "q4"}},
		{"type": "assistant", "timestamp": "2024-01-01T10:00:09Z", "uuid": "l", "parentUuid": "k", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a4"}}}},
	}
	for _, m := range mainMsgs {
		content.WriteString(buildMetadataLine(m) + "\n")
	}

	// Fork from b
	forkMsgs := []map[string]any{
		{"type": "user", "timestamp": "2024-01-01T10:01:00Z", "uuid": "fork-u1", "parentUuid": "b", "message": map[string]any{"content": "forked question"}},
		{"type": "assistant", "timestamp": "2024-01-01T10:01:01Z", "uuid": "fork-a1", "parentUuid": "fork-u1", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "forked answer"}}}},
	}
	for _, m := range forkMsgs {
		content.WriteString(buildMetadataLine(m) + "\n")
	}

	err := os.WriteFile(path, []byte(content.String()), 0o644)
	require.NoError(t, err)

	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 2, "expected main + fork result")

	// Both sessions should carry the same source metadata.
	for i, r := range results {
		s := r.Session
		assert.Equal(t, "/workspace", s.Cwd,
			"result[%d] Cwd", i)
		assert.Equal(t, "feat/forks", s.GitBranch,
			"result[%d] GitBranch", i)
		assert.Equal(t, "sess-orig", s.SourceSessionID,
			"result[%d] SourceSessionID", i)
		assert.Equal(t, "3.5.0", s.SourceVersion,
			"result[%d] SourceVersion", i)
	}
}

func TestParseClaudeSession_LinearMetadata(t *testing.T) {
	t.Parallel()

	// Linear session (no uuids) should still carry metadata.
	dir := t.TempDir()
	path := filepath.Join(dir, "linear-meta.jsonl")

	content := buildMetadataLine(map[string]any{
		"type":      "user",
		"timestamp": tsZero,
		"sessionId": "lin-001",
		"version":   "1.2.3",
		"cwd":       "/tmp/linear",
		"gitBranch": "main",
		"message": map[string]any{
			"content": "linear hello",
		},
	}) + "\n" + buildMetadataLine(map[string]any{
		"type":      "assistant",
		"timestamp": tsZeroS1,
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "linear reply"},
			},
		},
	}) + "\n"

	err := os.WriteFile(path, []byte(content), 0o644)
	require.NoError(t, err)

	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)

	sess := results[0].Session
	assert.Equal(t, "/tmp/linear", sess.Cwd)
	assert.Equal(t, "main", sess.GitBranch)
	assert.Equal(t, "lin-001", sess.SourceSessionID)
	assert.Equal(t, "1.2.3", sess.SourceVersion)

	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].SourceType)
	assert.Equal(t, "assistant", msgs[1].SourceType)
}

func TestClaudeIncrementalRenameTriggersFullParse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rename-incremental.jsonl")

	renameLine := `{"type":"system","subtype":"local_command","content":"<command-name>/rename</command-name>\n<command-args>new</command-args>","timestamp":"2026-06-01T00:00:02Z","sessionId":"s1"}`
	err := os.WriteFile(path, []byte(renameLine+"\n"), 0o644)
	require.NoError(t, err)

	_, _, _, parseErr := callParseClaudeSessionFrom(path, 0, 0, "")
	require.Error(t, parseErr)
	assert.True(t, IsIncrementalFullParseFallback(parseErr))
}

func TestClaudeIncrementalSessionIdentityTriggersFullParse(t *testing.T) {
	t.Parallel()

	initial := buildMetadataLine(map[string]any{
		"type":      "user",
		"uuid":      "u1",
		"timestamp": tsEarly,
		"message": map[string]any{
			"content": "hello",
		},
	}) + "\n"
	path := createTestFile(t, "identity-incremental.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)

	appended := buildMetadataLine(map[string]any{
		"type":         "user",
		"uuid":         "u2",
		"parentUuid":   "u1",
		"timestamp":    tsLate,
		"agentSetting": "triage",
		"entrypoint":   "sdk-cli",
		"message": map[string]any{
			"content": "late identity",
		},
	}) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, parseErr := callParseClaudeSessionFrom(path, info.Size(), 1, "u1")
	require.Error(t, parseErr)
	assert.True(t, IsIncrementalFullParseFallback(parseErr))
}

func TestClaudeIncrementalStoredIdentityAppendStaysIncremental(t *testing.T) {
	t.Parallel()

	// Real Claude CLI transcripts carry a top-level entrypoint ("cli") on
	// most message lines. Once the stored session already has that
	// identity, ordinary appends must stay on the incremental path instead
	// of escalating every append to a full re-parse of the whole file.
	initial := buildMetadataLine(map[string]any{
		"type":       "user",
		"uuid":       "u1",
		"timestamp":  tsEarly,
		"entrypoint": "cli",
		"message": map[string]any{
			"content": "hello",
		},
	}) + "\n"
	path := createTestFile(t, "identity-stored-incremental.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)

	appended := buildMetadataLine(map[string]any{
		"type":       "user",
		"uuid":       "u2",
		"parentUuid": "u1",
		"timestamp":  tsLate,
		"entrypoint": "cli",
		"message": map[string]any{
			"content": "routine append",
		},
	}) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	msgs, _, _, _, parseErr := claudeParseSessionFrom(
		path, info.Size(), claudeIncrementalScan{
			startOrdinal:  1,
			lastEntryUUID: "u1",
			stored:        claudeStoredIdentity{entrypoint: "cli"},
		},
	)
	require.NoError(t, parseErr)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
}

func TestClaudeIncrementalNewIdentityFieldStillEscalates(t *testing.T) {
	t.Parallel()

	// The stored entrypoint is known, but agentSetting appears for the
	// first time in the append. First-non-empty-wins means the appended
	// value changes the stored session, so the full-parse fallback must
	// still fire for the not-yet-stored field.
	initial := buildMetadataLine(map[string]any{
		"type":       "user",
		"uuid":       "u1",
		"timestamp":  tsEarly,
		"entrypoint": "cli",
		"message": map[string]any{
			"content": "hello",
		},
	}) + "\n"
	path := createTestFile(t, "identity-new-field.jsonl", initial)

	info, err := os.Stat(path)
	require.NoError(t, err)

	appended := buildMetadataLine(map[string]any{
		"type":         "user",
		"uuid":         "u2",
		"parentUuid":   "u1",
		"timestamp":    tsLate,
		"entrypoint":   "cli",
		"agentSetting": "triage",
		"message": map[string]any{
			"content": "late label",
		},
	}) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, _, _, parseErr := claudeParseSessionFrom(
		path, info.Size(), claudeIncrementalScan{
			startOrdinal:  1,
			lastEntryUUID: "u1",
			stored:        claudeStoredIdentity{entrypoint: "cli"},
		},
	)
	require.Error(t, parseErr)
	assert.True(t, IsIncrementalFullParseFallback(parseErr))
}

func TestClaudeRenameSetsDisplayName(t *testing.T) {
	t.Parallel()

	// userLine is a minimal user message that causes the parser
	// to emit a session (without it, ParseClaudeSession returns
	// no results).
	userLine := map[string]any{
		"type":       "user",
		"uuid":       "u1",
		"parentUuid": "",
		"timestamp":  tsZero,
		"sessionId":  "rename-test",
		"cwd":        "/x",
		"message": map[string]any{
			"content": "hi",
		},
	}

	// renameLine builds a system/local_command line for /rename.
	renameLine := func(name string) map[string]any {
		return map[string]any{
			"type":      "system",
			"subtype":   "local_command",
			"content":   "<command-name>/rename</command-name>\n<command-args>" + name + "</command-args>",
			"timestamp": tsZeroS1,
			"sessionId": "rename-test",
		}
	}

	tests := []struct {
		name        string
		lines       []map[string]any
		wantDisplay string
	}{
		{
			name: "single rename",
			lines: []map[string]any{
				userLine,
				renameLine("My Session"),
			},
			wantDisplay: "My Session",
		},
		{
			name: "two renames last wins",
			lines: []map[string]any{
				userLine,
				renameLine("First"),
				renameLine("Second"),
			},
			wantDisplay: "Second",
		},
		{
			name: "rename then empty rename clears",
			lines: []map[string]any{
				userLine,
				renameLine("Named"),
				renameLine(""),
			},
			wantDisplay: "",
		},
		{
			name: "no rename",
			lines: []map[string]any{
				userLine,
			},
			wantDisplay: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "rename-session.jsonl")

			var sb strings.Builder
			for _, m := range tc.lines {
				sb.WriteString(buildMetadataLine(m) + "\n")
			}
			err := os.WriteFile(path, []byte(sb.String()), 0o644)
			require.NoError(t, err)

			results, err := parseClaudeSession(path, "proj", "local")
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tc.wantDisplay, results[0].Session.SessionName)
		})
	}
}
