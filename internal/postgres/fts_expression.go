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
	return db.RenderFTSExpression(expr, func(term string) string {
		return column + " ILIKE '%'||" + pb.add(escapeLike(term)) + "||'%' ESCAPE E'\\\\'"
	}), nil
}
