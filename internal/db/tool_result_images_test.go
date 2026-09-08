package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func testInlineImageContent() string {
	return `[{"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"}]`
}

func testImageMessage(sessionID string) Message {
	content := testInlineImageContent()
	return Message{
		SessionID: sessionID,
		Ordinal:   0,
		Role:      "assistant",
		Content:   "answer",
		ToolCalls: []ToolCall{{
			ToolName:      "Read",
			Category:      "Read",
			ToolUseID:     "call-1",
			ResultContent: content,
			ResultEvents: []ToolResultEvent{{
				ToolUseID: "call-1",
				Source:    "tool",
				Status:    "completed",
				Content:   content,
			}},
		}},
	}
}

func TestStripToolResultImages(t *testing.T) {
	content := testInlineImageContent()
	got, stats := StripToolResultImages(content)
	assert.Equal(t, int64(1), stats.Payloads)
	assert.Equal(t, int64(3), stats.DecodedBytes)
	assert.Equal(t, int64(len("data:image/png;base64,AAEC")), stats.StoredBytes)
	assert.NotContains(t, got, "input_image")
	assert.Contains(t, got, `"type":"agentsview_image"`)
	assert.Contains(t, got, `"version":1`)
	assert.Contains(t, got, `"media_type":"image/png"`)
	assert.Contains(t, got, `"byte_size":3`)
	assert.Contains(t, got, `"sha256":""`)
	assert.Contains(t, got, `"text":"before"`)
	assert.Contains(t, got, `"text":"after"`)
	assert.Equal(t, got, mustStripImage(t, got))

	var blocks []map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &blocks))
	assert.Equal(t, "agentsview_image", blocks[1]["type"])

	withMarkup := `[{"type":"text","text":"<b> & redirect >"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	got, _ = StripToolResultImages(withMarkup)
	assert.Contains(t, got, `<b> & redirect >`)
	assert.NotContains(t, got, `\u003c`)

	withUnknownFields := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC","detail":"high","vendor":{"mode":"full"}}]`
	got, _ = StripToolResultImages(withUnknownFields)
	assert.Contains(t, got, `"detail":"high"`)
	assert.Contains(t, got, `"vendor":{"mode":"full"}`)

	withSHA256 := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC","sha256":"abc123"}]`
	got, _ = StripToolResultImages(withSHA256)
	assert.Contains(t, got, `"sha256":"abc123"`)

	withCaseVariantKeys := `[{"TYPE":"input_image","IMAGE_URL":"data:image/png;base64,AAEC"}]`
	got, stats = StripToolResultImages(withCaseVariantKeys)
	assert.Equal(t, int64(1), stats.Payloads)
	assert.NotContains(t, got, "data:image/png")
	assert.Contains(t, got, `"type":"agentsview_image"`)

	multiAgent := "agent-a:\n" + content + "\n\nagent-b:\n" + content
	got, stats = StripToolResultImages(multiAgent)
	assert.Equal(t, int64(2), stats.Payloads)
	assert.NotContains(t, got, "input_image")
	assert.Contains(t, got, "agent-a:\n")
	assert.Contains(t, got, "agent-b:\n")
}

func TestStripToolResultImagesAcceptsEscapedType(t *testing.T) {
	content := `[{"type":"input_\u0069mage","image_url":"data:image/png;base64,AAEC"}]`
	got, stats := StripToolResultImages(content)
	assert.Equal(t, int64(1), stats.Payloads)
	assert.NotContains(t, got, "input_image")
}

func mustStripImage(t *testing.T, content string) string {
	t.Helper()
	got, stats := StripToolResultImages(content)
	assert.Zero(t, stats)
	return got
}

func TestStripToolResultImagesNegativeSpace(t *testing.T) {
	tests := []string{
		`ordinary data:image/png;base64,AAEC text`,
		`[{"type":"input_image","image_url":"https://example.test/image.png"}]`,
		`[{"type":"input_image","image_url":"data:text/plain;base64,AAEC"}]`,
		`[{"type":"input_image","image_url":"data:image/png;base64,%%%"}]`,
		`[{"type":"agentsview_image","version":1,"text":"[Image]","media_type":"image/png","byte_size":3,"sha256":"abc"}]`,
		`[{"type":"text","text":"data:image/png;base64,AAEC"}]`,
	}
	for _, content := range tests {
		got, stats := StripToolResultImages(content)
		assert.Equal(t, content, got)
		assert.Zero(t, stats)
	}
}

func TestDBPolicyZeroValue(t *testing.T) {
	d := testDB(t)
	assert.Equal(t, config.ToolResultImagesKeep, d.ToolResultImages())
	insertSession(t, d, "keep", "project")
	insertMessages(t, d, testImageMessage("keep"))
	got, err := d.GetAllMessages(context.Background(), "keep")
	require.NoError(t, err)
	assert.Contains(t, got[0].ToolCalls[0].ResultContent, "input_image")

	d.SetToolResultImages(config.ToolResultImagesDrop)
	assert.Equal(t, config.ToolResultImagesDrop, d.ToolResultImages())
}

