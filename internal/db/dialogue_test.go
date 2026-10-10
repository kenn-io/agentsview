package db

import (
	"bytes"
	"database/sql"
	"encoding/gob"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestDeriveMessageText(t *testing.T) {
	bash := ToolCall{
		ToolName: "Bash", Category: "Bash",
		InputJSON: `{"command":"grep -rn quokka ."}`,
	}
	tests := []struct {
		name         string
		msg          Message
		wantDialogue *string
		wantThinking string
	}{{
		name: "plain text",
		msg:  Message{Content: "Found the bug."},
	}, {
		name: "recorded thinking",
		msg: Message{
			Content:      "[Thinking]\nfirst\n[/Thinking]\nFound it.\n[Thinking]\nsecond\n[/Thinking]",
			ThinkingText: "first\n\nsecond", HasThinking: true,
		},
		wantDialogue: new("Found it."), wantThinking: "first\n\nsecond",
	}, {
		name: "inline-only thinking",
		msg: Message{
			Content:     "[Thinking]\nweighing zanzibar\n[/Thinking]\nDone.",
			HasThinking: true,
		},
		wantDialogue: new("Done."), wantThinking: "weighing zanzibar",
	}, {
		name: "rendering regenerated from stored input",
		msg: Message{
			Content:   "Checking.\n[Bash]\n$ grep -rn quokka .\nDone.",
			ToolCalls: []ToolCall{bash},
		},
		wantDialogue: new("Checking.\nDone."),
	}, {
		name: "parse-time rendering",
		msg: Message{
			Content:   "Done.\n[Pi tool]",
			ToolCalls: []ToolCall{{ToolName: "pi_tool", Rendering: "[Pi tool]"}},
		},
		wantDialogue: new("Done."),
	}, {
		name:         "message that is only a tool call",
		msg:          Message{Content: "[Bash]\n$ grep -rn quokka .", ToolCalls: []ToolCall{bash}},
		wantDialogue: new(""),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deriveMessageText(&tt.msg, "")
			assert.Equal(t, tt.wantDialogue, tt.msg.DialogueText)
			assert.Equal(t, tt.wantThinking, tt.msg.ThinkingText)
		})
	}
}

func TestCopyOrphanedDataFromFillsDialogue(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "gone", "proj")
	reply := asstMsg("gone", 0, "[Thinking]\nplan\n[/Thinking]\nDone.\n[Bash]\n$ ls")
	reply.HasThinking, reply.HasToolUse = true, true
	reply.ToolCalls = []ToolCall{{
		ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"ls"}`,
	}}
	insertMessages(t, srcDB, reply)
	// Rows written before data version 129 have neither field.
	_, err := srcDB.getWriter().Exec(t.Context(),
		"UPDATE messages SET dialogue_text = NULL, thinking_text = ''")
	require.NoError(t, err)
	_, err = srcDB.getWriter().Exec(t.Context(), "PRAGMA user_version = 126")
	require.NoError(t, err)
	srcDB.Close()

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	var dialogue sql.NullString
	var thinking string
	require.NoError(t, dstDB.getReader().QueryRow(t.Context(),
		"SELECT dialogue_text, thinking_text FROM messages WHERE session_id = 'gone'",
	).Scan(&dialogue, &thinking))
	assert.Equal(t, sql.NullString{String: "Done.", Valid: true}, dialogue)
	assert.Equal(t, "plan", thinking)
}

func TestStoragePoliciesProjectDialogue(t *testing.T) {
	msg := Message{
		Role: "assistant", Model: "claude", TokenUsage: []byte(`{"input_tokens":1}`),
		Content:     "[Thinking]\nplan\n[/Thinking]\nprivate reply\n[Bash]\n$ cat secret.txt",
		HasThinking: true, HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash",
			InputJSON: `{"command":"cat secret.txt"}`,
			Rendering: "[Bash]\n$ cat secret.txt",
		}},
	}
	_, usage := ProjectSessionForStoragePolicy(Session{}, []Message{msg}, config.ArchiveContentUsage)
	require.Len(t, usage, 1)
	assert.Nil(t, usage[0].DialogueText)

	_, transcripts := ProjectSessionForStoragePolicy(Session{}, []Message{msg}, config.ArchiveContentTranscripts)
	require.Len(t, transcripts, 1)
	assert.Equal(t, new("private reply"), transcripts[0].DialogueText)
}

func TestCopyOrphanedDataIntoTranscriptsArchiveKeepsPathsOutOfDialogue(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "gone", "proj")
	reply := asstMsg("gone", 0, "Found it.\n[Read: /a/walrus.md]")
	reply.HasToolUse = true
	reply.ToolCalls = []ToolCall{{
		ToolName: "Read", Category: "Read",
		InputJSON: `{"file_path":"/a/walrus.md"}`, FilePath: "/a/walrus.md",
	}}
	insertMessages(t, srcDB, reply)
	srcDB.Close()

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	dstDB.SetArchiveContent(config.ArchiveContentTranscripts)
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	page, err := dstDB.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "walrus", Sources: []string{"messages"}, IncludeOneShot: true,
	})
	require.NoError(t, err)
	assert.Empty(t, page.Matches)
}

func assertMessagesMiss(t *testing.T, d *DB, pattern string) {
	t.Helper()
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: pattern, Sources: []string{"messages"}, IncludeOneShot: true,
	})
	require.NoError(t, err)
	assert.Empty(t, page.Matches)
}

func TestStagedPublishDerivesDialogue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "codex:s", "proj", func(s *Session) { s.Agent = "codex" })
	msgs := []Message{{
		SessionID: "codex:s", Role: "assistant", HasToolUse: true,
		Content: "Checking.\n[Bash]\n$ cat quokka.txt",
		ToolCalls: []ToolCall{{
			ToolUseID: "call_a", ToolName: "exec_command", Category: "Bash",
			InputJSON: `{"cmd":"cat quokka.txt"}`, Rendering: "[Bash]\n$ cat quokka.txt",
		}},
	}}
	require.NoError(t, d.ReplaceSessionContentStaged(
		t.Context(), "codex:s", msgs, newScratchStagedResults(t), map[string]bool{},
		func(map[string]bool) (SessionSignalUpdate, []SecretFinding, error) {
			return SessionSignalUpdate{}, nil, nil
		},
	))
	assertMessagesMiss(t, d, "quokka")
}

func TestReplaceSessionMessagesFillsNullDialogue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "proj")
	reply := asstMsg("s", 0, "Done.\n[Bash]\n$ cat quokka.txt")
	reply.HasToolUse = true
	reply.ToolCalls = []ToolCall{{
		ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"cat quokka.txt"}`,
	}}
	insertMessages(t, d, reply)
	_, err := d.getWriter().Exec(t.Context(), "UPDATE messages SET dialogue_text = NULL")
	require.NoError(t, err)

	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s", []Message{reply}))
	assertMessagesMiss(t, d, "quokka")
}

