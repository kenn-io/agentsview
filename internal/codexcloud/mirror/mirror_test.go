package mirror

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fetched = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func rec(kind Kind, data string) Record {
	return Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
}

func mirrorPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "account", "thread.jsonl.zstd")
}

func appendOK(t *testing.T, path string, offset int64, recs ...Record) AppendResult {
	t.Helper()
	result, err := Append(path, offset, recs)
	require.NoError(t, err)
	return result
}

func readOK(t *testing.T, path string) []Record {
	t.Helper()
	recs, err := ReadAll(t.Context(), path)
	require.NoError(t, err)
	return recs
}

func TestAppendThenReadAll(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	c := rec(KindTokenUsage, `{"turnId":"u1"}`)
	first := appendOK(t, path, 0, a)
	second := appendOK(t, path, first.Offset, b, c)
	assert.Empty(t, second.Recovered)
	assert.False(t, second.Truncated)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), second.Offset)
	assert.Equal(t, []Record{a, b, c}, readOK(t, path))
}

func TestAppendRecoversFramesPastStaleOffset(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	c := rec(KindItem, `{"id":"i2"}`)
	afterA := appendOK(t, path, 0, a).Offset
	appendOK(t, path, afterA, b)
	result := appendOK(t, path, afterA, c)
	assert.Equal(t, []Record{b}, result.Recovered)
	assert.Equal(t, []Record{a, b, c}, readOK(t, path))
}

func TestAppendFromZeroRecoversEverything(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	appendOK(t, path, 0, a)
	result := appendOK(t, path, 0, b)
	assert.Equal(t, []Record{a}, result.Recovered)
	assert.Equal(t, []Record{a, b}, readOK(t, path))
}

func TestAppendTruncatesTornTail(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	afterA := appendOK(t, path, 0, a).Offset
	appendOK(t, path, afterA, rec(KindItem, `{"id":"torn","output":"0123456789"}`))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(path, afterA+(info.Size()-afterA)/2))

	assert.Equal(t, []Record{a}, readOK(t, path), "a torn tail contributes nothing")
	result := appendOK(t, path, afterA, b)
	assert.True(t, result.Truncated)
	assert.Empty(t, result.Recovered)
	assert.Equal(t, []Record{a, b}, readOK(t, path))
}

func TestAppendScansWhenOffsetPastEOF(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	afterA := appendOK(t, path, 0, a).Offset
	result := appendOK(t, path, afterA+1000, b)
	assert.Equal(t, []Record{a}, result.Recovered)
	assert.Equal(t, []Record{a, b}, readOK(t, path))
}

func TestAppendScansWhenOffsetMidFrame(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	b := rec(KindItem, `{"id":"i1"}`)
	afterA := appendOK(t, path, 0, a).Offset
	result := appendOK(t, path, afterA-3, b)
	assert.Equal(t, []Record{a}, result.Recovered)
	assert.Equal(t, []Record{a, b}, readOK(t, path))
}

func TestAppendNothingOnlyRecovers(t *testing.T) {
	path := mirrorPath(t)
	a := rec(KindThread, `{"id":"t1"}`)
	end := appendOK(t, path, 0, a).Offset
	result := appendOK(t, path, 0)
	assert.Equal(t, []Record{a}, result.Recovered)
	assert.Equal(t, end, result.Offset)
}

func TestAppendCreatesPrivateFile(t *testing.T) {
	path := mirrorPath(t)
	appendOK(t, path, 0, rec(KindThread, `{"id":"t1"}`))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestReadAllRejectsCorruptFile(t *testing.T) {
	path := mirrorPath(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("plain text"), 0o600))
	_, err := ReadAll(t.Context(), path)
	require.Error(t, err)
	_, err = Append(path, 0, []Record{rec(KindThread, `{}`)})
	require.Error(t, err, "Append must not write after bytes it cannot read")
}
