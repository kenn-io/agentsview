//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushCompactComparisons(t *testing.T) {
	ctx := t.Context()
	const id = "compact"
	schema := fmt.Sprintf("agentsview_compact_%x", rand.Uint64())
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	messages := []db.Message{
		{
			SessionID: id, Ordinal: 0, Role: "assistant", Content: strings.Repeat("large-payload-λ", 10000) + "\x01 control\xff",
			ThinkingText: "reasoning\x01text\xff", Timestamp: "2025-08-12T10:11:12.123456789+05:30",
			DialogueText: new("dialogue\x00text\xff"),
			HasThinking:  true, HasToolUse: true, IsSystem: true,
			Model: "sample-model", ReasoningEffort: "high", ProviderID: "provider-a",
			TokenUsage:    jsontext.Value(`{"input_tokens":23,"output_tokens":7}`),
			ContextTokens: 23, OutputTokens: 7, HasContextTokens: true, HasOutputTokens: true,
			ClaudeMessageID: "message-a", ClaudeRequestID: "request-a",
			SourceType: "assistant", SourceSubtype: "analysis", PromptSource: "conversation",
			SourceUUID: "source-a", SourceParentUUID: "parent-a", IsSidechain: true, IsCompactBoundary: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName: "Read", Category: "file", ToolUseID: "call-a", InputJSON: "input\xff",
					SkillName: "reader", ResultContentLength: 10, ResultContent: "agent-a:\nfirst\n\nsecond",
					SubagentSessionID: "subagent-a", FilePath: "file\xff.txt",
					ObservedOutcome: new("completed"), ObservedRepeat: new("first"), SequenceEnding: new("continue"),
					ResultEvents: []db.ToolResultEvent{
						{EventIndex: 0, ToolUseID: "call-a", AgentID: "agent-a", SubagentSessionID: "subagent-a", Source: "tool_result", Status: "completed", Content: "first\xff", ContentLength: 5, Timestamp: "2025-08-12T10:11:13.987654321-04:00"},
						{EventIndex: 1, ToolUseID: "call-a", AgentID: "agent-a", SubagentSessionID: "subagent-a", Source: "tool_result", Status: "completed", Content: "second", ContentLength: 6, Timestamp: "2025-08-12T10:11:14.000000001-04:00"},
					},
				},
				{
					ToolName: "Bash", Category: "shell", ResultEvents: []db.ToolResultEvent{
						{EventIndex: 0, Source: "tool_result", Status: "empty", Content: "", ContentLength: 0},
					},
				},
			},
		},
		{
			SessionID: id, Ordinal: 1, Role: "assistant", Content: "tail response", HasToolUse: true,
			DialogueText: new(""),
			Timestamp:    "2025-08-12T10:12:00.000000001Z", Model: "sample-model",
			SourceUUID: "source-b", ToolCalls: []db.ToolCall{{
				ToolName: "Write", Category: "file", ToolUseID: "call-b", InputJSON: `{"path":"out.txt"}`,
				FilePath: "/tmp/out.txt", ObservedOutcome: new("initial"),
			}},
		},
	}
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: len(messages)}))
	require.NoError(t, local.InsertMessages(ctx, messages))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	counts, err := local.MessageContentFingerprints(ctx, []string{id})
	require.NoError(t, err)
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	mismatchedCounts := maps.Clone(counts)
	aggregate := mismatchedCounts[id]
	aggregate.Count++
	mismatchedCounts[id] = aggregate
	skipped, err := readPushSessionMessageComparisons(
		ctx, tx, []string{id}, mismatchedCounts, true,
	)
	require.NoError(t, err)
	assert.Equal(t, len(messages), skipped.MessageCounts[id])
	for _, fingerprints := range []map[string]string{skipped.MessageContentHash, skipped.MessageRoleTime, skipped.MessageFlags, skipped.MessageDialogue, skipped.MessageTokenFingerprint, skipped.ToolCallFingerprint, skipped.ToolResultFingerprint} {
		assert.NotContains(t, fingerprints, id)
	}
	compact, err := readPushSessionMessageComparisons(
		ctx, tx, []string{id}, counts, true,
	)
	require.NoError(t, err)

	snapshot, err := local.BeginMessageSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Close()) }()
	matches, err := pushTranscriptMatches(ctx, snapshot, id, compact)
	require.NoError(t, err)
	assert.True(t, matches, "empty fields and sanitized text must match the published rows")
	for _, change := range []struct{ name, statement string }{
		{"content", `UPDATE messages SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
		{"thinking", `UPDATE messages SET thinking_text = 'x' || substring(thinking_text from 2) WHERE session_id = $1`},
		{"dialogue", `UPDATE messages SET dialogue_text = 'x' || substring(dialogue_text from 2) WHERE session_id = $1 AND ordinal = 0`},
		{"dialogue-null", `UPDATE messages SET dialogue_text = NULL WHERE session_id = $1 AND ordinal = 1`},
		{"input", `UPDATE tool_calls SET input_json = 'x' || substring(input_json from 2) WHERE session_id = $1 AND call_index = 0`},
		{"result", `UPDATE tool_calls SET result_content = 'x' || substring(result_content from 2) WHERE session_id = $1 AND call_index = 0`},
		{"path", `UPDATE tool_calls SET file_path = 'x' || substring(file_path from 2) WHERE session_id = $1 AND call_index = 0`},
		{"event", `UPDATE tool_result_events SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, err := tx.ExecContext(ctx, `SAVEPOINT change`)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT change`)
				require.NoError(t, err)
				_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT change`)
				require.NoError(t, err)
			})
			_, err = tx.ExecContext(ctx, change.statement, id)
			require.NoError(t, err)
			changed, err := readPushSessionMessageComparisons(
				ctx, tx, []string{id}, counts, true,
			)
			require.NoError(t, err)
			matches, err := pushTranscriptMatches(ctx, snapshot, id, changed)
			require.NoError(t, err)
			assert.False(t, matches)
		})
	}
}

