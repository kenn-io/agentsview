package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCallTimingRead(t *testing.T) {
	start := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		values []driver.Value
		want   *int64
	}{
		{"null", []driver.Value{1, 0, nil, nil, nil, nil, nil}, nil},
		{"zero", []driver.Value{1, 0, start, start, nil, nil, nil}, new(int64(0))},
		{"positive", []driver.Value{1, 0, start.Format(time.RFC3339), []byte(start.Add(time.Second).Format(time.RFC3339)), nil, nil, nil}, new(int64(1000))},
		{"reversed", []driver.Value{1, 0, start.Add(time.Second), start, nil, nil, nil}, nil},
		{"child precedence", []driver.Value{1, 0, start, start, start, start.Add(2 * time.Second), "child"}, new(int64(2000))},
		{"invalid child falls back", []driver.Value{1, 0, start, start.Add(time.Second), "bad", start, "child"}, new(int64(1000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &durationTestRows{values: tc.values}
			base := durationTestBase(t, rows)
			got, err := base.GetToolCallDurations(t.Context(), "session", []ToolCallPosition{{MessageOrdinal: 1}})
			require.NoError(t, err)
			assert.Equal(t, map[ToolCallPosition]*int64{{MessageOrdinal: 1}: tc.want}, got)
			assert.True(t, rows.closed)
		})
	}
	for _, failure := range []string{"query", "scan", "rows", "close", "presence"} {
		t.Run(failure, func(t *testing.T) {
			rows := &durationTestRows{values: []driver.Value{1, 0, nil, nil, nil, nil, nil}, failure: failure}
			if failure == "scan" {
				rows.values[0] = "invalid ordinal"
			}
			base := durationTestBase(t, rows)
			if failure == "presence" {
				base.SessionLookup = func(context.Context, string) (*Session, error) { return nil, errors.New("presence failed") }
			}
			got, err := base.GetToolCallDurations(t.Context(), "session", []ToolCallPosition{{MessageOrdinal: 1}})
			require.Error(t, err)
			assert.Nil(t, got)
			if failure != "query" && failure != "presence" {
				assert.True(t, rows.closed)
			}
		})
	}
	base := durationTestBase(t, &durationTestRows{failure: "query"})
	got, err := base.GetToolCallDurations(t.Context(), "session", nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got)
	base.SessionLookup = func(context.Context, string) (*Session, error) { return nil, nil }
	got, err = base.GetToolCallDurations(t.Context(), "session", nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestToolCallTimingRead_SQLiteMatchesFullTiming(t *testing.T) {
	d := testDB(t)
	timingInsertSession(t, d, "selected", "2026-04-26T10:00:00Z", "2026-04-26T10:00:30Z")
	for ordinal := range 25 {
		timingInsertMessage(t, d, "selected", ordinal, "assistant", "tool", "2026-04-26T10:00:00Z", true)
		timingInsertToolCall(t, d, "selected", timingMsgID(t, d, "selected", ordinal), "reused", "Bash", "Bash", "")
		timingInsertToolResultEvent(t, d, "selected", ordinal, 0, "reused", "started", "2026-04-26T10:00:00Z", 0)
		timingInsertToolResultEvent(t, d, "selected", ordinal, 0, "reused", "completed", "2026-04-26T10:00:02Z", 1)
	}
	full, err := d.GetSessionTiming(t.Context(), "selected")
	require.NoError(t, err)
	selected, err := d.GetToolCallDurations(t.Context(), "selected", []ToolCallPosition{{MessageOrdinal: 23}, {MessageOrdinal: 24}, {MessageOrdinal: 24, CallIndex: 1}})
	require.NoError(t, err)
	require.Len(t, selected, 2)
	assert.Equal(t, full.Turns[23].Calls[0].DurationMs, selected[ToolCallPosition{MessageOrdinal: 23}])
	assert.Equal(t, full.Turns[24].Calls[0].DurationMs, selected[ToolCallPosition{MessageOrdinal: 24}])
}

type durationTestRows struct {
	values       []driver.Value
	read, closed bool
	failure      string
}

func (r *durationTestRows) Columns() []string {
	return []string{"ordinal", "index", "start", "end", "child_start", "child_end", "child"}
}

func (r *durationTestRows) Close() error {
	r.closed = true
	if r.failure == "close" {
		return errors.New("close failed")
	}
	return nil
}

func (r *durationTestRows) Next(dest []driver.Value) error {
	if r.failure == "rows" {
		return errors.New("rows failed")
	}
	if r.read {
		return io.EOF
	}
	r.read = true
	copy(dest, r.values)
	return nil
}

type durationTestConnector struct{ rows *durationTestRows }

func (c durationTestConnector) Connect(context.Context) (driver.Conn, error) {
	return durationTestConn(c), nil
}
func (c durationTestConnector) Driver() driver.Driver { return durationTestDriver{} }

type durationTestDriver struct{}

func (durationTestDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type durationTestConn struct{ rows *durationTestRows }

func (durationTestConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("use query") }
func (durationTestConn) Close() error                        { return nil }
func (durationTestConn) Begin() (driver.Tx, error)           { return nil, errors.New("use query") }
func (c durationTestConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.rows.failure == "query" {
		return nil, errors.New("query failed")
	}
	return c.rows, nil
}

type durationTestQuery struct{ db *sql.DB }

func (q durationTestQuery) QueryToolCallDurationRows(ctx context.Context, _ string, _ []ToolCallPosition) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, "selected durations")
}

func durationTestBase(t *testing.T, rows *durationTestRows) ToolCallTimingReadBase {
	t.Helper()
	d := sql.OpenDB(durationTestConnector{rows})
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return ToolCallTimingReadBase{SessionLookup: func(context.Context, string) (*Session, error) { return &Session{}, nil }, Queries: durationTestQuery{d}}
}
