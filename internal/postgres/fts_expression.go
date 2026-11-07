package postgres

import (
	"go.kenn.io/agentsview/internal/db"
)

// pgFTSExpression preserves the Boolean structure of prepared FTS queries.
// PostgreSQL uses literal ILIKE terms instead of SQLite's token index, so short
// CJK terms remain searchable without changing the archive or its indexes.
func pgFTSExpression(pattern, column string, pb *paramBuilder) (string, error) {
	expr, err := db.ParseFTSExpression(pattern)
	if err != nil {
		return "", err
	}
	if expr == nil {
		return "FALSE", nil
	}
	return pgFTSSQL(expr, column, pb), nil
}

func pgFTSSQL(expr *db.FTSExpr, column string, pb *paramBuilder) string {
	if expr.Op == "term" {
		return column + " ILIKE '%'||" + pb.add(escapeLike(expr.Term)) + "||'%' ESCAPE E'\\\\'"
	}
	left := pgFTSSQL(expr.Left, column, pb)
	right := pgFTSSQL(expr.Right, column, pb)
	if expr.Op == "NOT" {
		return "(" + left + " AND NOT (" + right + "))"
	}
	return "(" + left + " " + expr.Op + " " + right + ")"
}
