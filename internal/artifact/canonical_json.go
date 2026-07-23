package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, reflect.ValueOf(v)); err != nil {
		return nil, fmt.Errorf("encoding canonical artifact JSON: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		buf.WriteString("null")
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalJSON(buf, v.Elem())
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := v.Interface().(json.RawMessage)
		if len(raw) == 0 {
			buf.WriteString("null")
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		// A second value or trailing garbage would be silently dropped from
		// the canonical bytes, hashing distinct raw content identically.
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
			return fmt.Errorf("unexpected content after JSON value in raw message")
		}
		return writeCanonicalJSON(buf, reflect.ValueOf(decoded))
	}
	if v.Type() == reflect.TypeFor[json.Number]() {
		buf.WriteString(v.Interface().(json.Number).String())
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		buf.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		data, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.String:
		data, err := json.Marshal(v.String())
		if err != nil {
			return err
		}
		buf.Write(data)
	case reflect.Slice, reflect.Array:
		buf.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, v.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case reflect.Map:
		return writeCanonicalMap(buf, v)
	case reflect.Struct:
		return writeCanonicalStruct(buf, v)
	default:
		return fmt.Errorf("unsupported canonical JSON kind %s", v.Kind())
	}
	return nil
}

func writeCanonicalMap(buf *bytes.Buffer, v reflect.Value) error {
	if v.IsNil() {
		buf.WriteString("null")
		return nil
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("unsupported canonical map key type %s", v.Type().Key())
	}
	keys := make([]string, 0, v.Len())
	for _, key := range v.MapKeys() {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyData, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(keyData)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, v.MapIndex(reflect.ValueOf(key))); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

type canonicalField struct {
	name  string
	value reflect.Value
}

func writeCanonicalStruct(buf *bytes.Buffer, v reflect.Value) error {
	fields := make([]canonicalField, 0, v.NumField())
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonField(field)
		if skip {
			continue
		}
		value := v.Field(i)
		if omitEmpty && isCanonicalEmpty(value) {
			continue
		}
		fields = append(fields, canonicalField{name: name, value: value})
	}
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].name < fields[j].name
	})

	buf.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := json.Marshal(field.name)
		if err != nil {
			return err
		}
		buf.Write(name)
		buf.WriteByte(':')
		if err := writeCanonicalJSON(buf, field.value); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func jsonField(field reflect.StructField) (name string, omitEmpty bool, skip bool) {
	name = field.Name
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag == "" {
		return name, false, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		name = parts[0]
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func isCanonicalEmpty(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Array:
		return v.Len() == 0
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
