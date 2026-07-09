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

func TestRecallEvidenceReplaceRemapsStableEndpoints(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*DB, string, []Message) error
	}{
		{
			name: "messages",
			replace: func(d *DB, sessionID string, messages []Message) error {
				return d.ReplaceSessionMessages(sessionID, messages)
			},
		},
		{
			name: "content",
			replace: func(d *DB, sessionID string, messages []Message) error {
				return d.ReplaceSessionContent(
					sessionID, messages, SessionSignalUpdate{}, nil,
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedRecallEvidenceWindow(t, d, "rewrite", 10, "stable", "")
			original := insertVerifiedRecallSelection(
				t, d, "m1", "rewrite", 10, 11, []string{"tool-a"},
			)
			shifted := shiftedRecallMessages(t, d, "rewrite", 1)

			err := tt.replace(d, "rewrite", shifted)

			require.NoError(t, err)
			got := requireRecallEntry(t, d, "m1")
			assert.True(t, got.ProvenanceOK)
			require.Len(t, got.Evidence, 1)
			assert.Equal(t, 11, got.Evidence[0].MessageStartOrdinal)
			assert.Equal(t, 12, got.Evidence[0].MessageEndOrdinal)
			assert.Equal(t, "stable-10", got.Evidence[0].MessageStartSourceUUID)
			assert.Equal(t, "stable-11", got.Evidence[0].MessageEndSourceUUID)
			assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
		})
	}
}

func TestRecallEvidenceDiffRevokesChangedContent(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "diff", 10, "stable", "")
	insertVerifiedRecallSelection(
		t, d, "m1", "diff", 10, 11, []string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "diff")
	require.NoError(t, err)
	messages[0].Content = "Run a different formatter."
	messages[0].ContentLength = len(messages[0].Content)

	err = d.ReplaceSessionMessages("diff", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "m1")
	assert.False(t, got.ProvenanceOK)
	assert.Equal(t, 10, got.Evidence[0].MessageStartOrdinal)
}

func TestRecallEvidenceReplaceKeepsOrdinalFallbackWhenDigestMatches(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "legacy", 10, "", "")
	original := insertVerifiedRecallSelection(
		t, d, "m1", "legacy", 10, 11, []string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "legacy")
	require.NoError(t, err)
	for i := range messages {
		messages[i].Timestamp = "2026-07-09T13:00:00Z"
	}

	err = d.ReplaceSessionMessages("legacy", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "m1")
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, 10, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, 11, got.Evidence[0].MessageEndOrdinal)
	assert.Empty(t, got.Evidence[0].MessageStartSourceUUID)
	assert.Empty(t, got.Evidence[0].MessageEndSourceUUID)
	assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
}

func TestRecallEvidenceDiffRevokesOrdinalFallbackWhenDigestChanges(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "legacy-change", 10, "", "")
	insertVerifiedRecallSelection(
		t, d, "m1", "legacy-change", 10, 11, []string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "legacy-change")
	require.NoError(t, err)
	messages[0].Content = "Changed content at the same legacy ordinal."
	messages[0].ContentLength = len(messages[0].Content)

	err = d.ReplaceSessionMessages("legacy-change", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "m1")
	assert.False(t, got.ProvenanceOK)
}

func TestRecallEvidenceReplaceRevokesMissingOrAmbiguousEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Message) []Message
	}{
		{
			name: "missing",
			mutate: func(messages []Message) []Message {
				return append(messages[:1], messages[2:]...)
			},
		},
		{
			name: "ambiguous",
			mutate: func(messages []Message) []Message {
				duplicate := messages[1]
				duplicate.ID = 0
				duplicate.Ordinal = 14
				duplicate.Role = "system"
				duplicate.Content = "Duplicate stable identity."
				duplicate.ContentLength = len(duplicate.Content)
				duplicate.ToolCalls = nil
				return append(messages, duplicate)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedRecallEvidenceWindow(t, d, "endpoint", 10, "stable", "")
			insertVerifiedRecallSelection(
				t, d, "m1", "endpoint", 10, 11, []string{"tool-a"},
			)
			shifted := shiftedRecallMessages(t, d, "endpoint", 1)
			shifted = tt.mutate(shifted)

			err := d.ReplaceSessionMessages("endpoint", shifted)

			require.NoError(t, err)
			got := requireRecallEntry(t, d, "m1")
			assert.False(t, got.ProvenanceOK)
		})
	}
}

func TestRecallEvidenceDiffRevokesMissingToolCall(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "missing-tool", 10, "stable", "")
	insertVerifiedRecallSelection(
		t, d, "m1", "missing-tool", 10, 11, []string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "missing-tool")
	require.NoError(t, err)
	require.Len(t, messages[1].ToolCalls, 2)
	messages[1].ToolCalls = messages[1].ToolCalls[:1]

	err = d.ReplaceSessionMessages("missing-tool", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "m1")
	assert.False(t, got.ProvenanceOK)
}

func TestRecallEvidenceAppendPreservesMetadata(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "append", 10, "stable", "")
	original := insertVerifiedRecallSelection(
		t, d, "m1", "append", 10, 11, []string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "append")
	require.NoError(t, err)
	messages = append(messages, recallEvidenceMessage(
		"append", 13, "user", "One more question.", "stable-13",
	))

	err = d.ReplaceSessionMessages("append", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "m1")
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, 10, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, 11, got.Evidence[0].MessageEndOrdinal)
	assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
}

