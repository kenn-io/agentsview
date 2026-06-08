package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type analyticsProbeDriver struct{}

type analyticsProbeConn struct {
	state *analyticsProbeState
}

type analyticsProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type analyticsProbeState struct {
	mu      sync.Mutex
	queries []string
}

var (
	analyticsProbeRegisterOnce sync.Once
	analyticsProbeStatesMu     sync.Mutex
	analyticsProbeStates       = map[string]*analyticsProbeState{}
)

func newAnalyticsProbeDB(
	t *testing.T, state *analyticsProbeState,
) *sql.DB {
	t.Helper()
	analyticsProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_analytics_probe", analyticsProbeDriver{})
	})
	name := t.Name()
	analyticsProbeStatesMu.Lock()
	analyticsProbeStates[name] = state
	analyticsProbeStatesMu.Unlock()
	t.Cleanup(func() {
		analyticsProbeStatesMu.Lock()
		delete(analyticsProbeStates, name)
		analyticsProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_analytics_probe", name)
	require.NoError(t, err, "open analytics probe db")
	t.Cleanup(func() { pg.Close() })
	return pg
}

func (analyticsProbeDriver) Open(name string) (driver.Conn, error) {
	analyticsProbeStatesMu.Lock()
	state := analyticsProbeStates[name]
	analyticsProbeStatesMu.Unlock()
	return &analyticsProbeConn{state: state}, nil
}

func (c *analyticsProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *analyticsProbeConn) Close() error { return nil }

func (c *analyticsProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (c *analyticsProbeConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.mu.Unlock()

	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "from sessions"):
		return &analyticsProbeRows{
			columns: []string{"id", "date", "agent"},
			values: [][]driver.Value{
				{"s1", time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC), "claude"},
				{"s2", time.Date(2024, 6, 4, 9, 0, 0, 0, time.UTC), "codex"},
			},
		}, nil
	case strings.Contains(normalized, "from tool_calls"):
		if !strings.Contains(normalized, "group by session_id, category") {
			return nil, errors.New("tool call query must group by session_id, category")
		}
		return &analyticsProbeRows{
			columns: []string{"session_id", "category", "count"},
			values: [][]driver.Value{
				{"s1", "Read", int64(2)},
				{"s1", "Bash", int64(1)},
				{"s2", "Read", int64(1)},
			},
		}, nil
	case strings.Contains(normalized, "from messages"):
		if strings.Contains(normalized, "to_char") {
			return nil, errors.New("velocity query must scan native timestamps")
		}
		return &analyticsProbeRows{
			columns: []string{
				"session_id", "ordinal", "role",
				"timestamp", "content_length",
			},
			values: [][]driver.Value{
				{
					"s1", int64(0), "user",
					time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC),
					int64(2),
				},
				{
					"s1", int64(1), "assistant",
					time.Date(2024, 6, 3, 9, 0, 10, 0, time.UTC),
					int64(5),
				},
			},
		}, nil
	default:
		return nil, errors.New("unexpected analytics query")
	}
}

func (r *analyticsProbeRows) Columns() []string { return r.columns }

func (r *analyticsProbeRows) Close() error { return nil }

func (r *analyticsProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestGetAnalyticsToolsAggregatesToolCallsInSQL(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}

	resp, err := store.GetAnalyticsTools(
		context.Background(),
		db.AnalyticsFilter{
			From: "2024-06-01",
			To:   "2024-06-30",
		},
	)
	require.NoError(t, err, "GetAnalyticsTools")

	assert.Equal(t, 4, resp.TotalCalls)
	require.NotEmpty(t, resp.ByCategory)
	assert.Equal(t, "Read", resp.ByCategory[0].Category)
	assert.Equal(t, 3, resp.ByCategory[0].Count)
}

func TestQueryVelocityMsgsScansNativeTimestamps(t *testing.T) {
	store := &Store{
		pg: newAnalyticsProbeDB(t, &analyticsProbeState{}),
	}
	sessionMsgs := map[string][]velocityMsg{}

	err := store.queryVelocityMsgs(
		context.Background(),
		[]string{"s1"},
		time.UTC,
		sessionMsgs,
	)
	require.NoError(t, err, "queryVelocityMsgs")

	require.Len(t, sessionMsgs["s1"], 2)
	assert.Equal(t, "assistant", sessionMsgs["s1"][1].role)
	assert.True(t, sessionMsgs["s1"][1].valid)
	assert.Equal(t, 10.0,
		sessionMsgs["s1"][1].ts.Sub(sessionMsgs["s1"][0].ts).Seconds())
}
