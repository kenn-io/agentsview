package db

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
	"strings"
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

func TestDropImagesPreservesEmptySummaryMeaning(t *testing.T) {
	const raw = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const projected = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	for _, tt := range []struct {
		name          string
		summaryLength int
		events        []ToolResultEvent
		wantLength    int
		wantEvent     string
		wantEventLen  int
	}{
		{
			name: "deduplicated", summaryLength: len(raw),
			events:     []ToolResultEvent{{Content: raw, ContentLength: len(raw)}},
			wantLength: len(projected), wantEvent: projected, wantEventLen: len(projected),
		},
		{
			name:      "genuinely empty",
			events:    []ToolResultEvent{{Content: raw, ContentLength: len(raw)}},
			wantEvent: projected, wantEventLen: len(projected),
		},
		{
			name: "blocked", summaryLength: len(raw),
			events:     []ToolResultEvent{{ContentLength: len(raw)}},
			wantLength: len(raw), wantEventLen: len(raw),
		},
		{
			name: "multiple events", summaryLength: len(raw),
			events:     []ToolResultEvent{{Content: raw}, {Content: "later"}},
			wantLength: len(raw), wantEvent: projected, wantEventLen: len(projected),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			d.SetToolResultImages(config.ToolResultImagesDrop)
			insertSession(t, d, "empty-summary", "project")
			require.NoError(t, d.InsertMessages([]Message{{
				SessionID: "empty-summary", Role: "assistant",
				ToolCalls: []ToolCall{{
					ToolUseID: "call", ResultContentLength: tt.summaryLength,
					ResultEvents: tt.events,
				}},
			}}))

			var summary, event string
			var summaryLength, eventLength int
			require.NoError(t, d.getReader().QueryRow(`
				SELECT COALESCE(tc.result_content, ''), COALESCE(tc.result_content_length, 0),
				       ev.content, ev.content_length
				FROM tool_calls tc
				JOIN tool_result_events ev
				  ON ev.session_id = tc.session_id AND ev.call_index = tc.call_index
				WHERE tc.session_id = 'empty-summary' AND ev.event_index = 0`,
			).Scan(&summary, &summaryLength, &event, &eventLength))
			assert.Empty(t, summary)
			assert.Equal(t, tt.wantLength, summaryLength)
			assert.Equal(t, tt.wantEvent, event)
			assert.Equal(t, tt.wantEventLen, eventLength)
		})
	}
}

func TestDropImagesLateResultsRetainRawIdentity(t *testing.T) {
	d := testDB(t)
	d.SetToolResultImages(config.ToolResultImagesDrop)
	insertSession(t, d, "late-images", "project")
	require.NoError(t, d.InsertMessages([]Message{{
		SessionID: "late-images", Role: "assistant",
		ToolCalls: []ToolCall{{
			ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
		}},
	}}))
	const first = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const second = `[{"type":"input_image","image_url":"data:image/png;base64,AwQF"}]`
	update := IncrementalSessionUpdate{
		MsgCount: 1, NextOrdinal: 1,
		ToolCallResultUpdates: []ToolCallResultUpdate{{
			ToolUseID: "call", Events: []ToolResultEvent{
				{Content: first, Source: "function_call_output"},
				{Content: second, Source: "function_call_output"},
			},
		}},
	}
	_, err := d.WriteSessionIncremental("late-images", nil, update)
	require.NoError(t, err)
	messages, err := d.GetAllMessages(t.Context(), "late-images")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	call := messages[0].ToolCalls[0]
	require.Len(t, call.ResultEvents, 2)
	const want = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	assert.Equal(t, want, call.ResultContent)
	for _, event := range call.ResultEvents {
		assert.Equal(t, want, event.Content)
		assert.Equal(t, len(want), event.ContentLength)
	}
	assert.NotEqual(t, call.ResultEvents[0].RawContentDigest, call.ResultEvents[1].RawContentDigest)
	before, err := d.GetSessionFull(t.Context(), "late-images")
	require.NoError(t, err)
	_, err = d.WriteSessionIncremental("late-images", nil, update)
	require.NoError(t, err)
	after, err := d.GetSessionFull(t.Context(), "late-images")
	require.NoError(t, err)
	assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision, "raw replay must remain a no-op after projection")
	assert.Equal(t, first, update.ToolCallResultUpdates[0].Events[0].Content)
}

