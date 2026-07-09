package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecallEvidenceWindowBuildsCanonicalHostAuthorization(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "window-session", 10, "source-a", "")

	window, err := d.BuildRecallEvidenceWindow(
		context.Background(), "window-session", 10, 12,
	)

	require.NoError(t, err)
	assert.Equal(t, "window-session", window.SessionID)
	assert.Equal(t, 10, window.MessageStartOrdinal)
	assert.Equal(t, 12, window.MessageEndOrdinal)
	assert.Equal(t, []string{"tool-a", "tool-z"}, window.AllowedToolUseIDs)
	assert.Equal(
		t,
		"fb664625551c1d233bd42c85348c4ea387f59313f7ec76bdbcec24c392bc12ae",
		window.AuthorizationDigest,
	)
	require.Len(t, window.Messages, 3)
	assert.Equal(t, []int{10, 11, 12}, []int{
		window.Messages[0].Ordinal,
		window.Messages[1].Ordinal,
		window.Messages[2].Ordinal,
	})
	assert.Equal(t, "source-a-10", window.Messages[0].SourceUUID)
	assert.Equal(t, "user", window.Messages[0].Role)
	assert.Equal(t, "Run the formatter.", window.Messages[0].Content)
	require.Len(t, window.Messages[1].ToolCalls, 2)
	assert.Equal(t, "tool-z", window.Messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "Write", window.Messages[1].ToolCalls[0].ToolName)
	assert.Equal(t, `{"path":"b.go"}`, window.Messages[1].ToolCalls[0].InputJSON)
	assert.Equal(t, "updated b.go", window.Messages[1].ToolCalls[0].ResultContent)
	assert.Equal(t, "tool-a", window.Messages[1].ToolCalls[1].ToolUseID)
}

