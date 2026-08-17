package db

import (
	"fmt"
	"reflect"
)

// canonicalWriteBatchPayloadLimit matches the largest tool-result payload the
// archive deliberately persists. It caps the estimated pre-format row payload
// instead of guessing at a row count or applying an irrelevant bind-variable
// ceiling; SQL syntax, escaping, and Bun's intermediate copies add overhead
// beyond this target. A single larger logical row is still written alone
// because splitting one stored value would change the schema contract.
const canonicalWriteBatchPayloadLimit = 16 << 20

func writeCanonicalBatches[T any](
	rows []T,
	write func([]T) error,
) error {
	return forEachCanonicalWriteBatch(
		rows,
		canonicalWriteBatchPayloadLimit,
		canonicalBunRowWorkingSetBytes[T],
		write,
	)
}

func forEachCanonicalWriteBatch[T any](
	rows []T,
	maxBytes int,
	rowBytes func(T) int,
	write func([]T) error,
) error {
	if maxBytes <= 0 {
		return fmt.Errorf("canonical Bun write byte limit must be positive")
	}
	batchStart := 0
	batchBytes := 0
	for index, row := range rows {
		size := max(rowBytes(row), 1)
		if index > batchStart && size > maxBytes-batchBytes {
			if err := write(rows[batchStart:index]); err != nil {
				return err
			}
			batchStart = index
			batchBytes = 0
		}
		if index == batchStart {
			batchBytes = min(size, maxBytes)
		} else {
			batchBytes = min(batchBytes+size, maxBytes)
		}
	}
	if batchStart < len(rows) {
		return write(rows[batchStart:])
	}
	return nil
}

func canonicalBunRowWorkingSetBytes[T any](row T) int {
	value := reflect.ValueOf(row)
	if !value.IsValid() {
		return 1
	}
	return int(value.Type().Size()) + canonicalDynamicValueBytes(value)
}

func canonicalDynamicValueBytes(value reflect.Value) int {
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return 0
		}
		elem := value.Elem()
		return int(elem.Type().Size()) + canonicalDynamicValueBytes(elem)
	case reflect.String:
		return value.Len()
	case reflect.Slice:
		if value.IsNil() {
			return 0
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value.Len()
		}
		total := value.Len() * int(value.Type().Elem().Size())
		for index := range value.Len() {
			total += canonicalDynamicValueBytes(value.Index(index))
		}
		return total
	case reflect.Struct:
		total := 0
		// reflect.Value.Fields allocates an iterator on this write hot path.
		//nolint:modernize
		for index := range value.NumField() {
			total += canonicalDynamicValueBytes(value.Field(index))
		}
		return total
	default:
		return 0
	}
}
