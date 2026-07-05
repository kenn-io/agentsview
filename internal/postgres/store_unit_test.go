package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestStoreHasSemanticFalse pins that the PostgreSQL store reports no
// semantic search capability until it gets its own VectorSearcher seam.
func TestStoreHasSemanticFalse(t *testing.T) {
	s := &Store{}
	assert.False(t, s.HasSemantic(), "PostgreSQL HasSemantic")
}

// TestStoreSearchContentSemanticModesUnavailable pins that "semantic" and
// "hybrid" are rejected with db.ErrSemanticUnavailable before any query runs
// -- a zero-value Store (no live *sql.DB) is enough to prove that.
func TestStoreSearchContentSemanticModesUnavailable(t *testing.T) {
	s := &Store{}
	for _, mode := range []string{"semantic", "hybrid"} {
		_, err := s.SearchContent(context.Background(),
			db.ContentSearchFilter{Pattern: "x", Mode: mode})
		require.Error(t, err, "mode %q", mode)
		assert.True(t, errors.Is(err, db.ErrSemanticUnavailable),
			"mode %q: want ErrSemanticUnavailable, got %v", mode, err)
	}
}

// TestStoreSearchContentSemanticInvalidInputReturns400Before501 pins backend
// parity (AGENTS.md): an invalid semantic/hybrid request -- cursor pagination
// or a non-messages source -- must return the same *db.SearchInputError
// SQLite's ValidateSemanticFilter returns, not db.ErrSemanticUnavailable, even
// though PostgreSQL has no VectorSearcher seam and would otherwise report the
// capability gate for any request in these modes.
func TestStoreSearchContentSemanticInvalidInputReturns400Before501(t *testing.T) {
	s := &Store{}
	cases := []struct {
		name string
		f    db.ContentSearchFilter
	}{
		{"cursor rejected", db.ContentSearchFilter{Pattern: "x", Cursor: 1}},
		{"non-messages source rejected", db.ContentSearchFilter{
			Pattern: "x", Sources: []string{"tool_input"},
		}},
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				f := tc.f
				f.Mode = mode
				_, err := s.SearchContent(context.Background(), f)
				require.Error(t, err)
				var inputErr *db.SearchInputError
				assert.True(t, errors.As(err, &inputErr),
					"expected *db.SearchInputError, got %T: %v", err, err)
				assert.False(t, errors.Is(err, db.ErrSemanticUnavailable),
					"invalid input must not be masked as ErrSemanticUnavailable")
			})
		}
	}
}

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
			"already-trashed":                   true,
			"vibe:session_already_trashed":      false,
			"concurrently-trashed":              false,
			"vibe:session_concurrently_trashed": false,
		},
		aliases: map[string][]string{
			"already-trashed":      {"vibe:session_already_trashed"},
			"concurrently-trashed": {"vibe:session_concurrently_trashed"},
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
	assert.True(t, state.excluded["vibe:session_already_trashed"])
	assert.True(t, state.deleted["vibe:session_already_trashed"])
	assert.False(t, state.deleted["concurrently-trashed"])
	assert.False(t, state.excluded["concurrently-trashed"])
	assert.False(t, state.excluded["vibe:session_concurrently_trashed"])
	assert.False(t, state.deleted["vibe:session_concurrently_trashed"])
}

func TestDeleteSessionIfTrashedExcludesRecordedAliases(t *testing.T) {
	state := &emptyTrashProbeState{
		sessions: map[string]bool{
			"trashed":              true,
			"vibe:session_trashed": false,
		},
		aliases: map[string][]string{
			"trashed": {"vibe:session_trashed"},
		},
		excluded: map[string]bool{},
		deleted:  map[string]bool{},
	}
	store := &Store{pg: newEmptyTrashProbeDB(t, state)}

	count, err := store.DeleteSessionIfTrashed("trashed")

	require.NoError(t, err, "DeleteSessionIfTrashed")
	assert.EqualValues(t, 1, count)
	assert.True(t, state.deleted["trashed"])
	assert.True(t, state.excluded["trashed"])
	assert.True(t, state.excluded["vibe:session_trashed"])
	assert.True(t, state.deleted["vibe:session_trashed"])
}

func TestDeleteSessionIfTrashedExcludesReverseAliasCanonical(t *testing.T) {
	state := &emptyTrashProbeState{
		sessions: map[string]bool{
			"vibe:canonical":       false,
			"vibe:session_trashed": true,
		},
		aliases: map[string][]string{
			"vibe:canonical": {"vibe:session_trashed"},
		},
		excluded: map[string]bool{},
		deleted:  map[string]bool{},
	}
	store := &Store{pg: newEmptyTrashProbeDB(t, state)}

	count, err := store.DeleteSessionIfTrashed("vibe:session_trashed")

	require.NoError(t, err, "DeleteSessionIfTrashed")
	assert.EqualValues(t, 1, count)
	assert.True(t, state.deleted["vibe:session_trashed"])
	assert.True(t, state.deleted["vibe:canonical"])
	assert.True(t, state.excluded["vibe:session_trashed"])
	assert.True(t, state.excluded["vibe:canonical"])
}