func TestRecallEvidenceReplaceRollbackOnReconcileFailure(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "rollback", 10, "stable", "")
	original := insertVerifiedRecallSelection(
		t, d, "m1", "rollback", 10, 11, []string{"tool-a"},
	)
	_, err := d.getWriter().Exec(`
		CREATE TRIGGER fail_recall_evidence_reconcile
		BEFORE UPDATE ON recall_evidence
		BEGIN
			SELECT RAISE(ABORT, 'forced reconciliation failure');
		END`)
	require.NoError(t, err)
	shifted := shiftedRecallMessages(t, d, "rollback", 1)

	err = d.ReplaceSessionMessages("rollback", shifted)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced reconciliation failure")
	messages, readErr := d.GetAllMessages(context.Background(), "rollback")
	require.NoError(t, readErr)
	require.Len(t, messages, 3)
	assert.Equal(t, 10, messages[0].Ordinal)
	assert.Equal(t, "Run the formatter.", messages[0].Content)
	got := requireRecallEntry(t, d, "m1")
	assert.True(t, got.ProvenanceOK)
	assert.Equal(t, 10, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
}

func TestRecallEvidenceWriteSessionBatchRemapsStableEndpoints(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "batch", 10, "stable", "")
	insertVerifiedRecallSelection(
		t, d, "m1", "batch", 10, 11, []string{"tool-a"},
	)
	shifted := shiftedRecallMessages(t, d, "batch", 1)
	session, err := d.GetSession(context.Background(), "batch")
	require.NoError(t, err)
	require.NotNil(t, session)

	result, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session:         *session,
		Messages:        shifted,
		ReplaceMessages: true,
	}})

	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	got := requireRecallEntry(t, d, "m1")
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, 11, got.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, 12, got.Evidence[0].MessageEndOrdinal)
}

func TestMigrationRecallEvidenceMetadataFingerprintsVerifiedLegacyRows(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "legacy-recall-evidence.db")
	d, err := Open(path)
	require.NoError(t, err)
	seedRecallEvidenceWindow(t, d, "legacy", 10, "stable", "")
	_, err = d.InsertRecallEntry(RecallEntry{
		ID:              "m1",
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Legacy verified recall",
		Body:            "This row predates evidence fingerprints.",
		SourceSessionID: "legacy",
		Transferable:    true,
		ProvenanceOK:    true,
		Evidence: []RecallEvidence{{
			SessionID:           "legacy",
			MessageStartOrdinal: 10,
			MessageEndOrdinal:   11,
			ToolUseID:           "tool-a",
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
	got := requireRecallEntry(t, reopened, "m1")
	assert.True(t, got.ProvenanceOK)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, "stable-10", got.Evidence[0].MessageStartSourceUUID)
	assert.Equal(t, "stable-11", got.Evidence[0].MessageEndSourceUUID)
	assert.Len(t, got.Evidence[0].ContentDigest, 64)
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
		recallEvidenceSourceUUID(sourcePrefix, start+1),
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
		recallEvidenceSourceUUID(sourcePrefix, start+2),
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
			recallEvidenceSourceUUID(sourcePrefix, start),
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

func recallEvidenceSourceUUID(prefix string, ordinal int) string {
	if prefix == "" {
		return ""
	}
	return prefix + "-" + ordinalString(ordinal)
}

func insertVerifiedRecallSelection(
	t *testing.T,
	d *DB,
	recallID string,
	sessionID string,
	start int,
	end int,
	toolUseIDs []string,
) RecallEvidenceSelectionMetadata {
	t.Helper()
	window, err := d.BuildRecallEvidenceWindow(
		context.Background(), sessionID, start, end,
	)
	require.NoError(t, err)
	metadata, err := window.BindSelection(RecallEvidenceSelection{
		MessageStartOrdinal: start,
		MessageEndOrdinal:   end,
		ToolUseIDs:          toolUseIDs,
	})
	require.NoError(t, err)
	evidence := make([]RecallEvidence, 0, max(1, len(metadata.ToolUseIDs)))
	if len(metadata.ToolUseIDs) == 0 {
		evidence = append(evidence, RecallEvidence{})
	} else {
		for _, toolUseID := range metadata.ToolUseIDs {
			evidence = append(evidence, RecallEvidence{ToolUseID: toolUseID})
		}
	}
	for i := range evidence {
		evidence[i].SessionID = sessionID
		evidence[i].MessageStartOrdinal = start
		evidence[i].MessageEndOrdinal = end
		evidence[i].MessageStartSourceUUID = metadata.MessageStartSourceUUID
		evidence[i].MessageEndSourceUUID = metadata.MessageEndSourceUUID
		evidence[i].ContentDigest = metadata.ContentDigest
	}
	_, err = d.InsertRecallEntry(RecallEntry{
		ID:              recallID,
		Type:            "fact",
		Scope:           "project",
		Status:          "accepted",
		Title:           "Verified recall " + recallID,
		Body:            "This recall is bound to host-built evidence.",
		SourceSessionID: sessionID,
		Transferable:    true,
		ProvenanceOK:    true,
		Evidence:        evidence,
	})
	require.NoError(t, err)
	return metadata
}

func shiftedRecallMessages(
	t *testing.T,
	d *DB,
	sessionID string,
	shift int,
) []Message {
	t.Helper()
	messages, err := d.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	for i := range messages {
		messages[i].ID = 0
		messages[i].Ordinal += shift
	}
	prefix := make([]Message, 0, shift+len(messages))
	for ordinal := range shift {
		prefix = append(prefix, recallEvidenceMessage(
			sessionID,
			10+ordinal,
			"system",
			"Inserted transcript metadata.",
			"inserted-"+ordinalString(ordinal),
		))
	}
	return append(prefix, messages...)
}

func requireRecallEntry(t *testing.T, d *DB, id string) *RecallEntry {
	t.Helper()
	entry, err := d.GetRecallEntry(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, entry)
	return entry
}