func TestIngestWithDropRemovesInlineImagesFromBothTables(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesDrop)
	insertSession(t, d, "ingest", "project")
	message := testImageMessage("ingest")
	original := message
	insertMessages(t, d, message)
	assert.Equal(t, original.ToolCalls[0].ResultContent, message.ToolCalls[0].ResultContent)

	got, err := d.GetAllMessages(context.Background(), "ingest")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.NotContains(t, got[0].ToolCalls[0].ResultContent, "input_image")
	assert.NotContains(t, got[0].ToolCalls[0].ResultEvents[0].Content, "input_image")
	assert.Contains(t, got[0].ToolCalls[0].ResultContent, "agentsview_image")
	assert.Contains(t, got[0].ToolCalls[0].ResultEvents[0].Content, "agentsview_image")
}

func TestToolResultImagesWriteRoutes(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesDrop)

	insertSession(t, d, "direct", "project")
	direct := testImageMessage("direct")
	require.NoError(t, d.InsertMessages([]Message{direct}))

	insertSession(t, d, "incremental", "project")
	_, err := d.WriteSessionIncremental(
		"incremental", []Message{testImageMessage("incremental")}, IncrementalSessionUpdate{},
	)
	require.NoError(t, err)

	insertSession(t, d, "replacement", "project")
	require.NoError(t, d.ReplaceSessionMessages("replacement", []Message{testImageMessage("replacement")}))
	require.NoError(t, d.ReplaceSessionContent("replacement", []Message{testImageMessage("replacement")}, SessionSignalUpdate{}, nil))

	insertSession(t, d, "batch", "project")
	_, err = d.WriteSessionBatch([]SessionBatchWrite{{
		Session:  Session{ID: "batch", Project: "project", Machine: "local", Agent: "codex"},
		Messages: []Message{testImageMessage("batch")},
	}})
	require.NoError(t, err)

	for _, sessionID := range []string{"direct", "incremental", "replacement", "batch"} {
		messages, err := d.GetAllMessages(context.Background(), sessionID)
		require.NoError(t, err, sessionID)
		require.Len(t, messages, 1, sessionID)
		assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image", sessionID)
	}

	insertSession(t, d, "incremental-link", "project")
	require.NoError(t, d.InsertMessages([]Message{testImageMessage("incremental-link")}))
	links := []ToolCallSubagentLink{{
		ToolUseID:        "call-1",
		ResultContent:    testInlineImageContent(),
		ResultContentLen: len(testInlineImageContent()),
		HasResult:        true,
	}}
	originalLinks := append([]ToolCallSubagentLink(nil), links...)
	_, err = d.WriteSessionIncremental(
		"incremental-link", nil, IncrementalSessionUpdate{SubagentLinks: links},
	)
	require.NoError(t, err)
	assert.Equal(t, originalLinks, links)
	messages, err := d.GetAllMessages(context.Background(), "incremental-link")
	require.NoError(t, err)
	assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
}

func TestWriteSessionBatchAtomicWithDropProjectsStoredToolRows(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesDrop)
	message := testImageMessage("atomic-images")
	message.ToolCalls[0].ResultContent =
		`[{"type":"text","text":"summary"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	originalCallContent := message.ToolCalls[0].ResultContent
	originalEventContent := message.ToolCalls[0].ResultEvents[0].Content

	result, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session: Session{
			ID: "atomic-images", Project: "project", Machine: "local", Agent: "codex",
		},
		Messages:        []Message{message},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	assert.Equal(t, originalCallContent, message.ToolCalls[0].ResultContent)
	assert.Equal(t, originalEventContent, message.ToolCalls[0].ResultEvents[0].Content)

	var storedCall, storedEvent string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT COALESCE(tc.result_content, ''), ev.content
		FROM tool_calls tc
		JOIN tool_result_events ev
		  ON ev.session_id = tc.session_id
		 AND ev.tool_call_message_ordinal = 0
		 AND ev.call_index = tc.call_index
		WHERE tc.session_id = ?`, "atomic-images").Scan(
		&storedCall, &storedEvent,
	))
	assert.NotContains(t, storedCall, "input_image")
	assert.NotContains(t, storedEvent, "input_image")
	assert.Contains(t, storedEvent, "agentsview_image")
}

func TestToolResultImagesDedupAndLengths(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesDrop)
	insertSession(t, d, "lengths", "project")
	require.NoError(t, d.InsertMessages([]Message{testImageMessage("lengths")}))

	var summaryLength, eventLength int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT result_content_length FROM tool_calls WHERE session_id = ?", "lengths",
	).Scan(&summaryLength))
	require.NoError(t, d.getReader().QueryRow(
		"SELECT content_length FROM tool_result_events WHERE session_id = ?", "lengths",
	).Scan(&eventLength))
	assert.Greater(t, summaryLength, 0)
	assert.Greater(t, eventLength, 0)
	assert.Equal(t, eventLength, summaryLength)
}
