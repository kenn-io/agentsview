package db

import (
	"strings"

	"github.com/uptrace/bun"
)

// BunSQLFragment is composable SQL that is executed only after it is handed to
// a bun.IDB query. SQL contains Bun placeholders; Args contains their values.
// Callers must keep user-controlled values in Args rather than interpolating
// them into SQL.
type BunSQLFragment struct {
	SQL  string
	Args []any
}

// BunSQL constructs a fragment from SQL owned by the application and the
// values for its Bun placeholders.
func BunSQL(sql string, args ...any) BunSQLFragment {
	return BunSQLFragment{SQL: sql, Args: append([]any(nil), args...)}
}

// BunValue constructs one scalar Bun placeholder. It is the preferred way to
// introduce a user-controlled value while composing a larger fragment.
func BunValue(value any) BunSQLFragment {
	return BunSQLFragment{SQL: "?", Args: []any{value}}
}

// BunIdentifier constructs one dialect-quoted identifier placeholder. This is
// the safe representation for dialect-owned dynamic table, schema, column, or
// index names; callers must not interpolate those names into fragment SQL.
func BunIdentifier(value string) BunSQLFragment {
	return BunSQLFragment{SQL: "?", Args: []any{bun.Ident(value)}}
}

// JoinBunSQLFragments joins non-empty fragments without changing placeholder
// order. The separator must be application-owned SQL.
func JoinBunSQLFragments(separator string, fragments ...BunSQLFragment) BunSQLFragment {
	parts := make([]string, 0, len(fragments))
	var args []any
	for _, fragment := range fragments {
		if fragment.SQL == "" {
			continue
		}
		parts = append(parts, fragment.SQL)
		args = append(args, fragment.Args...)
	}
	return BunSQLFragment{SQL: strings.Join(parts, separator), Args: args}
}

// BunCTEFragment names one application-owned common-table expression. Query
// remains a Bun fragment so its bound values compose with the enclosing query.
type BunCTEFragment struct {
	Name  string
	Query BunSQLFragment
}
