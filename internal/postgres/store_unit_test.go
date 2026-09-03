package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"go.kenn.io/agentsview/internal/db"
)

// TestStoreHasSemanticFalse pins that the PostgreSQL store reports no
// semantic search capability until it gets its own VectorSearcher seam.
func TestStoreHasSemanticFalse(t *testing.T) {
	s := &Store{}
	assert.False(t, s.HasSemantic(), "PostgreSQL HasSemantic")
}

func TestStoreBunBackendCapabilitiesFollowInsightProbe(t *testing.T) {
	store := &Store{}
	backend := &postgresBunBackend{store: store}

	capabilities := backend.Capabilities()
	assert.True(t, capabilities.AllowsWrite(db.WriteCuration))
	assert.True(t, capabilities.AllowsWrite(db.WriteSessionManagement))
	assert.False(t, capabilities.AllowsWrite(db.WriteArchive))
	assert.False(t, capabilities.AllowsWrite(db.WriteRecall))
	assert.False(t, capabilities.AllowsWrite(db.WriteInsight))
	assert.False(t, capabilities.AllowsWrite(db.WriteInsightDelete))
	assert.False(t, capabilities.Recall)

	store.setInsightCapabilities(true, false)
	assert.True(t, backend.Capabilities().AllowsWrite(db.WriteInsight))
	assert.False(t, backend.Capabilities().AllowsWrite(db.WriteInsightDelete))
	store.setInsightCapabilities(false, true)
	assert.False(t, backend.Capabilities().AllowsWrite(db.WriteInsight))
	assert.True(t, backend.Capabilities().AllowsWrite(db.WriteInsightDelete))
}

func TestPostgresBunBackendUpdatePreservesReadOnlySentinel(t *testing.T) {
	backend := &postgresBunBackend{store: &Store{}}
	cause := &pgconn.PgError{Code: "42501", Message: "permission denied"}

	err := backend.Update(t.Context(), func(bun.IDB) error {
		return fmt.Errorf("deleting insight: %w", cause)
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrReadOnly)
	assert.ErrorIs(t, err, cause)
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
