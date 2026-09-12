package db

import "context"

// BunSearchSource describes a dialect-owned relational source without
// executing it. StableRow is an expression that uniquely and deterministically
// identifies one source row for ranking tie-breaks and result hydration.
type BunSearchSource struct {
	CTEs      []BunCTEFragment
	From      BunSQLFragment
	Joins     []BunSQLFragment
	StableRow BunSQLFragment
}

// BunLexicalBindings contains prepared forms of one user query. Each value is
// represented as a Bun fragment so user input remains in Args when a dialect
// reuses it in match, rank, position, or snippet expressions.
type BunLexicalBindings struct {
	Query BunSQLFragment
	Plain BunSQLFragment
	Terms []BunSQLFragment
}

// BunRankDirection describes which numeric direction represents a better
// lexical match for a dialect's rank expression.
type BunRankDirection uint8

const (
	BunRankAscending BunRankDirection = iota
	BunRankDescending
)

// BunRankExpression couples a dialect-owned rank expression with its ordering
// direction. Callers add their own stable tie-breakers after this expression.
type BunRankExpression struct {
	Expression BunSQLFragment
	Direction  BunRankDirection
}

// BunLexicalDialect builds the engine-specific pieces of a Bun-owned lexical
// query. Its methods are compositional only: none may execute SQL. SQL operands
// supplied to methods must be application-owned expressions; user values enter
// through Bind and remain in BunSQLFragment.Args.
type BunLexicalDialect interface {
	Available() bool
	Bind(string) (BunLexicalBindings, error)
	MessageSource(string, BunLexicalBindings) BunSearchSource
	Match(BunSQLFragment, BunLexicalBindings) BunSQLFragment
	Rank(BunSQLFragment, BunLexicalBindings) BunRankExpression
	MatchPosition(BunSQLFragment, BunLexicalBindings) BunSQLFragment
	Snippet(BunSQLFragment, BunLexicalBindings, int) BunSQLFragment
	Classify(error) error
}

// BunQueryEncoder performs the non-relational query-embedding step. It is
// intentionally separate from BunVectorDialect: encoding may call a model,
// while every vector metadata/candidate operation remains a Bun query.
type BunQueryEncoder interface {
	Encode(context.Context, string) ([]float32, error)
}

// BunVectorGeneration describes the generation used by a vector dialect.
// Metadata is an optional Bun query fragment for adapters that resolve the
// active generation in SQL; ID and Dimension hold already-resolved metadata.
// No field causes that query to execute.
type BunVectorGeneration struct {
	ID        int64
	Dimension int
	Metadata  BunSQLFragment
}

// BunVectorDialect builds vector candidate-query fragments. Methods never
// execute SQL. CandidateSource owns validated generation identifiers;
// EncodeParam keeps an embedding in Bun arguments; and BeforeCandidates
// returns any transaction-scoped setup statements for the Bun executor to run.
type BunVectorDialect interface {
	Available() bool
	UnavailableError() error
	Generation() BunVectorGeneration
	EncodeParam(BunVectorGeneration, []float32) (BunSQLFragment, error)
	CandidateSource(BunVectorGeneration) BunSearchSource
	Distance(BunVectorGeneration, BunSQLFragment, BunSQLFragment) BunSQLFragment
	Score(BunSQLFragment) BunSQLFragment
	BeforeCandidates(BunVectorGeneration, int) []BunSQLFragment
}