func TestRecallEvidenceWindowRejectsIncompleteAndReversedRanges(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "gapped", "agentsview")
	insertMessages(t, d,
		recallEvidenceMessage("gapped", 3, "user", "first", "uuid-3"),
		recallEvidenceMessage("gapped", 5, "assistant", "third", "uuid-5"),
	)

	_, err := d.BuildRecallEvidenceWindow(context.Background(), "gapped", 3, 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing ordinal 4")

	_, err = d.BuildRecallEvidenceWindow(context.Background(), "gapped", 5, 3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence window range")

	_, err = d.BuildRecallEvidenceWindow(context.Background(), "missing", 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing ordinal 0")
}

func TestRecallEvidenceWindowBindsContainedSelection(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "window-session", 10, "source-a", "")
	window, err := d.BuildRecallEvidenceWindow(
		context.Background(), "window-session", 10, 12,
	)
	require.NoError(t, err)

	metadata, err := window.BindSelection(RecallEvidenceSelection{
		MessageStartOrdinal: 11,
		MessageEndOrdinal:   12,
		ToolUseIDs:          []string{"tool-z", "tool-z"},
	})

	require.NoError(t, err)
	assert.Equal(t, "source-a-11", metadata.MessageStartSourceUUID)
	assert.Equal(t, "source-a-12", metadata.MessageEndSourceUUID)
	assert.Equal(t, []string{"tool-z"}, metadata.ToolUseIDs)
	assert.Len(t, metadata.ContentDigest, 64)
}

func TestRecallEvidenceWindowRejectsSelectionOutsideAuthorization(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "window-session", 10, "source-a", "")
	window, err := d.BuildRecallEvidenceWindow(
		context.Background(), "window-session", 10, 12,
	)
	require.NoError(t, err)

	tests := []struct {
		name      string
		selection RecallEvidenceSelection
		want      string
	}{
		{
			name: "start before window",
			selection: RecallEvidenceSelection{
				MessageStartOrdinal: 9,
				MessageEndOrdinal:   11,
			},
			want: "outside authorized window",
		},
		{
			name: "end after window",
			selection: RecallEvidenceSelection{
				MessageStartOrdinal: 11,
				MessageEndOrdinal:   13,
			},
			want: "outside authorized window",
		},
		{
			name: "reversed",
			selection: RecallEvidenceSelection{
				MessageStartOrdinal: 12,
				MessageEndOrdinal:   11,
			},
			want: "invalid evidence selection range",
		},
		{
			name: "unknown tool",
			selection: RecallEvidenceSelection{
				MessageStartOrdinal: 10,
				MessageEndOrdinal:   12,
				ToolUseIDs:          []string{"tool-missing"},
			},
			want: "tool use tool-missing is outside authorized window",
		},
		{
			name: "tool outside narrowed selection",
			selection: RecallEvidenceSelection{
				MessageStartOrdinal: 10,
				MessageEndOrdinal:   10,
				ToolUseIDs:          []string{"tool-a"},
			},
			want: "tool use tool-a is outside selected messages",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := window.BindSelection(tt.selection)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRecallEvidenceWindowContentDigestIgnoresCoordinatesAndStorageMetadata(
	t *testing.T,
) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "first", 10, "source-a", "")
	seedRecallEvidenceWindow(t, d, "shifted", 20, "source-b", "")
	seedRecallEvidenceWindow(t, d, "changed-content", 30, "source-c", "changed")
	seedRecallEvidenceWindow(t, d, "changed-tool", 40, "source-d", "tool")

	digests := make(map[string]string)
	authorizations := make(map[string]string)
	for _, tc := range []struct {
		session string
		start   int
	}{
		{session: "first", start: 10},
		{session: "shifted", start: 20},
		{session: "changed-content", start: 30},
		{session: "changed-tool", start: 40},
	} {
		window, err := d.BuildRecallEvidenceWindow(
			context.Background(), tc.session, tc.start, tc.start+2,
		)
		require.NoError(t, err, tc.session)
		metadata, err := window.BindSelection(RecallEvidenceSelection{
			MessageStartOrdinal: tc.start,
			MessageEndOrdinal:   tc.start + 1,
			ToolUseIDs:          []string{"tool-a"},
		})
		require.NoError(t, err, tc.session)
		digests[tc.session] = metadata.ContentDigest
		authorizations[tc.session] = window.AuthorizationDigest
	}

	assert.Equal(t, digests["first"], digests["shifted"])
	assert.NotEqual(t, authorizations["first"], authorizations["shifted"])
	assert.NotEqual(t, digests["first"], digests["changed-content"])
	assert.NotEqual(t, digests["first"], digests["changed-tool"])
}

func TestMigrationRecallEvidenceMetadataPreservesLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recall-evidence.db")
	d, err := Open(path)
	require.NoError(t, err)
	insertSession(t, d, "s1", "agentsview")
	_, err = d.InsertRecallEntry(RecallEntry{
		ID:              "m1",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Legacy evidence",
		Body:            "Evidence survives additive metadata migration.",
		SourceSessionID: "s1",
		Evidence: []RecallEvidence{{
			SessionID:           "s1",
			MessageStartOrdinal: 3,
			MessageEndOrdinal:   4,
			ToolUseID:           "tool-a",
			Snippet:             "legacy snippet",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	for _, column := range []string{
		"message_start_source_uuid",
		"message_end_source_uuid",
		"content_digest",
	} {
		_, err = conn.Exec("ALTER TABLE recall_evidence DROP COLUMN " + column)
		require.NoError(t, err, column)
	}
	require.NoError(t, conn.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	got, err := reopened.GetRecallEntry(context.Background(), "m1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "legacy snippet", got.Evidence[0].Snippet)
	assert.Equal(t, "tool-a", got.Evidence[0].ToolUseID)
	assert.Empty(t, got.Evidence[0].MessageStartSourceUUID)
	assert.Empty(t, got.Evidence[0].MessageEndSourceUUID)
	assert.Empty(t, got.Evidence[0].ContentDigest)
}

func seedRecallEvidenceWindow(
	t *testing.T,
	d *DB,
	sessionID string,
	start int,
	sourcePrefix string,
	mutation string,
) {
	t.Helper()
	insertSession(t, d, sessionID, "agentsview")
	firstContent := "Run the formatter."
	toolAInput := `{"path":"a.go"}`
	if mutation == "changed" {
		firstContent = "Run a different formatter."
	}
	if mutation == "tool" {
		toolAInput = `{"path":"changed.go"}`
	}
	second := recallEvidenceMessage(
		sessionID, start+1, "assistant", "I will inspect the files.",
		sourcePrefix+"-"+ordinalString(start+1),
	)
	second.ToolCalls = []ToolCall{
		{
			ToolName:            "Write",
			Category:            "Edit",
			ToolUseID:           "tool-z",
			InputJSON:           `{"path":"b.go"}`,
			SkillName:           "editing",
			ResultContentLength: len("updated b.go"),
			ResultContent:       "updated b.go",
			SubagentSessionID:   "subagent-1",
		},
		{
			ToolName:            "Read",
			Category:            "Read",
			ToolUseID:           "tool-a",
			InputJSON:           toolAInput,
			ResultContentLength: len("package main"),
			ResultContent:       "package main",
		},
	}
	third := recallEvidenceMessage(
		sessionID, start+2, "assistant", "The formatter passed.",
		sourcePrefix+"-"+ordinalString(start+2),
	)
	third.ToolCalls = []ToolCall{{
		ToolName:  "Write",
		Category:  "Edit",
		ToolUseID: "tool-z",
		InputJSON: `{"path":"b.go","mode":"verify"}`,
	}}
	insertMessages(t, d,
		recallEvidenceMessage(
			sessionID, start, "user", firstContent,
			sourcePrefix+"-"+ordinalString(start),
		),
		second,
		third,
	)
}

func recallEvidenceMessage(
	sessionID string,
	ordinal int,
	role string,
	content string,
	sourceUUID string,
) Message {
	return Message{
		SessionID:     sessionID,
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     "2026-07-09T12:00:00Z",
		SourceUUID:    sourceUUID,
	}
}

func ordinalString(ordinal int) string {
	return strconv.Itoa(ordinal)
}
