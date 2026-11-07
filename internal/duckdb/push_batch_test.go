//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushBatchedDependentsPreserveValues(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	write := duckBatchFixture("batch-values", 203)
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{write})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	sess, err := local.GetSession(ctx, write.Session.ID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	_, err = syncer.pushSingleSession(ctx, *sess, "initial")
	require.NoError(t, err)
	assertDuckBatchParity(t, local, syncer.DB(), sess.ID)
	var summary string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT result_content FROM tool_calls WHERE session_id = ? AND tool_use_id = ?`,
		sess.ID, "row-102-call-0",
	).Scan(&summary))
	assert.Equal(t, strings.Repeat("summary\u03bb", 160000)+"row-102 summary", summary)

	replacement := duckBatchFixture(sess.ID, 1)
	replacement.Messages[0].ToolCalls = nil
	replacement.Messages[0].HasToolUse = false
	replacement.UsageEvents = nil
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{replacement})
	require.NoError(t, err)
	sess, err = local.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	_, err = syncer.pushSingleSession(ctx, *sess, "replacement")
	require.NoError(t, err)
	assertDuckBatchParity(t, local, syncer.DB(), sess.ID)
	assertDuckDBCount(t, syncer.DB(), "messages", 1)
	for _, table := range []string{"tool_calls", "tool_result_events", "usage_events"} {
		assertDuckDBCount(t, syncer.DB(), table, 0)
	}
}

func TestDuckAppenderPayloadBounds(t *testing.T) {
	const byteLimit = 1 << 20
	first, second := strings.Repeat("a", 614400), strings.Repeat("b", 614400)
	exactPrefix := strings.Repeat("e", byteLimit-4)
	oversized := strings.Repeat("o", byteLimit+1)
	empty := ""
	var null *string
	smallRows := make([]any, 101)
	wantSmallRows := make([]sql.NullString, 101)
	for i := range smallRows {
		smallRows[i] = "s\u03bb"
		wantSmallRows[i] = sql.NullString{String: "s\u03bb", Valid: true}
	}
	for _, tc := range []struct {
		name         string
		values       []any
		wantValues   []sql.NullString
		wantRows     []int
		wantPayloads []int
	}{
		{name: "empty"},
		{
			name: "100-small-rows", values: smallRows[:100], wantValues: wantSmallRows[:100],
			wantRows: []int{100}, wantPayloads: []int{300},
		},
		{
			name: "101-small-rows", values: smallRows, wantValues: wantSmallRows,
			wantRows: []int{101}, wantPayloads: []int{303},
		},
		{
			name: "combined-byte-overflow", values: []any{first, second},
			wantValues: []sql.NullString{{String: first, Valid: true}, {String: second, Valid: true}},
			wantRows:   []int{1, 1}, wantPayloads: []int{614400, 614400},
		},
		{
			name: "exact-byte-limit-and-tail", values: []any{exactPrefix, "edge", "tail"},
			wantValues: []sql.NullString{{String: exactPrefix, Valid: true}, {String: "edge", Valid: true}, {String: "tail", Valid: true}},
			wantRows:   []int{2, 1}, wantPayloads: []int{byteLimit, 4},
		},
		{
			name: "small-oversized-small", values: []any{"a", oversized, "b"},
			wantValues: []sql.NullString{{String: "a", Valid: true}, {String: oversized, Valid: true}, {String: "b", Valid: true}},
			wantRows:   []int{1, 1, 1}, wantPayloads: []int{1, byteLimit + 1, 1},
		},
		{
			name: "nullable-pointer-payload", values: []any{&first, &second, null, &empty, nil, ""},
			wantValues: []sql.NullString{{String: first, Valid: true}, {String: second, Valid: true}, {}, {Valid: true}, {}, {Valid: true}},
			wantRows:   []int{1, 5}, wantPayloads: []int{614400, 614400},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			conn, err := Open(ctx, ":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			_, err = conn.ExecContext(ctx, `CREATE TABLE batch_bounds (id INTEGER PRIMARY KEY, payload VARCHAR)`)
			require.NoError(t, err)
			var statementRows, statementPayloads []int
			err = func() error {
				sqlConn, err := conn.Conn(ctx)
				if err != nil {
					return err
				}
				defer sqlConn.Close()
				return sqlConn.Raw(func(raw any) (err error) {
					count, payload, nextPayload := 0, 0, 0
					batch, err := newDuckAppendBatch(ctx, raw, map[string][]string{"batch_bounds": {"id", "payload"}}, func(_ string, flushed bool) error {
						if flushed && count > 0 {
							statementRows = append(statementRows, count)
							statementPayloads = append(statementPayloads, payload)
							count, payload = 0, 0
						}
						if !flushed {
							count++
							payload += nextPayload
						}
						return nil
					})
					if err != nil {
						return err
					}
					defer func() { err = errors.Join(err, batch.close(ctx)) }()
					for i, value := range tc.values {
						nextPayload = len(tc.wantValues[i].String)
						if err := batch.add(ctx, "batch_bounds", i, value); err != nil {
							return err
						}
					}
					return batch.flush(ctx)
				})
			}()
			require.NoError(t, err)
			assert.Equal(t, tc.wantRows, statementRows)
			assert.Equal(t, tc.wantPayloads, statementPayloads)

			rows, err := conn.QueryContext(ctx, `SELECT id, payload FROM batch_bounds ORDER BY id`)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var stored []sql.NullString
			for rows.Next() {
				var id int
				var value sql.NullString
				require.NoError(t, rows.Scan(&id, &value))
				assert.Equal(t, len(stored), id)
				stored = append(stored, value)
			}
			require.NoError(t, rows.Err())
			assert.Equal(t, tc.wantValues, stored)
		})
	}
}

func TestPushBatchedDependentsRollbackAndRetry(t *testing.T) {
	for _, table := range []string{"messages", "tool_calls", "tool_result_events", "usage_events"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", table, canceled), func(t *testing.T) {
				ctx := t.Context()
				local := newLocalDB(t)
				old := duckBatchFixture("batch-rollback", 1)
				_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{old})
				require.NoError(t, err)
				syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
				require.NoError(t, createSchema(ctx, syncer.DB()))
				sess, err := local.GetSession(ctx, old.Session.ID)
				require.NoError(t, err)
				_, err = syncer.pushSingleSession(ctx, *sess, "before")
				require.NoError(t, err)
				before := duckBatchSnapshot(t, syncer.DB(), sess.ID)

				updated := duckBatchFixture(sess.ID, 203)
				updated.UsageEvents[0].DedupKey = strings.Repeat("usage", 220000)
				updated.UsageEvents[1].DedupKey = strings.Repeat("usage", 220000) + "second"
				_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{updated})
				require.NoError(t, err)
				sess, err = local.GetSession(ctx, sess.ID)
				require.NoError(t, err)
				attemptCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				failure := errors.New("injected dependent write failure")
				var injected bool
				flushed := false
				_, err = syncer.writeSessionBatch(attemptCtx, []db.Session{*sess}, map[string]string{sess.ID: "after"}, func(name string, flush bool) error {
					if name != table {
						return nil
					}
					if flush {
						flushed = true
						return nil
					}
					if flushed {
						injected = true
						if canceled {
							cancel()
							return attemptCtx.Err()
						}
						return failure
					}
					return nil
				})
				require.True(t, injected, "the failure must follow one real inserted chunk: %v", err)
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, failure)
				}
				assert.Equal(t, before, duckBatchSnapshot(t, syncer.DB(), sess.ID))
				var fingerprint string
				require.NoError(t, syncer.DB().QueryRowContext(ctx,
					`SELECT agentsview_push_fingerprint FROM sessions WHERE id = ?`, sess.ID,
				).Scan(&fingerprint))
				assert.Equal(t, "before", fingerprint)

				_, err = syncer.pushSingleSession(ctx, *sess, "after")
				require.NoError(t, err)
				assertDuckBatchParity(t, local, syncer.DB(), sess.ID)
				require.NoError(t, syncer.DB().QueryRowContext(ctx,
					`SELECT agentsview_push_fingerprint FROM sessions WHERE id = ?`, sess.ID,
				).Scan(&fingerprint))
				assert.Equal(t, "after", fingerprint)
			})
		}
	}
}

func duckBatchFixture(id string, count int) db.SessionBatchWrite {
	const ts = "2026-01-02T03:04:05.123Z"
	write := db.SessionBatchWrite{
		Session:     syncSession(id, "batch-fixture", "generated", ts, count),
		DataVersion: 1, ReplaceMessages: true,
	}
	for i := range count {
		key := fmt.Sprintf("row-%03d", i)
		content := key + " ' quoted ? Unicode \u03bb\n"
		if i == 99 || i == 100 {
			content = strings.Repeat("payload\u03bb", 140000) + key
		}
		dialogue := key + " dialogue"
		if i%3 == 1 {
			dialogue = ""
		}
		msg := syncMessage(id, i, "assistant", content, ts)
		if i%3 != 0 {
			msg.DialogueText = &dialogue
		}
		msg.ThinkingText, msg.HasThinking = key+" thinking", i%2 == 0
		msg.IsSystem, msg.IsSidechain, msg.IsCompactBoundary = i%2 == 0, i%3 == 0, i%5 == 0
		msg.Model, msg.ReasoningEffort, msg.ProviderID = key+" model", "high", key+" provider"
		msg.ClaudeMessageID, msg.ClaudeRequestID = key+" message", key+" request"
		msg.SourceType, msg.SourceSubtype, msg.PromptSource = "assistant", "fixture", key+" prompt"
		msg.SourceUUID, msg.SourceParentUUID = key+" uuid", key+" parent"
		msg.ContextTokens, msg.OutputTokens = i, i+7
		for j := range 2 {
			result := fmt.Sprintf("%s result-%d", key, j)
			if i == 101 && j == 0 {
				result = strings.Repeat("result\u03bb", 160000)
			}
			outcome, repeat, ending := "success", "", "verified"
			tc := db.ToolCall{
				ToolName: "Read", Category: "read", ToolUseID: fmt.Sprintf("%s-call-%d", key, j),
				InputJSON: `{"path":"fixture.go"}`, SkillName: key + " skill",
				FilePath: key + ".go", SubagentSessionID: key + " child",
				ResultContent: result, ResultContentLength: len(result),
			}
			if i == 102 && j == 0 {
				tc.ResultContent = strings.Repeat("summary\u03bb", 160000) + "row-102 summary"
				tc.ResultContentLength = len(tc.ResultContent)
			}
			if j == 0 {
				tc.ObservedOutcome, tc.ObservedRepeat, tc.SequenceEnding = &outcome, &repeat, &ending
			} else {
				tc.InputJSON, tc.SkillName, tc.FilePath, tc.SubagentSessionID = "", "", "", ""
			}
			tc.ResultEvents = []db.ToolResultEvent{
				{ToolUseID: tc.ToolUseID, Source: "tool_execution", Status: "started", Timestamp: ts, EventIndex: 0},
				{
					ToolUseID: tc.ToolUseID, AgentID: key + " agent", SubagentSessionID: tc.SubagentSessionID,
					Source: "tool_result", Status: "completed", Content: result, ContentLength: len(result), Timestamp: ts, EventIndex: 1,
				},
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
		msg.HasToolUse = true
		write.Messages = append(write.Messages, msg)
		ev := db.UsageEvent{
			SessionID: id, Source: "fixture", Model: key + " model", ProviderID: key + " provider",
			InputTokens: i, OutputTokens: i + 1, CacheCreationInputTokens: i + 2,
			CacheReadInputTokens: i + 3, ReasoningTokens: i + 4, DedupKey: key,
		}
		if i%2 == 0 {
			ordinal := i
			cost := money.Money{Microdollars: int64(i) * 1000001}
			ev.MessageOrdinal, ev.Cost, ev.OccurredAt = &ordinal, &cost, ts
			ev.CostStatus, ev.CostSource = "reported", "fixture"
		}
		write.UsageEvents = append(write.UsageEvents, ev)
	}
	return write
}

var duckBatchProjections = []struct{ table, columns, order string }{
	{"messages", `id, session_id, ordinal, role, content, thinking_text, dialogue_text,
		timestamp, has_thinking, has_tool_use, content_length, is_system, model,
		reasoning_effort, token_usage, context_tokens, output_tokens, provider_id,
		has_context_tokens, has_output_tokens, claude_message_id, claude_request_id,
		source_type, source_subtype, prompt_source, source_uuid, source_parent_uuid,
		is_sidechain, is_compact_boundary`, "ordinal"},
	{"tool_calls", `message_id, session_id, tool_name, category, call_index,
		tool_use_id, input_json, skill_name, result_content_length, result_content,
		subagent_session_id, file_path, observed_outcome, observed_repeat, sequence_ending`, "message_id, call_index"},
	{"tool_result_events", `session_id, tool_call_message_ordinal, call_index,
		tool_use_id, agent_id, subagent_session_id, source, status, content,
		content_length, timestamp, event_index`, "tool_call_message_ordinal, call_index, event_index"},
	{"usage_events", `id, session_id, message_ordinal, source, model, provider_id,
		input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		reasoning_tokens, cost_microdollars, cost_status, cost_source, occurred_at, dedup_key`, "id"},
}

func assertDuckBatchParity(t *testing.T, local *db.DB, mirror *sql.DB, sessionID string) {
	t.Helper()
	got := duckBatchSnapshot(t, mirror, sessionID)
	for _, projection := range duckBatchProjections {
		rows, err := local.Reader().Query(t.Context(), "SELECT "+projection.columns+
			" FROM "+projection.table+" WHERE session_id = ? ORDER BY "+projection.order, sessionID)
		require.NoError(t, err)
		assert.Equal(t, duckBatchRows(t, rows), got[projection.table], projection.table)
	}
}

func duckBatchSnapshot(t *testing.T, mirror *sql.DB, sessionID string) map[string][][]any {
	t.Helper()
	result := make(map[string][][]any)
	for _, projection := range duckBatchProjections {
		rows, err := mirror.QueryContext(t.Context(), "SELECT "+projection.columns+
			" FROM "+projection.table+" WHERE session_id = ? ORDER BY "+projection.order, sessionID)
		require.NoError(t, err)
		result[projection.table] = duckBatchRows(t, rows)
	}
	return result
}

func duckBatchRows(t *testing.T, rows *sql.Rows) [][]any {
	t.Helper()
	defer func() { require.NoError(t, rows.Close()) }()
	columns, err := rows.Columns()
	require.NoError(t, err)
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		for i, value := range values {
			switch value := value.(type) {
			case []byte:
				values[i] = string(value)
			case int32:
				values[i] = int64(value)
			case bool:
				values[i] = int64(0)
				if value {
					values[i] = int64(1)
				}
			case time.Time:
				values[i] = value.UTC().Format(time.RFC3339Nano)
			}
			if (columns[i] == "timestamp" || columns[i] == "occurred_at") && values[i] != nil {
				parsed, err := time.Parse(time.RFC3339Nano, values[i].(string))
				require.NoError(t, err)
				values[i] = parsed.UTC().Format(time.RFC3339Nano)
			}
		}
		result = append(result, values)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestPushReplacementClearsAllSessionRows(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	write := duckBatchFixture("replace-all", 2)
	write.PlanUsage = duckPlanUsage(12.5)
	write.Findings = []db.SecretFinding{{SessionID: write.Session.ID, RuleName: "fixture", Confidence: "definite", LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 3, RedactedMatch: "row", RulesVersion: "fixture"}}

	write.Messages[0].IsSystem = false
	write.Messages[0].IsSidechain = false
	write.Messages[1].SourceType = ""
	write.Messages[1].SourceSubtype = ""
	write.Messages[0].Role = "user"
	write.Messages[1].ToolCalls = []db.ToolCall{{ResultContent: "[main abc1234567890123456789012345678901234567] Save the change", ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m change"}`, ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234567890123456789012345678901234567] Save the change"}}}}

	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{write})
	require.NoError(t, err)
	msgs, err := local.GetAllMessages(ctx, write.Session.ID)
	require.NoError(t, err)
	_, err = local.PinMessage(ctx, write.Session.ID, msgs[0].ID, nil)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	sess, err := local.GetSession(ctx, write.Session.ID)
	require.NoError(t, err)
	_, err = syncer.pushSingleSession(ctx, *sess, "before")
	require.NoError(t, err)
	tables := []string{"messages", "tool_calls", "tool_result_events", "usage_events", "session_plan_usage", "commit_links", "secret_findings", "pinned_messages"}
	for _, table := range tables {
		var count int
		require.NoError(t, syncer.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Positive(t, count, table)
	}
	require.NoError(t, local.UnpinMessage(ctx, sess.ID, msgs[0].ID))
	replacement := duckBatchFixture(sess.ID, 0)
	replacement.Session.Project = "replacement"
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{replacement})
	require.NoError(t, err)
	sess, err = local.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	_, err = syncer.pushSingleSession(ctx, *sess, "after")
	require.NoError(t, err)
	for _, table := range tables {
		assertDuckDBCount(t, syncer.DB(), table, 0)
	}
	assertDuckDBCount(t, syncer.DB(), "sessions", 1)
	var project, fingerprint string
	require.NoError(t, syncer.DB().QueryRowContext(ctx, "SELECT project, agentsview_push_fingerprint FROM sessions").Scan(&project, &fingerprint))
	assert.Equal(t, "replacement", project)
	assert.Equal(t, "after", fingerprint)
}

