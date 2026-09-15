// Package bundialect implements Bun's SQL dialect contract for DuckDB.
package bundialect

import (
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/feature"
	"github.com/uptrace/bun/schema"
)

const duckDBName dialect.Name = -1

const features = feature.CTE |
	feature.WithValues |
	feature.Returning |
	feature.InsertReturning |
	feature.DeleteReturning |
	feature.InsertOnConflict |
	feature.UpdateFromTable |
	feature.TableNotExists |
	feature.CreateIndexIfNotExists |
	feature.SelectExists |
	feature.CompositeIn |
	feature.DefaultPlaceholder

// Dialect describes the DuckDB SQL understood by the pinned database driver.
type Dialect struct {
	schema.BaseDialect

	tables *schema.Tables
}

var _ schema.Dialect = (*Dialect)(nil)

// New returns an independent DuckDB dialect and model registry.
func New() *Dialect {
	d := new(Dialect)
	d.tables = schema.NewTables(d)
	return d
}

// Init implements schema.Dialect. DuckDB requires no connection initialization.
func (*Dialect) Init(*sql.DB) {}

// Name returns a private non-built-in dialect value so Bun never applies
// SQLite- or PostgreSQL-specific behavior based on dialect identity.
func (*Dialect) Name() dialect.Name {
	return duckDBName
}

// Features returns only the Bun query forms covered by DuckDB execution tests.
func (*Dialect) Features() feature.Feature {
	return features
}

// Tables returns this dialect's model metadata registry.
func (d *Dialect) Tables() *schema.Tables {
	return d.tables
}

// OnTable normalizes Bun's discovered Go types to DuckDB DDL types.
func (*Dialect) OnTable(table *schema.Table) {
	for _, field := range table.FieldMap {
		onField(field)
	}
}

// IdentQuote returns DuckDB's ANSI identifier quote.
func (*Dialect) IdentQuote() byte {
	return '"'
}

// AppendString appends a DuckDB string literal.
func (d *Dialect) AppendString(b []byte, value string) []byte {
	return d.BaseDialect.AppendString(b, value)
}

// AppendBytes appends a DuckDB BLOB expression without relying on string
// encoding or invalid UTF-8 handling in the driver.
func (*Dialect) AppendBytes(b, value []byte) []byte {
	if value == nil {
		return dialect.AppendNull(b)
	}

	b = append(b, "from_hex('"...)
	start := len(b)
	b = append(b, make([]byte, hex.EncodedLen(len(value)))...)
	hex.Encode(b[start:], value)
	b = append(b, "')"...)
	return b
}

// AppendJSON appends JSON as a safely quoted DuckDB literal.
func (d *Dialect) AppendJSON(b, value []byte) []byte {
	return d.BaseDialect.AppendJSON(b, value)
}

// AppendBool appends DuckDB's native boolean literals.
func (d *Dialect) AppendBool(b []byte, value bool) []byte {
	return d.BaseDialect.AppendBool(b, value)
}

// AppendTime appends a UTC DuckDB TIMESTAMP literal. DuckDB TIMESTAMP has
// microsecond precision, matching the driver's time.Time round trip.
func (*Dialect) AppendTime(b []byte, value time.Time) []byte {
	b = append(b, "TIMESTAMP '"...)
	b = value.UTC().AppendFormat(b, "2006-01-02 15:04:05.999999")
	b = append(b, '\'')
	return b
}

// AppendCanonicalTimestamp keeps canonical Bun values on DuckDB's typed
// microsecond literal path. Quoted RFC3339 strings can round one microsecond
// low when DuckDB casts them on Windows.
func (d *Dialect) AppendCanonicalTimestamp(b []byte, value time.Time) []byte {
	return d.AppendTime(b, value)
}

// AppendSequence deliberately leaves the column definition unchanged.
// Mirror primary keys are assigned by the source archive.
func (*Dialect) AppendSequence(b []byte, _ *schema.Table, _ *schema.Field) []byte {
	return b
}

// DefaultVarcharLen reports that DuckDB does not require a VARCHAR length.
func (*Dialect) DefaultVarcharLen() int {
	return 0
}

// DefaultSchema returns DuckDB's default schema.
func (*Dialect) DefaultSchema() string {
	return "main"
}
