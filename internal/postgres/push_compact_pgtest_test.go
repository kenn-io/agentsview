//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync/atomic"
	"testing"

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
	const schema = "agentsview_compact_test"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	body := strings.Repeat("large-payload-λ", 10000)
	messages := []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: body, ThinkingText: body, HasThinking: true, HasToolUse: true, Timestamp: "2026-10-01T10:00:00.123456789Z", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: "call", InputJSON: body, ResultContent: body, ResultContentLength: len(body), FilePath: body, ResultEvents: []db.ToolResultEvent{{Content: body + "event", ContentLength: len(body) + 5, Source: "tool_result", Status: "completed"}}}, {ToolName: "Empty"}}}}
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
	aggregate.Count = 2
	mismatchedCounts[id] = aggregate
	skipped, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, mismatchedCounts)
	require.NoError(t, err)
	assert.Equal(t, 1, skipped.MessageAggregates[id].Count)
	for _, fingerprints := range []map[string]string{skipped.MessageContentHash, skipped.MessageRoleTime, skipped.MessageFlags, skipped.MessageSystemOrdinals, skipped.MessageTokenFingerprint, skipped.ToolCallFingerprint, skipped.ToolResultFingerprint} {
		assert.NotContains(t, fingerprints, id)
	}
	compact, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, counts)
	require.NoError(t, err)
	assert.Contains(t, compact.MessageContentHash, id)
	assert.Contains(t, compact.ToolCallFingerprint, id)
	localFP, err := localPushMessageFingerprint(ctx, local, id, "", false)
	require.NoError(t, err)
	calls, err := local.ToolCallComparisonFingerprints(ctx, []string{id})
	require.NoError(t, err)
	localFP.ToolCallFP = calls[id]
	assert.True(t, shouldSkipSessionMessages(id, 1, localFP, false, compact))
	for _, change := range []struct{ name, statement string }{
		{"content", `UPDATE messages SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
		{"thinking", `UPDATE messages SET thinking_text = 'x' || substring(thinking_text from 2) WHERE session_id = $1`},
		{"input", `UPDATE tool_calls SET input_json = 'x' || substring(input_json from 2) WHERE session_id = $1 AND call_index = 0`},
		{"summary", `UPDATE tool_calls SET result_content = 'x' || substring(result_content from 2) WHERE session_id = $1 AND call_index = 0`},
		{"path", `UPDATE tool_calls SET file_path = 'x' || substring(file_path from 2) WHERE session_id = $1 AND call_index = 0`},
		{"event", `UPDATE tool_result_events SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, err := tx.ExecContext(ctx, `SAVEPOINT change`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, change.statement, id)
			require.NoError(t, err)
			changed, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, counts)
			require.NoError(t, err)
			assert.False(t, shouldSkipSessionMessages(id, 1, localFP, false, changed))
			_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT change`)
			require.NoError(t, err)
		})
	}
}

func TestPushCompactLegacyTextRewrittenOnce(t *testing.T) {
	ctx := t.Context()
	const schema = "agentsview_compact_legacy_test"
	const id = "legacy"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "clean\x01text"}}))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE messages SET content = $1 WHERE session_id = $2`, "clean\x01text", id)
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
	var content string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT content FROM messages WHERE session_id=$1`, id).Scan(&content))
	assert.Equal(t, "cleantext", content)
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

func TestPushCompactMixedResponseBytes(t *testing.T) {
	var measured []int64
	for _, size := range []int{4 << 10, 256 << 10} {
		t.Run(fmt.Sprintf("%d_bytes", size), func(t *testing.T) {
			ctx := t.Context()
			const schema = "agentsview_push_response_bytes_test"
			url := testPGURL(t)
			cleanNamedPGSchema(t, url, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			local := testDB(t)
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "sample-model"}}))
			require.NoError(t, local.UpsertGenAIPricing(ctx, db.GenAIPricingDocument{
				Version: "test", SourceRef: "test", Source: db.GenAIPricingSourceFetched, Data: []byte(`[]`),
			}))
			for _, id := range []string{"growing", "unchanged", "edited"} {
				count := 1
				if id == "growing" {
					count = 2000
				}
				require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: count}))
			}
			growing := make([]db.Message, 2000)
			for i := range growing {
				growing[i] = db.Message{SessionID: "growing", Ordinal: i, Role: "user", Content: "small message"}
			}
			require.NoError(t, local.InsertMessages(ctx, growing))
			body := strings.Repeat("c", size)
			edited := []db.Message{{SessionID: "edited", Ordinal: 0, Role: "assistant", Content: body, ThinkingText: strings.Repeat("t", size), HasThinking: true, HasToolUse: true,
				ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: "call", InputJSON: strings.Repeat("i", size), ResultContent: strings.Repeat("r", size), ResultContentLength: size, FilePath: "example.txt",
					ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Status: "completed", Content: strings.Repeat("e", size), ContentLength: size}},
				}},
			}}
			require.NoError(t, local.InsertMessages(ctx, edited))
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
			var pricingRows int
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM model_pricing_bands) + (SELECT COUNT(*) FROM model_pricing) + (SELECT COUNT(*) FROM genai_pricing)`).Scan(&pricingRows))
			require.Zero(t, pricingRows)
			before := pgMessageCTID(t, syncer.pg, "unchanged", 0)
			growing = append(growing, db.Message{SessionID: "growing", Ordinal: 2000, Role: "user", Content: "appended"})
			require.NoError(t, local.ReplaceSessionMessages(ctx, "growing", growing))
			require.NoError(t, local.RenameSession(ctx, "unchanged", new("renamed session")))
			edited[0].Content = "x" + body[1:]
			require.NoError(t, local.ReplaceSessionMessages(ctx, "edited", edited))
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
			assert.Equal(t, 3, result.SessionsPushed)
			assert.Positive(t, bytes)
			assert.Less(t, bytes, int64(640<<10), "whole-Push cap includes row-count-dependent suffix hashes")
			measured = append(measured, bytes)
			t.Logf("PostgreSQL received bytes with %d-byte fields: %d", size, bytes)
			assert.Equal(t, before, pgMessageCTID(t, syncer.pg, "unchanged", 0))
			var appended, name, content string
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT content FROM messages WHERE session_id='growing' AND ordinal=2000`).Scan(&appended))
			assert.Equal(t, "appended", appended)
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT display_name FROM sessions WHERE id='unchanged'`).Scan(&name))
			assert.Equal(t, "renamed session", name)
			require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT content FROM messages WHERE session_id='edited' AND ordinal=0`).Scan(&content))
			assert.Equal(t, "x"+body[1:], content)
		})
	}
	require.Len(t, measured, 2)
	assert.LessOrEqual(t, measured[1]-measured[0], int64(32<<10), "response traffic must not grow with transcript bodies")
}