type emptyTrashProbeDriver struct{}

type emptyTrashProbeConn struct {
	state *emptyTrashProbeState
}

type emptyTrashProbeTx struct {
	state *emptyTrashProbeState
}

type emptyTrashProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type emptyTrashProbeState struct {
	mu       sync.Mutex
	sessions map[string]bool
	aliases  map[string][]string
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

func (c *emptyTrashProbeConn) CheckNamedValue(
	*driver.NamedValue,
) error {
	return nil
}

func (c *emptyTrashProbeConn) ExecContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	normalized := strings.ToLower(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	switch {
	case strings.Contains(normalized, "set deleted_at = deleted_at"):
		return driver.RowsAffected(c.state.trashedCountLocked()), nil
	case strings.Contains(normalized, "insert into excluded_sessions"):
		c.state.excludeNamedValuesLocked(args)
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "delete from sessions") &&
		strings.Contains(normalized, "id = any($1)") &&
		strings.Contains(normalized, "deleted_at is not null"):
		return driver.RowsAffected(c.state.deleteNamedTrashedLocked(args)), nil
	case strings.Contains(normalized, "delete from sessions") &&
		strings.Contains(normalized, "id = any($1)"):
		return driver.RowsAffected(c.state.deleteNamedExistingLocked(args)), nil
	default:
		return nil, errors.New("unexpected empty-trash exec")
	}
}

func (c *emptyTrashProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	if !strings.Contains(normalized, "left join session_aliases") ||
		!strings.Contains(normalized, "for update of s") {
		return nil, errors.New("unexpected empty-trash query")
	}

	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	filterID := ""
	if strings.Contains(normalized, "s.id = $1") && len(args) > 0 {
		filterID, _ = args[0].Value.(string)
	}
	values := c.state.trashedSessionAliasRowsLocked(filterID)
	if _, ok := c.state.sessions["concurrently-trashed"]; ok {
		c.state.sessions["concurrently-trashed"] = true
	}
	return &emptyTrashProbeRows{
		columns: []string{
			"id", "alias_id", "session_id", "alias_id",
		},
		values: values,
	}, nil
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

func (s *emptyTrashProbeState) trashedSessionAliasRowsLocked(
	filterID string,
) [][]driver.Value {
	values := [][]driver.Value{}
	for id, trashed := range s.sessions {
		if filterID != "" && id != filterID {
			continue
		}
		if !trashed {
			continue
		}
		directAliases := s.aliases[id]
		if len(directAliases) == 0 {
			directAliases = []string{""}
		}
		reverseAliases := s.reverseAliasRowsLocked(id)
		if len(reverseAliases) == 0 {
			for _, aliasID := range directAliases {
				values = append(values, []driver.Value{
					id, nullableDriverString(aliasID), nil, nil,
				})
			}
			continue
		}
		for _, aliasID := range directAliases {
			for _, reverseAlias := range reverseAliases {
				values = append(values, []driver.Value{
					id,
					nullableDriverString(aliasID),
					reverseAlias[0],
					nullableDriverString(reverseAlias[1]),
				})
			}
		}
	}
	return values
}

func (s *emptyTrashProbeState) reverseAliasRowsLocked(
	aliasID string,
) [][2]string {
	rows := [][2]string{}
	for sessionID, aliases := range s.aliases {
		hasAlias := slices.Contains(aliases, aliasID)
		if !hasAlias {
			continue
		}
		for _, alias := range aliases {
			rows = append(rows, [2]string{sessionID, alias})
		}
	}
	return rows
}

func nullableDriverString(value string) driver.Value {
	if value == "" {
		return nil
	}
	return value
}

func (s *emptyTrashProbeState) excludeNamedValuesLocked(
	args []driver.NamedValue,
) {
	for _, id := range namedValueStrings(args) {
		s.excluded[id] = true
	}
}

func (s *emptyTrashProbeState) deleteNamedTrashedLocked(
	args []driver.NamedValue,
) int64 {
	var count int64
	for _, id := range namedValueStrings(args) {
		if !s.sessions[id] {
			continue
		}
		s.deleted[id] = true
		delete(s.sessions, id)
		count++
	}
	return count
}

func (s *emptyTrashProbeState) deleteNamedExistingLocked(
	args []driver.NamedValue,
) int64 {
	var count int64
	for _, id := range namedValueStrings(args) {
		if _, ok := s.sessions[id]; !ok {
			continue
		}
		s.deleted[id] = true
		delete(s.sessions, id)
		count++
	}
	return count
}

func namedValueStrings(args []driver.NamedValue) []string {
	if len(args) == 0 {
		return nil
	}
	switch value := args[0].Value.(type) {
	case []string:
		return value
	case string:
		return []string{value}
	default:
		return nil
	}
}

func (r *emptyTrashProbeRows) Columns() []string { return r.columns }

func (r *emptyTrashProbeRows) Close() error { return nil }

func (r *emptyTrashProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}