func TestStripToolImagesMixedSummary(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "mixed", "project")
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "mixed", Role: "assistant", ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "exec_command", Category: "Bash"}}}}))
	raw := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	_, err := d.WriteSessionIncremental("mixed", nil, IncrementalSessionUpdate{MsgCount: 1, NextOrdinal: 1, ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "call", Events: []ToolResultEvent{
		{AgentID: "agent-a", Content: raw, Source: "function_call_output"},
		{AgentID: "agent-b", Content: "plain result", Source: "function_call_output"},
		{Content: raw, Source: "function_call_output"},
	}}}})
	require.NoError(t, err)
	report, err := d.StripToolImages(t.Context(), StripImagesFilter{})
	require.NoError(t, err)
	var after string
	require.NoError(t, d.getReader().QueryRow("SELECT result_content FROM tool_calls WHERE session_id = ?", "mixed").Scan(&after))
	assert.NotContains(t, after, "input_image")
	assert.Equal(t, int64(4), report.Payloads)
}

func TestDropImagesLateSummaryWithExistingRawEvent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "late-existing", "project")
	raw := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	require.NoError(t, d.InsertMessages([]Message{{SessionID: "late-existing", Role: "assistant", ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "exec_command", Category: "Bash"}}}}))
	_, firstErr := d.WriteSessionIncremental("late-existing", nil, IncrementalSessionUpdate{MsgCount: 1, NextOrdinal: 1, ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "call", Events: []ToolResultEvent{{AgentID: "agent-a", Content: raw, Source: "function_call_output"}}}}})
	require.NoError(t, firstErr)
	d.SetToolResultImages(config.ToolResultImagesDrop)
	_, err := d.WriteSessionIncremental("late-existing", nil, IncrementalSessionUpdate{MsgCount: 1, NextOrdinal: 1, ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "call", Events: []ToolResultEvent{{AgentID: "agent-b", Content: raw, Source: "function_call_output"}}}}})
	require.NoError(t, err)
	messages, err := d.GetAllMessages(t.Context(), "late-existing")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	call := messages[0].ToolCalls[0]
	require.Len(t, call.ResultEvents, 2)
	assert.NotContains(t, call.ResultEvents[1].Content, "input_image")
	assert.NotContains(t, call.ResultContent, "input_image")
	assert.Equal(t, len(call.ResultContent), call.ResultContentLength)
	assert.Contains(t, call.ResultContent, "agent-a:")
	assert.Contains(t, call.ResultContent, "agent-b:")
	assert.Contains(t, call.ResultEvents[0].Content, "input_image")
}

func TestStripToolResultSummarySections(t *testing.T) {
	const raw = `[
 {"type":"text","text":"before"},

 {"type":"input_image","image_url":"data:image/png;base64,AAEC"}
]`
	for _, tt := range []struct {
		content  string
		payloads int64
	}{
		{"agent-a:\nplain text\n\nagent-b:\n" + raw + "\n\n" + raw, 2},
		{raw + "\n\n" + raw, 2},
		{"plain text\n\n" + raw, 1},
	} {
		projected, stats := StripToolResultImages(tt.content)
		assert.NotContains(t, projected, "input_image")
		assert.Contains(t, projected, `"text":"before"`)
		assert.Equal(t, tt.payloads, stats.Payloads)
		again, repeated := StripToolResultImages(projected)
		assert.Equal(t, projected, again)
		assert.Zero(t, repeated)
	}
}

