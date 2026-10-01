//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"slices"
	"strings"
	"testing"

	duckdbdriver "github.com/duckdb/duckdb-go/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// TestGetAllMessagesSkipsNegativeCallIndex guards against a panic when the
// DuckDB mirror holds a tool_calls or tool_result_events row with a negative
// call_index (a corrupt or malformed mirror row). Such a row would skip the
// grow loop / pass the upper-bound check and index ToolCalls[-1], crashing
// message loading with "index out of range [-1]". The Postgres store already
// guards callIndex < 0; the DuckDB store must behave the same way.
func TestGetAllMessagesSkipsNegativeCallIndex(t *testing.T) {
	ctx := t.Context()
	store, fixture := newSyncedStore(t)

	// alpha message ordinal 1 has exactly one real tool call ("search").
	// Inject malformed rows with call_index = -1 for that same message.
	_, err := store.duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category,
			call_index, tool_use_id
		)
		SELECT 90001, m.id, m.session_id, 'bad', 'other', -1, 'bad-tool'
		FROM messages m
		WHERE m.session_id = ? AND m.ordinal = 1`, fixture.alphaID)
	require.NoError(t, err)

	_, err = store.duck.ExecContext(ctx, `
		INSERT INTO tool_result_events (
			id, session_id, tool_call_message_ordinal, call_index,
			source, status, content, content_length, event_index
		) VALUES (90002, ?, 1, -1, 'tool', 'complete', 'bad', 3, 0)`,
		fixture.alphaID)
	require.NoError(t, err)

	// Must not panic; the negative-index rows are simply skipped.
	msgs, err := store.GetAllMessages(ctx, fixture.alphaID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	// The valid tool call and its result event are preserved intact.
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "search", msgs[1].ToolCalls[0].ToolName)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "duck result", msgs[1].ToolCalls[0].ResultEvents[0].Content)
}

// TestDuckMessageHydratesToolCallFilePathAndCallIndex mirrors the SQLite
// round-trip coverage (db.TestResolveToolCallsDerivesPositionalCallIndex):
// the DuckDB message hydrator must populate db.ToolCall.FilePath and
// CallIndex so GetMessages/GetAllMessages consumers see them at parity with
// SQLite.
func TestDuckMessageHydratesToolCallFilePathAndCallIndex(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "tc", Project: "p", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}), "upsert session")
	// One assistant message with three tool calls; the write path numbers
	// them positionally (0,1,2) and each carries a distinct file_path.
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: "tc", Ordinal: 0, Role: "assistant", Content: "tools",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{
			{ToolName: "Read", Category: "Read", FilePath: "a.go"},
			{ToolName: "Edit", Category: "Edit", FilePath: "b.go"},
			{ToolName: "Write", Category: "Write", FilePath: "c.go"},
		},
	}}), "insert messages")

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "push to duckdb mirror")
	store := NewStoreFromDB(syncer.DB())

	msgs, err := store.GetAllMessages(ctx, "tc")
	require.NoError(t, err, "get all messages")
	require.Len(t, msgs, 1)
	calls := msgs[0].ToolCalls
	require.Len(t, calls, 3)
	for i, tc := range calls {
		assert.Equal(t, i, tc.CallIndex, "call %d index", i)
	}
	assert.Equal(t, "a.go", calls[0].FilePath)
	assert.Equal(t, "b.go", calls[1].FilePath)
	assert.Equal(t, "c.go", calls[2].FilePath)
}

// The wrapper counts real DuckDB result rows before database/sql scans or attachment.
type hydrationConnector struct {
	driver.Connector
	queries *[]*hydrationQuery
}

type hydrationConn struct {
	driver.Conn
	queries *[]*hydrationQuery
}

type hydrationQuery struct {
	query    string
	args     int
	ordinals []int64
	bytes    int
}

type hydrationRows struct {
	driver.Rows
	query *hydrationQuery
}

func (c hydrationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return hydrationConn{Conn: conn, queries: c.queries}, nil
}

func (c hydrationConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(query, "FROM tool_calls tc") && !strings.Contains(query, "FROM tool_result_events") {
		return rows, nil
	}
	observed := &hydrationQuery{query: query, args: len(args)}
	*c.queries = append(*c.queries, observed)
	return &hydrationRows{Rows: rows, query: observed}, nil
}

func (r *hydrationRows) Next(values []driver.Value) error {
	if err := r.Rows.Next(values); err != nil {
		return err
	}
	r.query.ordinals = append(r.query.ordinals, int64(values[0].(int32)))
	for _, value := range values {
		if text, ok := value.(string); ok {
			r.query.bytes += len(text)
		}
	}
	return nil
}

func TestMessageHydration(t *testing.T) {
	ctx := t.Context()
	connector, err := duckdbdriver.NewConnector("", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connector.Close()) })
	var queries []*hydrationQuery
	conn := sql.OpenDB(hydrationConnector{Connector: connector, queries: &queries})
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.NoError(t, createSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	local := newLocalDB(t)
	seedDuckWindowMessages(t, local, "hydration")
	messages, err := local.GetAllMessages(ctx, "hydration")
	require.NoError(t, err)
	for _, message := range messages {
		_, err = conn.ExecContext(ctx, `INSERT INTO messages (id,session_id,ordinal,role,content,timestamp) VALUES (?,?,?,?,?,'2026-01-01 00:00:00')`, message.Ordinal+1, "hydration", message.Ordinal, message.Role, "msg")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `INSERT INTO tool_calls (id,message_id,session_id,call_index,tool_name,category,input_json,result_content_length) VALUES (?,?,?,0,'Read','Read','x',1)`, message.Ordinal+1, message.Ordinal+1, "hydration")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `INSERT INTO tool_result_events (id,session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,event_index,timestamp) VALUES (?, ?, ?,0,'tool','completed','r',1,0,'2026-01-02 00:00:00')`, message.Ordinal+1, "hydration", message.Ordinal)
		require.NoError(t, err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO tool_result_events (id,session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,event_index,timestamp) VALUES (999,'hydration',2,0,'tool','completed','later',5,1,'2026-01-03 00:00:00')`)
	require.NoError(t, err)
	from, anchor, empty := 7, 4, 9000
	for _, tc := range []struct {
		name   string
		window db.MessageWindow
		want   []int
	}{
		{"sparse", db.MessageWindow{Limit: 2, Asc: true, Roles: []string{"user"}}, []int{0, 2}},
		{"descending", db.MessageWindow{From: &from, Limit: 2, Roles: []string{"user"}}, []int{7, 5}},
		{"around", db.MessageWindow{Around: &anchor, Before: 1, After: 1, Roles: []string{"user"}}, []int{2, 4, 5}},
		{"empty", db.MessageWindow{From: &empty, Asc: true, Limit: 2}, []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []int
			for round := range 2 {
				queries = nil
				msgs, err := store.GetMessagesWindow(ctx, "hydration", tc.window)
				require.NoError(t, err)
				require.Equal(t, tc.want, duckOrdinalsOf(msgs))
				if len(tc.want) == 0 {
					require.Empty(t, queries)
					continue
				}
				require.Len(t, queries, 2)
				want := make([]int64, 0, len(tc.want)+1)
				for _, ordinal := range tc.want {
					want = append(want, int64(ordinal))
				}
				assert.ElementsMatch(t, want, queries[0].ordinals)
				if slices.Contains(tc.want, 2) {
					want = append(want, 2)
				}
				assert.ElementsMatch(t, want, queries[1].ordinals)
				for _, msg := range msgs {
					require.Len(t, msg.ToolCalls, 1)
					if msg.Ordinal == 2 {
						require.Len(t, msg.ToolCalls[0].ResultEvents, 2)
						assert.Equal(t, []string{"r", "later"}, []string{msg.ToolCalls[0].ResultEvents[0].Content, msg.ToolCalls[0].ResultEvents[1].Content})
						assert.Equal(t, []int{1, 5}, []int{msg.ToolCalls[0].ResultEvents[0].ContentLength, msg.ToolCalls[0].ResultEvents[1].ContentLength})
						assert.Equal(t, []int{0, 1}, []int{msg.ToolCalls[0].ResultEvents[0].EventIndex, msg.ToolCalls[0].ResultEvents[1].EventIndex})
						assert.Contains(t, msg.ToolCalls[0].ResultEvents[1].Timestamp, "2026-01-03")
					} else {
						assert.Equal(t, "r", msg.ToolCalls[0].ResultContent)
					}
				}
				bytes := []int{queries[0].bytes, queries[1].bytes}
				if round == 0 {
					before = bytes
					_, err = conn.ExecContext(ctx, `UPDATE tool_calls SET input_json=repeat('x',1048576) WHERE session_id='hydration' AND message_id=12`)
					require.NoError(t, err)
					_, err = conn.ExecContext(ctx, `UPDATE tool_result_events SET content=repeat('z',1048576),content_length=1048576 WHERE session_id='hydration' AND tool_call_message_ordinal=11`)
					require.NoError(t, err)
				} else {
					assert.Equal(t, before, bytes)
				}
				t.Logf("round=%d call rows=%d event rows=%d returned text bytes=%v", round, len(queries[0].ordinals), len(queries[1].ordinals), bytes)
			}
		})
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (id,session_id,ordinal,role,content) SELECT range+1000,'hydration',range+1000,'assistant','msg' FROM range(1001)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO tool_calls (id,message_id,session_id,call_index,tool_name,category,input_json) SELECT range+1000,range+1000,'hydration',0,'Read','Read','x' FROM range(1001)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO tool_result_events (id,session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,event_index) SELECT range+1000,'hydration',range+1000,0,'tool','completed','r',1,0 FROM range(1001)`)
	require.NoError(t, err)
	queries = nil
	all, err := store.GetAllMessages(ctx, "hydration")
	require.NoError(t, err)
	require.Len(t, all, 1013)
	require.Len(t, queries, 6)
	var calls, events int
	for _, q := range queries {
		assert.LessOrEqual(t, q.args, 501)
		assert.LessOrEqual(t, len(q.ordinals), 501)
		if strings.Contains(q.query, "FROM tool_calls tc") {
			calls += len(q.ordinals)
		} else {
			events += len(q.ordinals)
		}
	}
	assert.Equal(t, 1013, calls)
	assert.Equal(t, 1014, events)
	for _, msg := range all {
		require.Len(t, msg.ToolCalls, 1)
		require.NotEmpty(t, msg.ToolCalls[0].ResultEvents)
	}
	t.Logf("full read call rows=%d event rows=%d hydration queries=%d, maximum 501 bound args", calls, events, len(queries))
}
