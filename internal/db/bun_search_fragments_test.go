package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type literalBunLexicalDialect struct{}

func (literalBunLexicalDialect) Available() bool { return true }

func (literalBunLexicalDialect) Bind(query string) (BunLexicalBindings, error) {
	return BunLexicalBindings{
		Query: BunValue(query),
		Plain: BunValue(query),
		Terms: []BunSQLFragment{BunValue(query)},
	}, nil
}

func (literalBunLexicalDialect) MessageSource(
	_ string, _ BunLexicalBindings,
) BunSearchSource {
	return BunSearchSource{
		From:      BunSQL("messages_fts"),
		StableRow: BunSQL("messages_fts.rowid"),
	}
}

func (literalBunLexicalDialect) Match(
	_ BunSQLFragment, bindings BunLexicalBindings,
) BunSQLFragment {
	return BunSQL("messages_fts MATCH ?", bindings.Query.Args[0])
}

func (literalBunLexicalDialect) Rank(
	_ BunSQLFragment, _ BunLexicalBindings,
) BunRankExpression {
	return BunRankExpression{
		Expression: BunSQL("messages_fts.rank"),
		Direction:  BunRankAscending,
	}
}

func (literalBunLexicalDialect) MatchPosition(
	content BunSQLFragment, bindings BunLexicalBindings,
) BunSQLFragment {
	return BunSQL("instr("+content.SQL+", ?)", bindings.Plain.Args[0])
}

func (literalBunLexicalDialect) Snippet(
	_ BunSQLFragment, _ BunLexicalBindings, _ int,
) BunSQLFragment {
	return BunSQL("snippet(messages_fts, 0, '', '', '...', 32)")
}

func (literalBunLexicalDialect) Classify(err error) error { return err }

func TestBunLexicalBindingsKeepQueryTextInArgs(t *testing.T) {
	dialect := literalBunLexicalDialect{}
	query := `quote' ? $1 --`

	bindings, err := dialect.Bind(query)
	require.NoError(t, err)
	match := dialect.Match(BunSQL("message.content"), bindings)
	position := dialect.MatchPosition(BunSQL("message.content"), bindings)

	assert.NotContains(t, match.SQL, query)
	assert.Equal(t, []any{query}, match.Args)
	assert.NotContains(t, position.SQL, query)
	assert.Equal(t, []any{query}, position.Args)
}

type literalBunVectorDialect struct{}

func (literalBunVectorDialect) Available() bool { return true }
func (literalBunVectorDialect) UnavailableError() error {
	return errors.New("unavailable")
}
func (literalBunVectorDialect) Generation() BunVectorGeneration {
	return BunVectorGeneration{ID: 7, Dimension: 2}
}
func (literalBunVectorDialect) EncodeParam(
	_ BunVectorGeneration, vector []float32,
) (BunSQLFragment, error) {
	return BunValue(vector), nil
}
func (literalBunVectorDialect) CandidateSource(BunVectorGeneration) BunSearchSource {
	return BunSearchSource{From: BunSQL("vector_chunks_g7")}
}
func (literalBunVectorDialect) Distance(
	_ BunVectorGeneration, column, parameter BunSQLFragment,
) BunSQLFragment {
	return BunSQL(column.SQL+" <=> "+parameter.SQL, parameter.Args...)
}
func (literalBunVectorDialect) Score(distance BunSQLFragment) BunSQLFragment {
	return BunSQL("1 - ("+distance.SQL+")", distance.Args...)
}
func (literalBunVectorDialect) BeforeCandidates(
	_ BunVectorGeneration, limit int,
) []BunSQLFragment {
	return []BunSQLFragment{BunSQL("SELECT set_config('search.limit', ?, true)", limit)}
}

func TestBunVectorFragmentsKeepEmbeddingAndSetupValuesInArgs(t *testing.T) {
	dialect := literalBunVectorDialect{}
	generation := dialect.Generation()
	embedding := []float32{0.25, -0.75}

	parameter, err := dialect.EncodeParam(generation, embedding)
	require.NoError(t, err)
	distance := dialect.Distance(generation, BunSQL("embedding"), parameter)
	setup := dialect.BeforeCandidates(generation, 80)

	assert.Equal(t, "embedding <=> ?", distance.SQL)
	assert.Equal(t, []any{embedding}, distance.Args)
	require.Len(t, setup, 1)
	assert.NotContains(t, setup[0].SQL, "80")
	assert.Equal(t, []any{80}, setup[0].Args)
}

var _ BunLexicalDialect = literalBunLexicalDialect{}
var _ BunVectorDialect = literalBunVectorDialect{}
