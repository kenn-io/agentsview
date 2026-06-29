package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type trendsProbeDriver struct{}

type trendsProbeConn struct {
	state *trendsProbeState
}

type trendsProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type trendsProbeState struct {
	mu      sync.Mutex
	queries []string
}

var (
	trendsProbeRegisterOnce sync.Once
	trendsProbeStatesMu     sync.Mutex
	trendsProbeStates       = map[string]*trendsProbeState{}
)

func newTrendsProbeDB(
	t *testing.T, state *trendsProbeState,
) *sql.DB {
	t.Helper()
	trendsProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_trends_probe", trendsProbeDriver{})
	})
	name := t.Name()
	trendsProbeStatesMu.Lock()
	trendsProbeStates[name] = state
	trendsProbeStatesMu.Unlock()
	t.Cleanup(func() {
		trendsProbeStatesMu.Lock()
		delete(trendsProbeStates, name)
		trendsProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_trends_probe", name)
	require.NoError(t, err, "open trends probe db")
	t.Cleanup(func() { pg.Close() })
	return pg
}

func (trendsProbeDriver) Open(name string) (driver.Conn, error) {
	trendsProbeStatesMu.Lock()
	state := trendsProbeStates[name]
	trendsProbeStatesMu.Unlock()
	return &trendsProbeConn{state: state}, nil
}

func (c *trendsProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *trendsProbeConn) Close() error { return nil }

func (c *trendsProbeConn) Begin() (driver.Tx, error) {
	return nil, driver.ErrSkip
}

func (c *trendsProbeConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.mu.Unlock()
	return &trendsProbeRows{
		columns: []string{"content", "timestamp", "started_at", "created_at"},
		values:  nil,
	}, nil
}

func (r *trendsProbeRows) Columns() []string { return r.columns }

func (r *trendsProbeRows) Close() error { return nil }

func (r *trendsProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestGetTrendsTermsModelFilterTargetsOuterMessages(t *testing.T) {
	state := &trendsProbeState{}
	store := &Store{
		pg: newTrendsProbeDB(t, state),
	}
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err, "ParseTrendTerms")

	_, err = store.GetTrendsTerms(
		context.Background(),
		db.AnalyticsFilter{
			From: "2024-06-01", To: "2024-06-02",
			Timezone: "UTC",
			Model:    "gpt-4o",
		},
		terms,
		"day",
	)
	require.NoError(t, err, "GetTrendsTerms")
	require.NotEmpty(t, state.queries, "queries")

	query := strings.ToLower(strings.Join(state.queries, "\n"))
	assert.Contains(t, query, "join messages m on m.session_id = s.id")
	assert.Contains(t, query, "order by m.session_id, m.ordinal")
	assert.NotContains(t, query, "and m.model = $1")
	assert.NotContains(t, query, "exists (select 1 from messages")
}