func TestPushDuckDBTimestampCastParity(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	write := duckBatchFixture("timestamp-cast", 1)
	// DuckDB accepts this offset and space separator, while parseTimestamp
	// deliberately leaves it for the SQL cast.
	const raw = "2026-01-02 03:04:05.123456+02:00"
	_, parsed := parseTimestamp(raw)
	require.False(t, parsed)
	write.Session.StartedAt = new(raw)
	write.Session.CreatedAt = raw
	write.Messages[0].Timestamp = raw
	write.Messages[0].ToolCalls[0].ResultEvents[0].Timestamp = raw
	write.UsageEvents[0].OccurredAt = raw
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{write})
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	sess, err := local.GetSession(ctx, write.Session.ID)
	require.NoError(t, err)
	sess.CreatedAt = raw
	_, err = syncer.pushSingleSession(ctx, *sess, "timestamp")
	require.NoError(t, err)
	for _, field := range []struct{ table, column string }{{"sessions", "started_at"}, {"sessions", "created_at"}, {"messages", "timestamp"}, {"tool_result_events", "timestamp"}, {"usage_events", "occurred_at"}} {
		var got, sqlCast time.Time
		require.NoError(t, syncer.DB().QueryRowContext(ctx, "SELECT "+field.column+", CAST(? AS TIMESTAMP) FROM "+field.table+" LIMIT 1", raw).Scan(&got, &sqlCast))
		assert.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC), got, field)
		assert.Equal(t, sqlCast, got, field)
	}
}
