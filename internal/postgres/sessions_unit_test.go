package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sessionSuffixProbeState struct {
	queries []string
}

type sessionSuffixProbeConnector struct {
	state *sessionSuffixProbeState
}

func (c sessionSuffixProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return &sessionSuffixProbeConn{state: c.state}, nil
}

func (sessionSuffixProbeConnector) Driver() driver.Driver {
	return sessionSuffixProbeDriver{}
}

type sessionSuffixProbeDriver struct{}

func (sessionSuffixProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("session suffix probe requires connector")
}

type sessionSuffixProbeConn struct {
	state *sessionSuffixProbeState
}

func (*sessionSuffixProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (*sessionSuffixProbeConn) Close() error { return nil }

func (*sessionSuffixProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func (c *sessionSuffixProbeConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.queries = append(c.state.queries, query)
	return &sessionSuffixProbeRows{
		values: []string{
			"kimi:project-hash:session-uuid",
			"openclaw:project-hash:session-uuid",
		},
	}, nil
}

type sessionSuffixProbeRows struct {
	values []string
	next   int
}

func (*sessionSuffixProbeRows) Columns() []string { return []string{"id"} }

func (*sessionSuffixProbeRows) Close() error { return nil }

func (r *sessionSuffixProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	dest[0] = r.values[r.next]
	r.next++
	return nil
}

func TestPGFindSessionIDsByRawSuffixUsesExactFirstSuffixQuery(t *testing.T) {
	state := &sessionSuffixProbeState{}
	pg := sql.OpenDB(sessionSuffixProbeConnector{state: state})
	t.Cleanup(func() { _ = pg.Close() })
	store := newStore(pg)

	ids, err := store.FindSessionIDsByRawSuffix(
		context.Background(), "project-hash:session-uuid", 2,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"kimi:project-hash:session-uuid",
		"openclaw:project-hash:session-uuid",
	}, ids)

	require.NotEmpty(t, state.queries)
	query := strings.ToLower(state.queries[len(state.queries)-1])

	assert.Contains(t, query,
		"right(id, length('project-hash:session-uuid') + 1) = ':' || 'project-hash:session-uuid'")
	assert.Contains(t, query, "deleted_at is null")
	assert.Contains(t, query, "order by (id = 'project-hash:session-uuid') desc")
	assert.Contains(t, query, "coalesce(ended_at, started_at, created_at) desc")
}
