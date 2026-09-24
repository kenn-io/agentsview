package serdejson

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Compact serializes v like serde_json::to_vec: no whitespace, map keys
// sorted byte-wise (BTreeMap order), struct fields in declaration order,
// and no escaping of '<', '>', '&', U+2028 or U+2029.
//
// Supported values: nil, bool, string, Number, Go integer and float
// kinds, slices and arrays, string-keyed maps, pointers and structs.
// Struct fields use the name in a `json:"name"` tag (else the Go field
// name); `json:"-"` skips a field and `,omitempty` omits it only when it
// is a nil pointer, interface, slice or map, matching Option::is_none.
func Compact(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, reflect.ValueOf(v), "", ""); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// CompactString returns Compact as a string. Unsupported Go types cause
// a panic; strings and numbers produced by Decode do not.
func CompactString(v any) string {
	b, err := Compact(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Pretty serializes v like serde_json::to_string_pretty: two-space
// indentation, a space after each object colon and no trailing newline.
func Pretty(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, reflect.ValueOf(v), "\n", "  "); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

var numberType = reflect.TypeFor[Number]()

func encode(b *bytes.Buffer, v reflect.Value, nl, indent string) error {
	return encodeAt(b, v, nl, indent, 0)
}

func encodeAt(b *bytes.Buffer, v reflect.Value, nl, indent string, depth int) error {
	if !v.IsValid() {
		b.WriteString("null")
		return nil
	}
	if v.Type() == numberType {
		s, err := Number(v.String()).canonical()
		if err != nil {
			return err
		}
		b.WriteString(s)
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			b.WriteString("null")
			return nil
		}
		return encodeAt(b, v.Elem(), nl, indent, depth)
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.String:
		writeString(b, v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32:
		// First round to the shortest f32 digits, then apply serde's
		// f64 notation thresholds to those digits. Converting directly
		// to f64 would expose the f32 representation's extra digits.
		shortest := strconv.FormatFloat(v.Float(), 'g', -1, 32)
		f, err := strconv.ParseFloat(shortest, 64)
		if err != nil {
			return fmt.Errorf("serdejson: format float32: %w", err)
		}
		b.WriteString(formatFloat(f))
	case reflect.Float64:
		b.WriteString(formatFloat(v.Float()))
	case reflect.Slice, reflect.Array:
		return encodeSeq(b, v, nl, indent, depth)
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("serdejson: unsupported map key type %s", v.Type().Key())
		}
		return encodeMap(b, v, nl, indent, depth)
	case reflect.Struct:
		return encodeStruct(b, v, nl, indent, depth)
	default:
		return fmt.Errorf("serdejson: unsupported type %s", v.Type())
	}
	return nil
}

func newline(b *bytes.Buffer, nl, indent string, depth int) {
	if nl == "" {
		return
	}
	b.WriteString(nl)
	b.WriteString(strings.Repeat(indent, depth))
}

func encodeSeq(b *bytes.Buffer, v reflect.Value, nl, indent string, depth int) error {
	if v.Len() == 0 {
		b.WriteString("[]")
		return nil
	}
	b.WriteByte('[')
	for i := range v.Len() {
		if i > 0 {
			b.WriteByte(',')
		}
		newline(b, nl, indent, depth+1)
		if err := encodeAt(b, v.Index(i), nl, indent, depth+1); err != nil {
			return err
		}
	}
	newline(b, nl, indent, depth)
	b.WriteByte(']')
	return nil
}

func encodeMap(b *bytes.Buffer, v reflect.Value, nl, indent string, depth int) error {
	if v.Len() == 0 {
		b.WriteString("{}")
		return nil
	}
	keys := v.MapKeys()
	slices.SortFunc(keys, func(a, c reflect.Value) int {
		return strings.Compare(a.String(), c.String())
	})
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		newline(b, nl, indent, depth+1)
		writeString(b, key.String())
		b.WriteByte(':')
		if nl != "" {
			b.WriteByte(' ')
		}
		if err := encodeAt(b, v.MapIndex(key), nl, indent, depth+1); err != nil {
			return err
		}
	}
	newline(b, nl, indent, depth)
	b.WriteByte('}')
	return nil
}

func encodeStruct(b *bytes.Buffer, v reflect.Value, nl, indent string, depth int) error {
	t := v.Type()
	wrote := false
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		value := v.Field(i)
		if hasOption(opts, "omitempty") && isNilish(value) {
			continue
		}
		if wrote {
			b.WriteByte(',')
		} else {
			b.WriteByte('{')
		}
		wrote = true
		newline(b, nl, indent, depth+1)
		writeString(b, name)
		b.WriteByte(':')
		if nl != "" {
			b.WriteByte(' ')
		}
		if err := encodeAt(b, value, nl, indent, depth+1); err != nil {
			return err
		}
	}
	if !wrote {
		b.WriteString("{}")
		return nil
	}
	newline(b, nl, indent, depth)
	b.WriteByte('}')
	return nil
}

func hasOption(opts, want string) bool {
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == want {
			return true
		}
	}
	return false
}

func isNilish(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		return v.IsNil()
	default:
		return false
	}
}

const hexDigits = "0123456789abcdef"

// writeString ports serde_json's format_escaped_str: only a quotation
// mark, backslash and bytes below 0x20 are escaped. Invalid UTF-8,
// impossible in Rust strings, is replaced with U+FFFD.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}
