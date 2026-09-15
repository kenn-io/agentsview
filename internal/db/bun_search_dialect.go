package db

import "strings"

// BunSearchDialect owns the small SQL-policy differences used by common Bun
// search queries. Ranked FTS and vector candidate generation remain separate
// backend capabilities.
type BunSearchDialect struct {
	caseFoldContentSearch bool
	contentTimestampNull  string
	portableContentFTS    bool
	unicodeRecentEdits    bool
	systemPrefixSQL       func(string, string) string
}

// SQLiteBunSearchDialect preserves SQLite's exact non-ASCII substring
// behavior and delegates Unicode recent-edit matching to Go.
func SQLiteBunSearchDialect() BunSearchDialect {
	return BunSearchDialect{
		unicodeRecentEdits: true,
		systemPrefixSQL:    SystemPrefixSQL,
	}
}

// PostgresBunSearchDialect configures common search SQL for PostgreSQL.
func PostgresBunSearchDialect() BunSearchDialect {
	return BunSearchDialect{
		caseFoldContentSearch: true,
		contentTimestampNull:  "CAST(NULL AS TIMESTAMPTZ)",
		portableContentFTS:    true,
		systemPrefixSQL:       PostgresSystemPrefixSQL,
	}
}

func (d BunSearchDialect) contentTimestampNullExpr() string {
	if d.contentTimestampNull != "" {
		return d.contentTimestampNull
	}
	return "NULL"
}

// DuckDBBunSearchDialect configures common search SQL for DuckDB.
func DuckDBBunSearchDialect() BunSearchDialect {
	return BunSearchDialect{
		caseFoldContentSearch: true,
		portableContentFTS:    true,
		systemPrefixSQL:       DuckDBSystemPrefixSQL,
	}
}

func (d BunSearchDialect) contentSearchPattern(literal string) string {
	if d.caseFoldContentSearch {
		literal = strings.ToLower(literal)
	}
	return "%" + EscapeLikePattern(literal) + "%"
}

func (d BunSearchDialect) contentSearchPredicate(column string) string {
	expression := "COALESCE(" + column + ", '')"
	if d.caseFoldContentSearch {
		expression = "LOWER(" + expression + ")"
	}
	return expression + " LIKE ? ESCAPE '\\'"
}

func (d BunSearchDialect) systemPrefixPredicate(content, role string) string {
	if d.systemPrefixSQL != nil {
		return d.systemPrefixSQL(content, role)
	}
	return SystemPrefixSQL(content, role)
}
