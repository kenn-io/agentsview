package db

import (
	"encoding/hex"
	"strings"

	"github.com/uptrace/bun/dialect/sqlitedialect"
)

// sqliteArchiveDialect preserves internal text keys containing NUL. Bun's
// default string formatter drops NUL bytes; SQLite can store them as TEXT
// when the literal is expressed as a blob cast. Provider content sanitization
// remains the ingestion layer's responsibility.
type sqliteArchiveDialect struct{ *sqlitedialect.Dialect }

func newSQLiteArchiveDialect() *sqliteArchiveDialect {
	return &sqliteArchiveDialect{Dialect: sqlitedialect.New()}
}

func (d *sqliteArchiveDialect) AppendString(dst []byte, value string) []byte {
	if !strings.ContainsRune(value, 0) {
		return d.Dialect.AppendString(dst, value)
	}
	dst = append(dst, "CAST(X'"...)
	dst = hex.AppendEncode(dst, []byte(value))
	return append(dst, "' AS TEXT)"...)
}
