package kept

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io"
)

// ErrLimitExceeded indicates that a selection exceeds its size limit.
var ErrLimitExceeded = errors.New("usage selection exceeds limit")

func EncodeRows[T any](rows []T, limit int64, size func(T) int64) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(len(rows)); err != nil {
		return nil, err
	}
	var total int64
	for _, row := range rows {
		total += size(row)
		if total > limit {
			return nil, ErrLimitExceeded
		}
		if err := enc.Encode(row); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func DecodeRows[T any](data []byte) ([]T, bool) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var count int
	if err := dec.Decode(&count); err != nil || count < 0 {
		return nil, false
	}
	rows := []T{}
	for range count {
		var row T
		if err := dec.Decode(&row); err != nil {
			return nil, false
		}
		rows = append(rows, row)
	}
	var extra T
	return rows, errors.Is(dec.Decode(&extra), io.EOF)
}
