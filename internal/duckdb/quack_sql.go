// SQL literalization for the Quack read path: parameterized reads are
// rendered to literal SQL for agentsview_remote.query().
package duckdb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

func duckSQLWithArgs(stmt string, args ...any) (string, error) {
	var b strings.Builder
	argIndex := 0
	for _, r := range stmt {
		if r != '?' {
			b.WriteRune(r)
			continue
		}
		if argIndex >= len(args) {
			return "", fmt.Errorf("duckdb remote statement missing argument")
		}
		lit, err := duckValueLiteral(args[argIndex])
		if err != nil {
			return "", err
		}
		b.WriteString(lit)
		argIndex++
	}
	if argIndex != len(args) {
		return "", fmt.Errorf("duckdb remote statement has unused argument")
	}
	return b.String(), nil
}

func duckValueLiteral(v any) (string, error) {
	switch value := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return duckRemoteStringLiteral(value)
	case *string:
		if value == nil {
			return "NULL", nil
		}
		return duckRemoteStringLiteral(*value)
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(value), nil
	case *int:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *int64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *float64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case bool:
		if value {
			return "TRUE", nil
		}
		return "FALSE", nil
	case time.Time:
		return "TIMESTAMP " + duckLiteral(
			value.UTC().Format("2006-01-02 15:04:05.999999"),
		), nil
	default:
		// Mirror database/sql's default parameter converter so named kinds
		// (export.WorktreeRelationship and friends) render the same way on
		// the remote transport as they bind on the local mirror path.
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String:
			return duckRemoteStringLiteral(rv.String())
		case reflect.Bool:
			if rv.Bool() {
				return "TRUE", nil
			}
			return "FALSE", nil
		case reflect.Int, reflect.Int8, reflect.Int16,
			reflect.Int32, reflect.Int64:
			return strconv.FormatInt(rv.Int(), 10), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16,
			reflect.Uint32, reflect.Uint64:
			return strconv.FormatUint(rv.Uint(), 10), nil
		case reflect.Float32, reflect.Float64:
			return strconv.FormatFloat(rv.Float(), 'g', -1, 64), nil
		default:
			return "", fmt.Errorf("unsupported duckdb remote argument type %T", v)
		}
	}
}

func duckRemoteStringLiteral(s string) (string, error) {
	s = strings.ReplaceAll(s, "\x00", "")
	for {
		var tagBytes [16]byte
		if _, err := rand.Read(tagBytes[:]); err != nil {
			return "", fmt.Errorf("generating duckdb string literal tag: %w", err)
		}
		tag := "agentsview_" + hex.EncodeToString(tagBytes[:])
		delimiter := "$" + tag + "$"
		if strings.Contains(s, delimiter) {
			continue
		}
		return delimiter + s + delimiter, nil
	}
}
