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
	if field.CreateTableSQLType == duckDBTypeTimestamp &&
		isDynamicTimestampDefault(field.SQLDefault) {
		field.SQLDefault = ""
	}

	if field.StructField.Type == jsonRawMessageType {
		field.Scan = scanJSONRawMessage
		return
	}
	if isIntegerKind(field.IndirectType.Kind()) {
		scan := field.Scan
		field.Scan = func(dest reflect.Value, src any) error {
			return scan(dest, normalizeIntegerSource(src))
		}
	}
}

func isDynamicTimestampDefault(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP()", "NOW()":
		return true
	default:
		return false
	}
}

func isIntegerKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Uint64
}

func normalizeIntegerSource(src any) any {
	switch value := src.(type) {
	case int:
		return int64(value)
	case int8:
		return int64(value)
	case int16:
		return int64(value)
	case int32:
		return int64(value)
	case uint:
		return uint64(value)
	case uint8:
		return uint64(value)
	case uint16:
		return uint64(value)
	case uint32:
		return uint64(value)
	default:
		return src
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
