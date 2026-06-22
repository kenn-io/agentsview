package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