func TestPushCompactLegacyTextRewrittenOnce(t *testing.T) {
	ctx := t.Context()
	schema := fmt.Sprintf("agentsview_compact_legacy_%x", rand.Uint64())
	const id = "legacy"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: id, Ordinal: 0, Role: "user", Content: "cleantext",
		ThinkingText: "thinkingtext", HasThinking: true, HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "Read", Category: "file", ToolUseID: "call",
			InputJSON: "inputtext", ResultContent: "resulttext",
			FilePath: "pathtext", ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "call", Source: "tool_result", Status: "completed",
				Content: "eventtext",
			}},
		}},
	}}))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	_, err = syncer.pg.ExecContext(ctx, `
		UPDATE messages SET content = $1, thinking_text = $2 WHERE session_id = $3
	`, "clean\x01text", "thinking\x01text", id)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `
		UPDATE tool_calls SET input_json = $1, result_content = $2, file_path = $3
		WHERE session_id = $4 AND call_index = 0
	`, "input\x01text", "result\x01text", "path\x01text", id)
	require.NoError(t, err)
	_, err = syncer.pg.ExecContext(ctx, `
		UPDATE tool_result_events SET content = $1 WHERE session_id = $2
	`, "event\x01text", id)
	require.NoError(t, err)
	// Call the publication path directly so the dependency marker doesn't
	// suppress the legacy comparison before it reaches PostgreSQL.
	for i := 0; i < 2; i++ {
		tx, err := syncer.pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		count, err := syncer.pushMessages(ctx, tx, id, false, nil, nil)
		require.NoError(t, err)
		if i == 0 {
			assert.Equal(t, 1, count)
		} else {
			assert.Zero(t, count)
		}
		require.NoError(t, tx.Commit())
	}
	var content, thinkingText string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT content, thinking_text FROM messages WHERE session_id=$1
	`, id).Scan(&content, &thinkingText))
	assert.Equal(t, "cleantext", content)
	assert.Equal(t, "thinkingtext", thinkingText)
	var inputJSON, resultContent, filePath string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT input_json, result_content, file_path FROM tool_calls
		WHERE session_id=$1 AND call_index=0
	`, id).Scan(&inputJSON, &resultContent, &filePath))
	assert.Equal(t, "inputtext", inputJSON)
	assert.Equal(t, "resulttext", resultContent)
	assert.Equal(t, "pathtext", filePath)
	var eventContent string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT content FROM tool_result_events WHERE session_id=$1
	`, id).Scan(&eventContent))
	assert.Equal(t, "eventtext", eventContent)
}

// pushResponseConn counts the PostgreSQL protocol bytes after TLS decoding.
type pushResponseConn struct {
	net.Conn
	received *atomic.Int64
}

func (c *pushResponseConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received.Add(int64(n))
	return n, err
}

func TestPushCompactResponseBytesDoNotGrowWithTranscriptBodies(t *testing.T) {
	small := compactPushResponseBytes(t, 4<<10)
	large := compactPushResponseBytes(t, 256<<10)
	assert.LessOrEqual(t, large-small, int64(32<<10), "response traffic must not grow with transcript bodies")
}

func compactPushResponseBytes(t *testing.T, size int) int64 {
	t.Helper()
	ctx := t.Context()
	schema := fmt.Sprintf("agentsview_push_response_bytes_%x", rand.Uint64())
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "sample-model"}}))
	require.NoError(t, local.UpsertGenAIPricing(ctx, db.GenAIPricingDocument{
		Version: "test", SourceRef: "test", Source: db.GenAIPricingSourceFetched, Data: []byte(`[]`),
	}))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "growing", FileMtime: new(time.Now().Add(time.Hour).UnixNano()), FileHash: new("initial"), Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	body := strings.Repeat("c", size)
	edited := []db.Message{{SessionID: "growing", Ordinal: 0, Role: "assistant", Content: body, ThinkingText: strings.Repeat("t", size), DialogueText: new(strings.Repeat("d", size)), HasThinking: true, HasToolUse: true,
		ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: "call", InputJSON: strings.Repeat("i", size), ResultContent: strings.Repeat("r", size), ResultContentLength: size, FilePath: "example.txt",
			ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Status: "completed", Content: strings.Repeat("e", size), ContentLength: size}},
		}},
	}}
	require.NoError(t, local.InsertMessages(ctx, edited))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "unchanged", Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1, FileMtime: new(time.Now().Add(time.Hour).UnixNano()), FileHash: new("initial")}))
	unchanged := edited[0]
	unchanged.SessionID = "unchanged"
	require.NoError(t, local.InsertMessages(ctx, []db.Message{unchanged}))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	initial, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, initial.Errors)
	// Keep unrelated pricing downloads outside the measured comparison response.
	_, err = syncer.pg.ExecContext(ctx, `TRUNCATE model_pricing_bands, model_pricing, genai_pricing`)
	require.NoError(t, err)
	before := pgMessageCTID(t, syncer.pg, "unchanged", 0)
	edited = append(edited, db.Message{SessionID: "growing", Ordinal: 1, Role: "user", Content: "new message"})
	require.NoError(t, local.ReplaceSessionMessages(ctx, "growing", edited))
	for _, id := range []string{"growing", "unchanged"} {
		session, err := local.GetSessionFull(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, session)
		session.FileHash = new("updated")
		if id == "growing" {
			session.MessageCount = 2
		}
		require.NoError(t, local.UpsertSession(ctx, *session))
	}
	quoted, err := quoteIdentifier(schema)
	require.NoError(t, err)
	connStr, err := appendConnParams(url, map[string]string{"search_path": quoted, "TimeZone": "UTC"})
	require.NoError(t, err)
	config, err := pgx.ParseConfig(connStr)
	require.NoError(t, err)
	var received atomic.Int64
	config.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, conn net.Conn) (net.Conn, error) {
		return &pushResponseConn{Conn: conn, received: &received}, nil
	}
	metered := stdlib.OpenDB(*config)
	metered.SetMaxOpenConns(5)
	metered.SetMaxIdleConns(5)
	require.NoError(t, syncer.pg.Close())
	syncer.pg = metered
	require.NoError(t, metered.PingContext(ctx))
	received.Store(0)
	result, err := syncer.Push(ctx, false, nil)
	bytes := received.Load()
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	assert.False(t, result.Full, "normal push history must select an incremental run")
	assert.Equal(t, 2, result.SessionsPushed, "both changed file hashes should reach publication")
	assert.Equal(t, 2, result.MessagesPushed, "only the growing transcript should be replaced")
	assert.Equal(t, before, pgMessageCTID(t, syncer.pg, "unchanged", 0))
	t.Logf("PostgreSQL received bytes with %d-byte fields: %d", size, bytes)
	return bytes
}
