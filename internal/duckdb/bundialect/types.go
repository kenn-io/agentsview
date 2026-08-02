package bundialect

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/uptrace/bun/dialect/sqltype"
	"github.com/uptrace/bun/schema"
)

var jsonRawMessageType = reflect.TypeFor[json.RawMessage]()

const (
	duckDBTypeText      = "TEXT"
	duckDBTypeDouble    = "DOUBLE"
	duckDBTypeTimestamp = "TIMESTAMP"
)

func fieldSQLType(field *schema.Field) string {
	if field.UserSQLType != "" {
		return normalizeSQLType(field.UserSQLType)
	}
	return normalizeSQLType(field.DiscoveredSQLType)
}

func onField(field *schema.Field) {
	field.DiscoveredSQLType = fieldSQLType(field)
	field.CreateTableSQLType = field.DiscoveredSQLType

	if field.StructField.Type == jsonRawMessageType {
		field.Scan = scanJSONRawMessage
	}
}

func scanJSONRawMessage(dest reflect.Value, src any) error {
	switch src := src.(type) {
	case nil:
		dest.SetBytes(nil)
		return nil
	case []byte:
		dest.SetBytes(bytes.Clone(src))
		return nil
	case string:
		dest.SetBytes([]byte(src))
		return nil
	default:
		value, err := json.Marshal(src)
		if err != nil {
			return err
		}
		dest.SetBytes(value)
		return nil
	}
}

func normalizeSQLType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "BOOL", sqltype.Boolean:
		return sqltype.Boolean
	case "INT2", sqltype.SmallInt:
		return sqltype.SmallInt
	case "INT", "INT4", sqltype.Integer:
		return sqltype.Integer
	case "INT8", sqltype.BigInt:
		return sqltype.BigInt
	case "FLOAT4", sqltype.Real:
		return sqltype.Real
	case "DOUBLE", "FLOAT8", sqltype.DoublePrecision:
		return duckDBTypeDouble
	case "CHARACTER VARYING", duckDBTypeText, sqltype.VarChar:
		return duckDBTypeText
	case "BYTEA", sqltype.Blob:
		return sqltype.Blob
	case sqltype.JSON, sqltype.JSONB:
		return sqltype.JSON
	case "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITHOUT TIME ZONE", sqltype.Timestamp:
		return duckDBTypeTimestamp
	default:
		return value
	}
}
