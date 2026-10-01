package serdejson

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
)

// ObjectWithDuplicateKeys holds the last value for each field, plus the field
// names that appeared more than once while decoding the object.
type ObjectWithDuplicateKeys struct {
	Fields        map[string]any
	DuplicateKeys []string
}

// Decode parses one JSON value into the serdejson value model: nil, bool,
// string, Number, []any and map[string]any. Like serde_json::from_slice it
// rejects trailing data, invalid UTF-8, lone surrogate escapes and
// out-of-range numbers; duplicate object keys keep the last value (serde's
// Map insert).
func Decode(data []byte) (any, error) {
	return decode(data, false)
}

// DecodeWithDuplicateKeys parses a JSON value like Decode, but returns each
// object as ObjectWithDuplicateKeys so typed callers can reject duplicate
// fields without changing last-wins behavior inside arbitrary values.
func DecodeWithDuplicateKeys(data []byte) (any, error) {
	return decode(data, true)
}

func decode(data []byte, preserveDuplicateKeys bool) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data),
		jsontext.AllowDuplicateNames(true))
	v, err := decodeValue(dec, preserveDuplicateKeys)
	if err != nil {
		return nil, fmt.Errorf("serdejson: decode: %w", err)
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("serdejson: decode: trailing characters")
	}
	return v, nil
}

func decodeValue(dec *jsontext.Decoder, preserveDuplicateKeys bool) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil
	case 't', 'f':
		return tok.Bool(), nil
	case '"':
		return tok.String(), nil
	case '0':
		n := Number(tok.String())
		if n.Kind() == NumberF64 {
			if _, ok := n.Float64(); !ok {
				return nil, fmt.Errorf("serdejson: number out of range: %q", string(n))
			}
		}
		return n, nil
	case '[':
		arr := []any{}
		for dec.PeekKind() != ']' {
			v, err := decodeValue(dec, preserveDuplicateKeys)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return arr, nil
	case '{':
		obj := map[string]any{}
		var duplicateKeys []string
		for dec.PeekKind() != '}' {
			keyTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			key := keyTok.String() // copy before the next read voids the token
			v, err := decodeValue(dec, preserveDuplicateKeys)
			if err != nil {
				return nil, err
			}
			if _, exists := obj[key]; exists && preserveDuplicateKeys {
				duplicateKeys = append(duplicateKeys, key)
			}
			obj[key] = v
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		if preserveDuplicateKeys {
			return ObjectWithDuplicateKeys{Fields: obj, DuplicateKeys: duplicateKeys}, nil
		}
		return obj, nil
	default:
		return nil, fmt.Errorf("unexpected token %v", tok.Kind())
	}
}

// PlainValue converts decoded objects with duplicate metadata back to the
// serdejson value model. Duplicate fields keep their last decoded value.
func PlainValue(v any) any {
	switch v := v.(type) {
	case ObjectWithDuplicateKeys:
		return plainObject(v.Fields)
	case map[string]any:
		return plainObject(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = PlainValue(item)
		}
		return out
	default:
		return v
	}
}

func plainObject(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for key, value := range v {
		out[key] = PlainValue(value)
	}
	return out
}