func TestTranscriptsProjectionKeepsDialogueWhenRepeated(t *testing.T) {
	msg := Message{
		Role: "assistant", Model: "claude", TokenUsage: []byte(`{"input_tokens":1}`),
		Content: "Looking.\n[Glob: *.go in /workspace/walrus]", HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Glob", Category: "Glob",
			InputJSON: `{"pattern":"*.go","path":"/workspace/walrus"}`,
			Rendering: "[Glob: *.go in /workspace/walrus]",
		}},
	}
	_, once := ProjectSessionForStoragePolicy(Session{}, []Message{msg}, config.ArchiveContentTranscripts)
	_, twice := ProjectSessionForStoragePolicy(Session{}, once, config.ArchiveContentTranscripts)
	require.Len(t, twice, 1)
	assert.Equal(t, new("Looking."), twice[0].DialogueText)
}

func TestCopyPreDialogueCodexOrphanIntoTranscriptsArchive(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "codex:gone", "proj", func(s *Session) { s.Agent = "codex" })
	reply := asstMsg("codex:gone", 0, "Inspecting files.\n[Tool: inspect]\ninspect credentials.txt")
	reply.HasToolUse = true
	reply.ToolCalls = []ToolCall{{
		ToolName: "inspect", Category: "Other", InputJSON: `{"path":"credentials.txt"}`,
	}}
	insertMessages(t, srcDB, reply)
	_, err := srcDB.getWriter().Exec(t.Context(), "UPDATE messages SET dialogue_text = NULL")
	require.NoError(t, err)
	_, err = srcDB.getWriter().Exec(t.Context(), "PRAGMA user_version = 126")
	require.NoError(t, err)
	srcDB.Close()

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	dstDB.SetArchiveContent(config.ArchiveContentTranscripts)
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	assertMessagesMiss(t, dstDB, "credentials")
}

func TestTranscriptsDialogueSurvivesPayloadRoundTrip(t *testing.T) {
	msg := Message{
		Role: "assistant", Model: "claude", TokenUsage: []byte(`{"input_tokens":1}`),
		Content: "[Read: /tmp/p/walrus.md]", HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Read", Category: "Read", FilePath: "/tmp/p/walrus.md",
			InputJSON: `{"file_path":"/tmp/p/walrus.md"}`,
			Rendering: "[Read: /tmp/p/walrus.md]",
		}},
	}
	_, projected := ProjectSessionForStoragePolicy(Session{}, []Message{msg}, config.ArchiveContentTranscripts)
	// Hosted payloads are gob encoded, which turns empty dialogue into nil.
	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(projected))
	var decoded []Message
	require.NoError(t, gob.NewDecoder(&buf).Decode(&decoded))
	require.Nil(t, decoded[0].DialogueText)

	derived := DeriveSearchText(decoded)
	assert.Equal(t, new(""), derived[0].DialogueText)
}

func TestCopyOrphanedDataFromFillsClaudeAIThinking(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	insertSession(t, srcDB, "claude-ai:gone", "proj", func(s *Session) { s.Agent = "claude-ai" })
	insertMessages(t, srcDB, asstMsg("claude-ai:gone", 0, "[Thinking]\nzanzibar\n[/Thinking]\nDone."))
	// Imports before data version 129 never set has_thinking.
	_, err := srcDB.getWriter().Exec(t.Context(),
		"UPDATE messages SET dialogue_text = NULL, thinking_text = '', has_thinking = 0")
	require.NoError(t, err)
	_, err = srcDB.getWriter().Exec(t.Context(), "PRAGMA user_version = 126")
	require.NoError(t, err)
	srcDB.Close()

	dstDB := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dstDB.Close()
	_, err = dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err)
	assertMessagesMiss(t, dstDB, "zanzibar")
	page, err := dstDB.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "zanzibar", Sources: []string{"thinking"}, IncludeOneShot: true,
	})
	require.NoError(t, err)
	assert.Len(t, page.Matches, 1)
}
