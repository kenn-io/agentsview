package db

import (
	"encoding/hex"
	"slices"
	"strings"

	"github.com/uptrace/bun/dialect/sqlitedialect"
)

// sqliteArchiveDialect preserves raw text identity, including invalid UTF-8
// and embedded NUL. SQLite accepts NUL text through a blob cast. Provider
// content sanitization remains the ingestion layer's responsibility.
type sqliteArchiveDialect struct{ *sqlitedialect.Dialect }

// NewSQLiteArchiveDialect preserves raw bytes in SQLite text keys.
// Archive and disposable staging stores share the same literal formatter.
func NewSQLiteArchiveDialect() *sqliteArchiveDialect {
	return &sqliteArchiveDialect{Dialect: sqlitedialect.New()}
}

func (d *sqliteArchiveDialect) AppendString(dst []byte, value string) []byte {
	if !strings.ContainsRune(value, 0) {
		dst = slices.Grow(dst, len(value)+strings.Count(value, "'")+2)
		dst = append(dst, '\'')
		for {
			quote := strings.IndexByte(value, '\'')
			if quote < 0 {
				break
			}
			dst = append(dst, value[:quote+1]...)
			dst = append(dst, '\'')
			value = value[quote+1:]
		}
		dst = append(dst, value...)
		return append(dst, '\'')
	}
	dst = append(dst, "CAST(X'"...)
	dst = hex.AppendEncode(dst, []byte(value))
	return append(dst, "' AS TEXT)"...)
}