func TestStripToolResultSummaryPreservesSectionBoundaries(t *testing.T) {
	const image = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const splitImage = `[{"type":
"input_image","image_url":"data:image/png;base64,AAEC"}]`
	const placeholder = `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	const unsupported = `[{"type":"input_image","image_url":"https://example.com/image.png"}]`
	for _, tt := range []struct{ name, content, want string }{
		{"anonymous split property", "plain\n\n" + splitImage, "plain\n\n" + placeholder},
		{"trailing newline", "agent-a:\nplain result\n\n\nagent-b:\n" + image, "agent-a:\nplain result\n\n\nagent-b:\n" + placeholder},
		{"unsupported neighboring result", "agent-a:\n" + unsupported + "\n\n" + image, "agent-a:\n" + unsupported + "\n\n" + placeholder},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, stats := StripToolResultImages(tt.content)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, int64(1), stats.Payloads)
		})
	}
}

func assertOffloadedImage(t *testing.T, content, dir string) {
	t.Helper()
	var blocks []struct {
		Ref string `json:"image_ref"`
	}
	require.NoError(t, json.Unmarshal([]byte(content), &blocks))
	var ref string
	for _, block := range blocks {
		if block.Ref != "" {
			ref = block.Ref
		}
	}
	require.True(t, strings.HasPrefix(ref, "asset://"), content)
	body, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(ref, "asset://")))
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 1, 2}, body)
}

func TestProjectToolResultImageContent(t *testing.T) {
	raw := testInlineImageContent()
	for _, mode := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesDrop, config.ToolResultImagesOffload, "unknown"} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			got := ProjectToolResultImageContent(raw, mode, dir)
			if mode == config.ToolResultImagesOffload {
				assertOffloadedImage(t, got, dir)
				assert.Contains(t, got, `"text":"before"`)
				assert.Contains(t, got, `"text":"after"`)
			} else {
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				assert.Empty(t, entries)
				if mode == config.ToolResultImagesDrop {
					assert.Contains(t, got, `"type":"agentsview_image"`)
					assert.NotContains(t, got, "image_ref")
				} else {
					assert.Equal(t, raw, got)
				}
			}
		})
	}
	for _, raw := range []string{"", "ordinary text", `[{"type":"input_image","image_url":"data:image/svg+xml;base64,AAEC"}]`, `[{"type":"input_image","image_url":"data:image/png;base64,!!!"}]`, `[{"type":"input_image","image_url":"data:image/png,AAEC"}]`} {
		dir := t.TempDir()
		assert.Equal(t, raw, ProjectToolResultImageContent(raw, config.ToolResultImagesOffload, dir))
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("occupied"), 0o600))
	assert.Equal(t, raw, ProjectToolResultImageContent(raw, config.ToolResultImagesOffload, blocked))
	assert.Equal(t, raw, ProjectToolResultImageContent(raw, config.ToolResultImagesOffload, ""))
}

func TestToolResultImagesOffloadWriteRoutes(t *testing.T) {
	for _, route := range []string{"insert", "incremental", "replacement", "content", "batch", "atomic"} {
		t.Run(route, func(t *testing.T) {
			d := testDB(t)
			d.SetToolResultImages(config.ToolResultImagesOffload)
			d.SetAssetsDir(t.TempDir())
			insertSession(t, d, route, "project")
			messages := []Message{testImageMessage(route)}
			switch route {
			case "insert":
				require.NoError(t, d.InsertMessages(messages))
			case "incremental":
				_, err := d.WriteSessionIncremental(route, messages, IncrementalSessionUpdate{})
				require.NoError(t, err)
			case "replacement":
				require.NoError(t, d.ReplaceSessionMessages(route, messages))
			case "content":
				require.NoError(t, d.ReplaceSessionContent(route, messages, SessionSignalUpdate{}, nil))
			default:
				writes := []SessionBatchWrite{{Session: Session{ID: route, Project: "project", Machine: "local", Agent: "codex"}, Messages: messages}}
				var result SessionBatchResult
				var err error
				if route == "atomic" {
					result, err = d.WriteSessionBatchAtomic(writes)
				} else {
					result, err = d.WriteSessionBatch(writes)
				}
				require.NoError(t, err)
				require.Equal(t, 1, result.WrittenSessions)
			}
			stored, err := d.GetAllMessages(t.Context(), route)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			call := stored[0].ToolCalls[0]
			assertOffloadedImage(t, call.ResultContent, d.AssetsDir())
			require.Len(t, call.ResultEvents, 1)
			assertOffloadedImage(t, call.ResultEvents[0].Content, d.AssetsDir())
			assert.Equal(t, len(call.ResultEvents[0].Content), call.ResultEvents[0].ContentLength)
			assert.Equal(t, testInlineImageContent(), messages[0].ToolCalls[0].ResultEvents[0].Content)
		})
	}
}

func TestToolResultImagesOffloadLateAndLinked(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocked=%t/linked=%t", blocked, linked), func(t *testing.T) {
				d := testDB(t)
				d.SetToolResultImages(config.ToolResultImagesOffload)
				d.SetAssetsDir(t.TempDir())
				insertSession(t, d, "late", "project")
				require.NoError(t, d.InsertMessages([]Message{{SessionID: "late", Role: "assistant", ToolCalls: []ToolCall{{ToolUseID: "call", Category: "Bash"}}}}))
				raw := testInlineImageContent()
				update := IncrementalSessionUpdate{MsgCount: 1, NextOrdinal: 1, BlockedResultCategories: map[string]bool{"Bash": blocked}}
				if linked {
					update.SubagentLinks = []ToolCallSubagentLink{{ToolUseID: "call", HasResult: true, ResultContent: raw, ResultContentLen: len(raw)}}
				} else {
					update.ToolCallResultUpdates = []ToolCallResultUpdate{{ToolUseID: "call", Events: []ToolResultEvent{{Content: raw, Source: "function_call_output"}}}}
				}
				_, err := d.WriteSessionIncremental("late", nil, update)
				require.NoError(t, err)
				stored, err := d.GetAllMessages(t.Context(), "late")
				require.NoError(t, err)
				call := stored[0].ToolCalls[0]
				if blocked {
					assert.Empty(t, call.ResultContent)
					assert.Equal(t, len(raw), call.ResultContentLength)
					entries, err := os.ReadDir(d.AssetsDir())
					require.NoError(t, err)
					assert.Empty(t, entries)
				} else {
					assertOffloadedImage(t, call.ResultContent, d.AssetsDir())
					assert.Equal(t, len(call.ResultContent), call.ResultContentLength)
				}
				before, err := d.GetSessionFull(t.Context(), "late")
				require.NoError(t, err)
				_, err = d.WriteSessionIncremental("late", nil, update)
				require.NoError(t, err)
				after, err := d.GetSessionFull(t.Context(), "late")
				require.NoError(t, err)
				assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
			})
		}
	}
}

func TestToolResultImagesResyncRoute(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			d := testDB(t)
			dir := t.TempDir()
			if failed {
				dir = filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(dir, []byte("occupied"), 0o600))
			}
			d.SetAssetsDir(dir)
			for _, id := range []string{"copied", "untouched"} {
				insertSession(t, d, id, "project")
				require.NoError(t, d.InsertMessages([]Message{testImageMessage(id)}))
			}
			d.SetToolResultImages(config.ToolResultImagesOffload)
			require.NoError(t, d.ProjectToolImagesForSessions(t.Context(), []string{"copied"}))
			for _, id := range []string{"copied", "untouched"} {
				messages, err := d.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				if id == "copied" && !failed {
					assertOffloadedImage(t, messages[0].ToolCalls[0].ResultContent, dir)
				} else {
					assert.Equal(t, testInlineImageContent(), messages[0].ToolCalls[0].ResultContent)
				}
			}
		})
	}
}

func TestToolResultImagesOffloadOmittedArchives(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetToolResultImages(config.ToolResultImagesOffload)
			d.SetAssetsDir(t.TempDir())
			d.SetArchiveContent(policy)
			insertSession(t, d, "omitted", "project")
			message := testImageMessage("omitted")
			message.ToolCalls[0].ResultEvents = nil
			require.NoError(t, d.InsertMessages([]Message{message}))
			_, err := d.WriteSessionIncremental("omitted", nil, IncrementalSessionUpdate{SubagentLinks: []ToolCallSubagentLink{{ToolUseID: "call-1", HasResult: true, ResultContent: testInlineImageContent()}}, ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "call-1", Events: []ToolResultEvent{{Content: testInlineImageContent()}}}}})
			require.NoError(t, err)
			require.NoError(t, d.ProjectToolImagesForSessions(t.Context(), []string{"omitted"}))
			entries, err := os.ReadDir(d.AssetsDir())
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestToolResultImagesOffloadPublishesBeforeInsert(t *testing.T) {
	d := testDB(t)
	d.SetAssetsDir(t.TempDir())
	d.SetToolResultImages(config.ToolResultImagesOffload)
	insertSession(t, d, "ordered", "project")
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(driverConn any) error {
		return driverConn.(*sqlite3.SQLiteConn).RegisterFunc("asset_published", func(content string) bool {
			const object = "ae4b3280e56e2faf83f414a6e3dabe9d5fbe18976544c05fed121accb85b53fc.png"
			body, err := os.ReadFile(filepath.Join(d.AssetsDir(), object))
			return err == nil && string(body) == "\x00\x01\x02" && strings.Contains(content, "asset://"+object)
		}, false)
	}))
	require.NoError(t, conn.Close())
	_, err = d.getWriter().Exec(`CREATE TEMP TRIGGER require_asset BEFORE INSERT ON tool_result_events WHEN NOT asset_published(NEW.content) BEGIN SELECT RAISE(ABORT, 'asset missing before insert'); END`)
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages([]Message{testImageMessage("ordered")}))
	var count int
	require.NoError(t, d.getReader().QueryRow("SELECT COUNT(*) FROM tool_result_events WHERE session_id = 'ordered'").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestOffloadLinkedSummaryKeepsReferenceWithOlderInlineEvent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "older", "project")
	require.NoError(t, d.InsertMessages([]Message{testImageMessage("older")}))
	d.SetToolResultImages(config.ToolResultImagesOffload)
	d.SetAssetsDir(t.TempDir())
	_, err := d.WriteSessionIncremental("older", nil, IncrementalSessionUpdate{SubagentLinks: []ToolCallSubagentLink{{ToolUseID: "call-1", HasResult: true, ResultContent: testInlineImageContent()}}})
	require.NoError(t, err)
	messages, err := d.GetAllMessages(t.Context(), "older")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	call := messages[0].ToolCalls[0]
	assertOffloadedImage(t, call.ResultContent, d.AssetsDir())
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, testInlineImageContent(), call.ResultEvents[0].Content)
	assert.Equal(t, len(call.ResultContent), call.ResultContentLength)
}

func TestOffloadOmittedLateResultDoesNotPublishOlderImages(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "retained", "project")
	message := testImageMessage("retained")
	PrepareToolResultEvent(&message.ToolCalls[0].ResultEvents[0])
	require.NoError(t, d.InsertMessages([]Message{message}))
	d.SetAssetsDir(t.TempDir())
	d.SetToolResultImages(config.ToolResultImagesOffload)
	d.SetArchiveContent(config.ArchiveContentTranscripts)
	_, err := d.WriteSessionIncremental("retained", nil, IncrementalSessionUpdate{ToolCallResultUpdates: []ToolCallResultUpdate{{ToolUseID: "call-1", Events: []ToolResultEvent{{Content: "later result", Source: "function_call_output"}}}}})
	require.NoError(t, err)
	objects, err := os.ReadDir(d.AssetsDir())
	require.NoError(t, err)
	assert.Empty(t, objects)
}
