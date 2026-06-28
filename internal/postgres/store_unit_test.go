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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestStripFTSQuotes pins the de-quoting behavior the PostgreSQL Search path
// relies on. The canonical implementation lives in the db package and is
// shared with the SQLite and HTTP paths so the backends stay in parity.
func TestStripFTSQuotes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"hello world"`, "hello world"},
		{`hello`, "hello"},
		{`"error" "401"`, "error 401"},
		{`"error-401"`, "error-401"},
		{`""`, ""},
		{`"a"`, "a"},
		{`already unquoted`, "already unquoted"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, db.StripFTSQuotes(tt.input),
			"input=%q", tt.input)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"100%", `100\%`},
		{"under_score", `under\_score`},
		{`back\slash`, `back\\slash`},
		{`%_\`, `\%\_\\`},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, escapeLike(tt.input),
			"input=%q", tt.input)
	}
}

func TestPGMessagesBranchFTSRequiresAllTerms(t *testing.T) {
	pb := &paramBuilder{}
	branch := pgMessagesBranch(
		db.ContentSearchFilter{
			Pattern: "quick fox",
			Mode:    "fts",
		},
		escapeLike("quick fox"),
		pb,
	)

	assert.Contains(t, branch,
		"m.content ILIKE '%'||$1||'%' ESCAPE E'\\\\'")
	assert.Contains(t, branch,
		"m.content ILIKE '%'||$2||'%' ESCAPE E'\\\\'")
	assert.Equal(t, []any{"quick", "fox"}, pb.args)
}

func TestPGSubstringSnippetFTSModeCentersOnFirstTerm(t *testing.T) {
	body := strings.Repeat("prefix ", 30) + "the quick brown fox jumps"

	got := pgSubstringSnippet(db.ContentSearchFilter{
		Pattern: "quick fox",
		Mode:    "fts",
	}, body)

	assert.Contains(t, got, "quick")
	assert.Contains(t, got, "fox")
}

func TestMapPGWriteErrorNormalizesReadOnlyPgErrors(t *testing.T) {
	for _, code := range []string{"25006", "42501"} {
		t.Run(code, func(t *testing.T) {
			err := mapPGWriteError("writing test row", &pgconn.PgError{
				Code:    code,
				Message: "permission denied",
			})

			require.ErrorIs(t, err, db.ErrReadOnly)
			assert.Contains(t, err.Error(), "writing test row")
		})
	}
}

func TestMapPGWriteErrorKeepsNonReadOnlyCause(t *testing.T) {
	cause := errors.New("network unavailable")

	err := mapPGWriteError("writing test row", cause)

	require.ErrorIs(t, err, cause)
	assert.False(t, errors.Is(err, db.ErrReadOnly))
	assert.Contains(t, err.Error(), "writing test row")
}

func TestEmptyTrashExcludesSameRowsItDeletes(t *testing.T) {
	state := &emptyTrashProbeState{
		sessions: map[string]bool{
			"already-trashed":      true,
			"concurrently-trashed": false,
		},
		excluded: map[string]bool{},
		deleted:  map[string]bool{},
	}
	store := &Store{pg: newEmptyTrashProbeDB(t, state)}

	count, err := store.EmptyTrash()

	require.NoError(t, err, "EmptyTrash")
	assert.Equal(t, 1, count)
	assert.True(t, state.deleted["already-trashed"])
	assert.True(t, state.excluded["already-trashed"])
	assert.False(t, state.deleted["concurrently-trashed"])
	assert.False(t, state.excluded["concurrently-trashed"])
}

type emptyTrashProbeDriver struct{}

type emptyTrashProbeConn struct {
	state *emptyTrashProbeState
}

type emptyTrashProbeTx struct {
	state *emptyTrashProbeState
}

type emptyTrashProbeRows struct {
	count int64
	read  bool
}

type emptyTrashProbeState struct {
	mu       sync.Mutex
	sessions map[string]bool
	excluded map[string]bool
	deleted  map[string]bool
}

var (
	emptyTrashProbeRegisterOnce sync.Once
	emptyTrashProbeStatesMu     sync.Mutex
	emptyTrashProbeStates       = map[string]*emptyTrashProbeState{}
)

func newEmptyTrashProbeDB(
	t *testing.T, state *emptyTrashProbeState,
) *sql.DB {
	t.Helper()
	emptyTrashProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_empty_trash_probe", emptyTrashProbeDriver{})
	})
	name := t.Name()
	emptyTrashProbeStatesMu.Lock()
	emptyTrashProbeStates[name] = state
	emptyTrashProbeStatesMu.Unlock()
	t.Cleanup(func() {
		emptyTrashProbeStatesMu.Lock()
		delete(emptyTrashProbeStates, name)
		emptyTrashProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_empty_trash_probe", name)
	require.NoError(t, err, "open empty-trash probe db")
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

func (emptyTrashProbeDriver) Open(name string) (driver.Conn, error) {
	emptyTrashProbeStatesMu.Lock()
	state := emptyTrashProbeStates[name]
	emptyTrashProbeStatesMu.Unlock()
	return &emptyTrashProbeConn{state: state}, nil
}

func (c *emptyTrashProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *emptyTrashProbeConn) Close() error { return nil }

func (c *emptyTrashProbeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *emptyTrashProbeConn) BeginTx(
	context.Context, driver.TxOptions,
) (driver.Tx, error) {
	return &emptyTrashProbeTx{state: c.state}, nil
}

func (c *emptyTrashProbeConn) ExecContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Result, error) {
	normalized := strings.ToLower(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	switch {
	case strings.Contains(normalized, "set deleted_at = deleted_at"):
		return driver.RowsAffected(c.state.trashedCountLocked()), nil
	case strings.Contains(normalized, "insert into excluded_sessions"):
		c.state.excludeTrashedLocked()
		c.state.sessions["concurrently-trashed"] = true
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "delete from sessions") &&
		strings.Contains(normalized, "deleted_at is not null"):
		return driver.RowsAffected(c.state.deleteTrashedLocked()), nil
	default:
		return nil, errors.New("unexpected empty-trash exec")
	}
}

func (c *emptyTrashProbeConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	if !strings.Contains(normalized, "with deleted as") ||
		!strings.Contains(normalized, "delete from sessions") ||
		!strings.Contains(normalized, "returning id") ||
		!strings.Contains(normalized, "select id from deleted") {
		return nil, errors.New("unexpected empty-trash query")
	}

	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	deleted := c.state.deleteTrashedLocked()
	for id := range c.state.deleted {
		c.state.excluded[id] = true
	}
	c.state.sessions["concurrently-trashed"] = true
	return &emptyTrashProbeRows{count: deleted}, nil
}

func (t *emptyTrashProbeTx) Commit() error { return nil }

func (t *emptyTrashProbeTx) Rollback() error { return nil }

func (s *emptyTrashProbeState) trashedCountLocked() int64 {
	var count int64
	for _, trashed := range s.sessions {
		if trashed {
			count++
		}
	}
	return count
}

func (s *emptyTrashProbeState) excludeTrashedLocked() {
	for id, trashed := range s.sessions {
		if trashed {
			s.excluded[id] = true
		}
	}
}

func (s *emptyTrashProbeState) deleteTrashedLocked() int64 {
	var count int64
	for id, trashed := range s.sessions {
		if !trashed {
			continue
		}
		s.deleted[id] = true
		delete(s.sessions, id)
		count++
	}
	return count
}

func (r *emptyTrashProbeRows) Columns() []string { return []string{"count"} }

func (r *emptyTrashProbeRows) Close() error { return nil }

func (r *emptyTrashProbeRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	dest[0] = r.count
	r.read = true
	return nil
}
