package db

import (
	"bytes"
	"context"
	"log"
	"strconv"
	"strings"
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

func TestRecallEvidenceSourceUUIDLookupUsesPartialIndex(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "query-plan", 10, "stable", "")

	tx, err := d.getReader().BeginTx(
		context.Background(),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	rows, err := tx.QueryContext(
		context.Background(),
		"EXPLAIN QUERY PLAN "+recallEvidenceOrdinalBySourceUUIDSQL,
		"query-plan",
		"stable-10",
	)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id int
		var parent int
		var unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	usesSourceUUIDIndex := false
	for _, detail := range details {
		if strings.Contains(detail, "SEARCH messages") &&
			strings.Contains(detail, "idx_messages_source_uuid") {
			usesSourceUUIDIndex = true
			break
		}
	}
	assert.True(
		t,
		usesSourceUUIDIndex,
		"query plan must search via idx_messages_source_uuid: %v",
		details,
	)
}

func TestRecallEvidenceReplaceResolvesMixedEndpointsIndependently(t *testing.T) {
	tests := []struct {
		name          string
		anchoredStart bool
		wantReason    string
	}{
		{
			name:          "stable start and legacy end",
			anchoredStart: true,
			wantReason:    "start_endpoint_unresolved",
		},
		{
			name:          "legacy start and stable end",
			anchoredStart: false,
			wantReason:    "end_endpoint_unresolved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedRecallEvidenceWindow(t, d, "mixed", 10, "stable", "")
			original := insertVerifiedRecallSelection(
				t, d, "m1", "mixed", 10, 11, []string{"tool-a"},
			)

			// A legacy endpoint has no stable identity, so its stored ordinal
			// already names the post-rewrite boundary. The digest still binds
			// the same selected content because coordinates are intentionally
			// excluded from it.
			var updateEvidence string
			if tt.anchoredStart {
				updateEvidence = `
					UPDATE recall_evidence
					SET message_end_ordinal = 12,
					    message_end_source_uuid = ''
					WHERE entry_id = 'm1'`
			} else {
				updateEvidence = `
					UPDATE recall_evidence
					SET message_start_ordinal = 11,
					    message_start_source_uuid = ''
					WHERE entry_id = 'm1'`
			}
			result, err := d.getWriter().Exec(updateEvidence)
			require.NoError(t, err)
			rows, err := result.RowsAffected()
			require.NoError(t, err)
			require.EqualValues(t, 1, rows)

			shifted := shiftedRecallMessages(t, d, "mixed", 1)
			if tt.anchoredStart {
				shifted[2].SourceUUID = ""
			} else {
				shifted[1].SourceUUID = ""
			}
			err = d.ReplaceSessionMessages("mixed", shifted)
			require.NoError(t, err)

			got := requireRecallEntry(t, d, "m1")
			assert.True(t, got.ProvenanceOK)
			require.Len(t, got.Evidence, 1)
			assert.Equal(t, 11, got.Evidence[0].MessageStartOrdinal)
			assert.Equal(t, 12, got.Evidence[0].MessageEndOrdinal)
			assert.Equal(t, original.ContentDigest, got.Evidence[0].ContentDigest)
			if tt.anchoredStart {
				assert.Equal(t, "stable-10", got.Evidence[0].MessageStartSourceUUID)
				assert.Empty(t, got.Evidence[0].MessageEndSourceUUID)
			} else {
				assert.Empty(t, got.Evidence[0].MessageStartSourceUUID)
				assert.Equal(t, "stable-11", got.Evidence[0].MessageEndSourceUUID)
			}

			messages, err := d.GetAllMessages(context.Background(), "mixed")
			require.NoError(t, err)
			if tt.anchoredStart {
				messages[1].SourceUUID = ""
			} else {
				messages[2].SourceUUID = ""
			}
			logs := captureRecallEvidenceLog(t)
			err = d.ReplaceSessionMessages("mixed", messages)
			require.NoError(t, err)

			got = requireRecallEntry(t, d, "m1")
			assert.False(t, got.ProvenanceOK)
			assert.Equal(
				t,
				"recall: revoked provenance entry=m1 session=mixed reason="+
					tt.wantReason,
				strings.TrimSpace(logs.String()),
			)
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

func TestRecallEvidenceReplaceSessionContentLogsCommittedRevocation(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "replace-content", 10, "stable", "")
	insertVerifiedRecallSelection(
		t,
		d,
		"content-entry",
		"replace-content",
		10,
		11,
		[]string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "replace-content")
	require.NoError(t, err)
	messages[0].Content = "Changed by ReplaceSessionContent."
	messages[0].ContentLength = len(messages[0].Content)
	logs := captureRecallEvidenceLog(t)

	err = d.ReplaceSessionContent(
		"replace-content",
		messages,
		SessionSignalUpdate{},
		nil,
	)

	require.NoError(t, err)
	entry := requireRecallEntry(t, d, "content-entry")
	assert.False(t, entry.ProvenanceOK)
	stored, err := d.GetAllMessages(context.Background(), "replace-content")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "Changed by ReplaceSessionContent.", stored[0].Content)
	assert.Equal(
		t,
		"recall: revoked provenance entry=content-entry "+
			"session=replace-content reason=content_digest_mismatch",
		strings.TrimSpace(logs.String()),
	)
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

func TestRecallEvidenceReconciliationLogsStableReason(t *testing.T) {
	tests := []struct {
		name         string
		sourcePrefix string
		wantReason   string
		mutate       func(*testing.T, *DB, []Message) []Message
	}{
		{
			name:         "both unresolved endpoints prefer start",
			sourcePrefix: "stable",
			wantReason:   "start_endpoint_unresolved",
			mutate: func(_ *testing.T, _ *DB, messages []Message) []Message {
				for i := range messages {
					messages[i].SourceUUID = ""
				}
				return messages
			},
		},
		{
			name:         "end endpoint unresolved",
			sourcePrefix: "stable",
			wantReason:   "end_endpoint_unresolved",
			mutate: func(_ *testing.T, _ *DB, messages []Message) []Message {
				messages[1].SourceUUID = ""
				return messages
			},
		},
		{
			name:         "invalid resolved range",
			sourcePrefix: "stable",
			wantReason:   "invalid_resolved_range",
			mutate: func(_ *testing.T, _ *DB, messages []Message) []Message {
				messages[0].SourceUUID, messages[1].SourceUUID =
					messages[1].SourceUUID, messages[0].SourceUUID
				return messages
			},
		},
		{
			name:         "window invalid",
			sourcePrefix: "",
			wantReason:   "window_invalid",
			mutate: func(_ *testing.T, _ *DB, messages []Message) []Message {
				return messages[1:]
			},
		},
		{
			name:         "selection invalid",
			sourcePrefix: "stable",
			wantReason:   "selection_invalid",
			mutate: func(t *testing.T, _ *DB, messages []Message) []Message {
				require.Len(t, messages[1].ToolCalls, 2)
				messages[1].ToolCalls = messages[1].ToolCalls[:1]
				return messages
			},
		},
		{
			name:         "missing digest",
			sourcePrefix: "stable",
			wantReason:   "missing_digest",
			mutate: func(t *testing.T, d *DB, messages []Message) []Message {
				result, err := d.getWriter().Exec(`
					UPDATE recall_evidence
					SET content_digest = ''
					WHERE entry_id = 'reason-entry'`)
				require.NoError(t, err)
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows)
				messages[0].Timestamp = "2026-07-09T13:00:00Z"
				return messages
			},
		},
		{
			name:         "content digest mismatch",
			sourcePrefix: "stable",
			wantReason:   "content_digest_mismatch",
			mutate: func(_ *testing.T, _ *DB, messages []Message) []Message {
				messages[0].Content = "Changed evidence content."
				messages[0].ContentLength = len(messages[0].Content)
				return messages
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			seedRecallEvidenceWindow(
				t, d, "reason-session", 10, tt.sourcePrefix, "",
			)
			insertVerifiedRecallSelection(
				t,
				d,
				"reason-entry",
				"reason-session",
				10,
				11,
				[]string{"tool-a"},
			)
			messages, err := d.GetAllMessages(
				context.Background(),
				"reason-session",
			)
			require.NoError(t, err)
			messages = tt.mutate(t, d, messages)
			logs := captureRecallEvidenceLog(t)

			err = d.ReplaceSessionMessages("reason-session", messages)

			require.NoError(t, err)
			got := requireRecallEntry(t, d, "reason-entry")
			assert.False(t, got.ProvenanceOK)
			assert.Equal(
				t,
				"recall: revoked provenance entry=reason-entry "+
					"session=reason-session reason="+tt.wantReason,
				strings.TrimSpace(logs.String()),
			)
		})
	}
}

func TestRecallEvidenceReconciliationLogsOnlyFirstRevocation(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "multi-group", 10, "stable", "")
	insertVerifiedRecallSelection(
		t, d, "multi-entry", "multi-group", 10, 11, []string{"tool-a"},
	)
	window, err := d.BuildRecallEvidenceWindow(
		context.Background(),
		"multi-group",
		11,
		12,
	)
	require.NoError(t, err)
	metadata, err := window.BindSelection(RecallEvidenceSelection{
		MessageStartOrdinal: 11,
		MessageEndOrdinal:   12,
		ToolUseIDs:          []string{"tool-z"},
	})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		INSERT INTO recall_evidence (
			entry_id, session_id, message_start_ordinal,
			message_end_ordinal, message_start_source_uuid,
			message_end_source_uuid, content_digest, tool_use_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"multi-entry", "multi-group", 11, 12,
		metadata.MessageStartSourceUUID, metadata.MessageEndSourceUUID,
		metadata.ContentDigest, "tool-z",
	)
	require.NoError(t, err)
	messages, err := d.GetAllMessages(context.Background(), "multi-group")
	require.NoError(t, err)
	messages[0].SourceUUID = ""
	messages[2].SourceUUID = ""
	logs := captureRecallEvidenceLog(t)

	err = d.ReplaceSessionMessages("multi-group", messages)

	require.NoError(t, err)
	got := requireRecallEntry(t, d, "multi-entry")
	assert.False(t, got.ProvenanceOK)
	assert.Equal(
		t,
		"recall: revoked provenance entry=multi-entry "+
			"session=multi-group reason=start_endpoint_unresolved",
		strings.TrimSpace(logs.String()),
	)

	logs.Reset()
	messages[0].Timestamp = "2026-07-09T13:00:00Z"
	err = d.ReplaceSessionMessages("multi-group", messages)
	require.NoError(t, err)
	assert.Empty(t, logs.String(), "revoked provenance must remain sticky")
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

func TestRecallEvidenceDiffRevokesEitherMissingToolFromMultiToolSelection(
	t *testing.T,
) {
	for _, tc := range []struct {
		name      string
		remaining func([]ToolCall) []ToolCall
	}{
		{
			name: "first tool missing",
			remaining: func(calls []ToolCall) []ToolCall {
				return calls[1:]
			},
		},
		{
			name: "second tool missing",
			remaining: func(calls []ToolCall) []ToolCall {
				return calls[:1]
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			seedRecallEvidenceWindow(t, d, "multi-tool", 10, "stable", "")
			insertVerifiedRecallSelection(
				t, d, "m1", "multi-tool", 10, 11,
				[]string{"tool-a", "tool-z"},
			)
			messages, err := d.GetAllMessages(
				context.Background(), "multi-tool",
			)
			require.NoError(t, err)
			require.Len(t, messages[1].ToolCalls, 2)
			messages[1].ToolCalls = tc.remaining(messages[1].ToolCalls)

			err = d.ReplaceSessionMessages("multi-tool", messages)

			require.NoError(t, err)
			got := requireRecallEntry(t, d, "m1")
			assert.False(t, got.ProvenanceOK)
		})
	}
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
	insertVerifiedRecallSelection(
		t, d, "a-revoked", "rollback", 10, 11, []string{"tool-a"},
	)
	original := insertVerifiedRecallSelection(
		t, d, "z-updated", "rollback", 10, 11, []string{"tool-a"},
	)
	_, err := d.getWriter().Exec(`
		UPDATE recall_evidence
		SET content_digest = 'stale-digest'
		WHERE entry_id = 'a-revoked'`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(`
		CREATE TRIGGER fail_recall_evidence_reconcile
		BEFORE UPDATE ON recall_evidence
		BEGIN
			SELECT RAISE(ABORT, 'forced reconciliation failure');
		END`)
	require.NoError(t, err)
	shifted := shiftedRecallMessages(t, d, "rollback", 1)
	logs := captureRecallEvidenceLog(t)

	err = d.ReplaceSessionMessages("rollback", shifted)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced reconciliation failure")
	assert.Empty(t, logs.String(), "rolled-back revocations must not be logged")
	messages, readErr := d.GetAllMessages(context.Background(), "rollback")
	require.NoError(t, readErr)
	require.Len(t, messages, 3)
	assert.Equal(t, 10, messages[0].Ordinal)
	assert.Equal(t, "Run the formatter.", messages[0].Content)
	revoked := requireRecallEntry(t, d, "a-revoked")
	assert.True(t, revoked.ProvenanceOK)
	require.Len(t, revoked.Evidence, 1)
	assert.Equal(t, "stale-digest", revoked.Evidence[0].ContentDigest)
	updated := requireRecallEntry(t, d, "z-updated")
	assert.True(t, updated.ProvenanceOK)
	require.Len(t, updated.Evidence, 1)
	assert.Equal(t, 10, updated.Evidence[0].MessageStartOrdinal)
	assert.Equal(t, original.ContentDigest, updated.Evidence[0].ContentDigest)
}

func TestRecallEvidenceWriteSessionBatchDiscardsSavepointRevocations(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "batch-failed", 10, "failed", "")
	seedRecallEvidenceWindow(t, d, "batch-committed", 10, "committed", "")
	insertVerifiedRecallSelection(
		t,
		d,
		"failed-entry",
		"batch-failed",
		10,
		11,
		[]string{"tool-a"},
	)
	insertVerifiedRecallSelection(
		t,
		d,
		"committed-entry",
		"batch-committed",
		10,
		11,
		[]string{"tool-a"},
	)
	failedMessages, err := d.GetAllMessages(
		context.Background(),
		"batch-failed",
	)
	require.NoError(t, err)
	failedMessages[0].Content = "Changed before savepoint rollback."
	failedMessages[0].ContentLength = len(failedMessages[0].Content)
	committedMessages, err := d.GetAllMessages(
		context.Background(),
		"batch-committed",
	)
	require.NoError(t, err)
	committedMessages[0].Content = "Changed and committed."
	committedMessages[0].ContentLength = len(committedMessages[0].Content)
	failedSession, err := d.GetSession(context.Background(), "batch-failed")
	require.NoError(t, err)
	require.NotNil(t, failedSession)
	committedSession, err := d.GetSession(
		context.Background(),
		"batch-committed",
	)
	require.NoError(t, err)
	require.NotNil(t, committedSession)
	_, err = d.getWriter().Exec(`
		CREATE TRIGGER fail_batch_after_reconcile
		BEFORE UPDATE OF data_version ON sessions
		WHEN OLD.id = 'batch-failed'
		BEGIN
			SELECT RAISE(ABORT, 'forced savepoint failure');
		END`)
	require.NoError(t, err)
	logs := captureRecallEvidenceLog(t)

	result, err := d.WriteSessionBatch([]SessionBatchWrite{
		{
			Session:         *failedSession,
			Messages:        failedMessages,
			DataVersion:     999,
			ReplaceMessages: true,
		},
		{
			Session:         *committedSession,
			Messages:        committedMessages,
			DataVersion:     999,
			ReplaceMessages: true,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	assert.Equal(t, 1, result.FailedSessions)
	require.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0].Error(), "forced savepoint failure")
	failed := requireRecallEntry(t, d, "failed-entry")
	assert.True(t, failed.ProvenanceOK)
	failedStored, err := d.GetAllMessages(context.Background(), "batch-failed")
	require.NoError(t, err)
	require.Len(t, failedStored, 3)
	assert.Equal(t, "Run the formatter.", failedStored[0].Content)
	committed := requireRecallEntry(t, d, "committed-entry")
	assert.False(t, committed.ProvenanceOK)
	committedStored, err := d.GetAllMessages(
		context.Background(),
		"batch-committed",
	)
	require.NoError(t, err)
	require.Len(t, committedStored, 3)
	assert.Equal(t, "Changed and committed.", committedStored[0].Content)
	assert.Equal(
		t,
		"recall: revoked provenance entry=committed-entry "+
			"session=batch-committed reason=content_digest_mismatch",
		strings.TrimSpace(logs.String()),
	)
}

func TestRecallEvidenceWriteSessionBatchAtomicLogsOnlyAfterCommit(t *testing.T) {
	d := testDB(t)
	seedRecallEvidenceWindow(t, d, "batch-atomic", 10, "atomic", "")
	insertVerifiedRecallSelection(
		t,
		d,
		"atomic-entry",
		"batch-atomic",
		10,
		11,
		[]string{"tool-a"},
	)
	messages, err := d.GetAllMessages(context.Background(), "batch-atomic")
	require.NoError(t, err)
	messages[0].Content = "Changed only if the atomic batch commits."
	messages[0].ContentLength = len(messages[0].Content)
	session, err := d.GetSession(context.Background(), "batch-atomic")
	require.NoError(t, err)
	require.NotNil(t, session)
	write := SessionBatchWrite{
		Session:         *session,
		Messages:        messages,
		ReplaceMessages: true,
	}
	logs := captureRecallEvidenceLog(t)

	result, err := d.WriteSessionBatchAtomic(
		[]SessionBatchWrite{write},
		func() error { return assert.AnError },
	)

	require.ErrorIs(t, err, assert.AnError)
	assert.Zero(t, result.WrittenSessions)
	assert.Empty(t, logs.String(), "rejected atomic writes must not be logged")
	entry := requireRecallEntry(t, d, "atomic-entry")
	assert.True(t, entry.ProvenanceOK)
	stored, err := d.GetAllMessages(context.Background(), "batch-atomic")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "Run the formatter.", stored[0].Content)

	logs.Reset()
	result, err = d.WriteSessionBatchAtomic([]SessionBatchWrite{write})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	entry = requireRecallEntry(t, d, "atomic-entry")
	assert.False(t, entry.ProvenanceOK)
	stored, err = d.GetAllMessages(context.Background(), "batch-atomic")
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "Changed only if the atomic batch commits.", stored[0].Content)
	assert.Equal(
		t,
		"recall: revoked provenance entry=atomic-entry "+
			"session=batch-atomic reason=content_digest_mismatch",
		strings.TrimSpace(logs.String()),
	)
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

func captureRecallEvidenceLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &output
}
