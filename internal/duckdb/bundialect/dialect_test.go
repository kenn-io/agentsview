package bundialect

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/feature"
	"github.com/uptrace/bun/schema"
)

var _ schema.Dialect = (*Dialect)(nil)

type dialectTypeFixture struct {
	bun.BaseModel `bun:"table:dialect_type_fixtures"`
	Small         int16
	Count         int32
	ID            int64 `bun:",pk,autoincrement"`
	Name          string
	Enabled       bool
	Payload       []byte
	Metadata      json.RawMessage `bun:",type:JSON"`
	OccurredAt    time.Time
}

func TestDialectIdentityIsDuckDBSpecific(t *testing.T) {
	d := New()

	assert.Equal(t, "custom", d.Name().String())
	assert.NotEqual(t, dialect.SQLite, d.Name())
	assert.NotEqual(t, dialect.PG, d.Name())
	assert.Equal(t, "main", d.DefaultSchema())
	assert.Equal(t, byte('"'), d.IdentQuote())
	assert.False(t, d.Features().Has(feature.AutoIncrement))
	assert.False(t, d.Features().Has(feature.Identity))
	assert.False(t, d.Features().Has(feature.GeneratedIdentity))
}

func TestDialectMapsCanonicalGoTypes(t *testing.T) {
	d := New()
	table := d.Tables().Get(reflect.TypeFor[dialectTypeFixture]())

	want := map[string]string{
		"small":       "SMALLINT",
		"count":       "INTEGER",
		"id":          "BIGINT",
		"name":        "TEXT",
		"enabled":     "BOOLEAN",
		"payload":     "BLOB",
		"metadata":    "JSON",
		"occurred_at": "TIMESTAMP",
	}
	for column, wantType := range want {
		field := table.FieldMap[column]
		require.NotNil(t, field, "field for column %s", column)
		assert.Equal(t, wantType, field.CreateTableSQLType, column)
	}
}

func TestDialectAppendsDuckDBLiterals(t *testing.T) {
	d := New()
	timestamp := time.Date(2026, 8, 2, 12, 30, 0, 123_000_000,
		time.FixedZone("west", -4*60*60))

	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{name: "string", got: d.AppendString(nil, "O'Reilly"), want: "'O''Reilly'"},
		{name: "json", got: d.AppendJSON(nil, []byte(`{"author":"O'Reilly"}`)), want: `'{"author":"O''Reilly"}'`},
		{name: "bytes", got: d.AppendBytes(nil, []byte{0x00, 0x7f, 0xff}), want: "from_hex('007fff')"},
		{name: "nil bytes", got: d.AppendBytes(nil, nil), want: "NULL"},
		{name: "true", got: d.AppendBool(nil, true), want: "TRUE"},
		{name: "false", got: d.AppendBool(nil, false), want: "FALSE"},
		{name: "UTC timestamp", got: d.AppendTime(nil, timestamp), want: "TIMESTAMP '2026-08-02 16:30:00.123'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(tt.got))
		})
	}
}

func TestDialectDoesNotGenerateSequences(t *testing.T) {
	d := New()
	table := d.Tables().Get(reflect.TypeFor[dialectTypeFixture]())
	field := table.FieldMap["id"]
	require.NotNil(t, field)

	assert.Equal(t, "prefix", string(d.AppendSequence([]byte("prefix"), table, field)))
}
