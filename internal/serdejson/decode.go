package serdejson

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
)

// Decode parses one JSON value into the serdejson value model: nil, bool,
// string, Number, []any and map[string]any. Like serde_json::from_slice it
// rejects trailing data, invalid UTF-8, lone surrogate escapes and
// out-of-range numbers; duplicate object keys keep the last value (serde's
// Map insert).
func Decode(data []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data),
		jsontext.AllowDuplicateNames(true))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("serdejson: decode: %w", err)
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("serdejson: decode: trailing characters")
	}
	return v, nil
}

func decodeValue(dec *jsontext.Decoder) (any, error) {
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
			v, err := decodeValue(dec)
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
		for dec.PeekKind() != '}' {
			keyTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			key := keyTok.String() // copy before the next read voids the token
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			obj[key] = v
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return obj, nil
	default:
		return nil, fmt.Errorf("unexpected token %v", tok.Kind())
	}
}
