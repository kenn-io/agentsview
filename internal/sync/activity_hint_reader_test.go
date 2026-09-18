package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type literalActivityHintDecoder struct{}

func (literalActivityHintDecoder) ActivityHintSources(
	context.Context,
) ([]parser.ActivityHintSource, error) {
	return nil, nil
}

func (literalActivityHintDecoder) DecodeActivityHint(
	line []byte,
) (parser.ActivityHint, bool) {
	var id string
	var ts int64
	if _, err := fmt.Sscanf(string(line), "%s %d", &id, &ts); err != nil {
		return parser.ActivityHint{}, false
	}
	return parser.ActivityHint{
		RawSessionID: id,
		Timestamp:    time.Unix(ts, 0).UTC(),
	}, true
}

type countingActivityHintDecoder struct {
	decoded int
}

func (d *countingActivityHintDecoder) ActivityHintSources(
	context.Context,
) ([]parser.ActivityHintSource, error) {
	return nil, nil
}

func (d *countingActivityHintDecoder) DecodeActivityHint(
	line []byte,
) (parser.ActivityHint, bool) {
	d.decoded++
	return literalActivityHintDecoder{}.DecodeActivityHint(line)
}

func activityHintCursorRetainsText(
	cursor *activityHintCursor,
	text string,
) bool {
	value := reflect.ValueOf(*cursor)
	needle := []byte(text)
	for _, field := range value.Fields() {
		switch {
		case field.Kind() == reflect.String:
			if strings.Contains(field.String(), text) {
				return true
			}
		case field.Kind() == reflect.Slice &&
			field.Type().Elem().Kind() == reflect.Uint8:
			if bytes.Contains(field.Bytes(), needle) {
				return true
			}
		case field.Kind() == reflect.Array &&
			field.Type().Elem().Kind() == reflect.Uint8:
			candidate := make([]byte, field.Len())
			for j := range field.Len() {
				candidate[j] = byte(field.Index(j).Uint())
			}
			if bytes.Contains(candidate, needle) {
				return true
			}
		}
	}
	return false
}

func TestReadActivityHintsBootstrapIsRecentAndBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	padding := strings.Repeat("x", activityHintBootstrapBytes) + "\n"
	content := padding +
		hintRecord("old", now.Add(-25*time.Hour)) +
		hintRecord("recent", now.Add(-23*time.Hour))
	require.NoError(os.WriteFile(path, []byte(content), 0o644))
	cursor := &activityHintCursor{}

	got, err := readActivityHints(t.Context(),
		parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("recent", got.Hints[0].RawSessionID)
	assert.LessOrEqual(got.BytesRead, activityHintBootstrapBytes)
	assert.Equal(int64(len(content)), cursor.offset)
	assert.True(cursor.initialized)
}

func TestReadActivityHintsRetainsPartialAndDeduplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(path, []byte(hintRecord("first", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	partial := strings.TrimSuffix(hintRecord("later", now), "\n")
	_, err = file.WriteString(partial)
	require.NoError(err)
	require.NoError(file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	assert.Empty(got.Hints)
	assert.True(cursor.hasPartial)
	assert.Equal(
		int64(len(hintRecord("first", now))), cursor.partialOffset,
	)

	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = file.WriteString("\n" + hintRecord("later", now))
	require.NoError(err)
	require.NoError(file.Close())
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("later", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsDoesNotRetainHistoryContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	const prompt = "private-prompt-sentinel"
	content := hintRecord("seed", now) + prompt + "\n" + prompt
	require.NoError(os.WriteFile(path, []byte(content), 0o644))
	cursor := &activityHintCursor{}

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("seed", got.Hints[0].RawSessionID)
	assert.False(activityHintCursorRetainsText(cursor, prompt))

	appendFile(t, path, "\n"+hintRecord("later", now))
	got, err = readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("later", got.Hints[0].RawSessionID)
	assert.False(activityHintCursorRetainsText(cursor, prompt))
}

func TestReadActivityHintsDropsOversizeLine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := strings.Repeat("private-prompt-sentinel", activityHintMaxLineBytes/8) +
		"\n" + hintRecord("valid", now)
	require.NoError(os.WriteFile(path, []byte(content), 0o644))

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("valid", got.Hints[0].RawSessionID)
	assert.NotContains(fmt.Sprint(err), "private-prompt-sentinel")
}

func TestReadActivityHintsIncrementalOverflowKeepsNewestTail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(path, []byte(hintRecord("seed", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = file.WriteString(strings.Repeat("x", activityHintMaxReadBytes+1024) +
		"\n" + hintRecord("newest", now))
	require.NoError(err)
	require.NoError(file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(err)
	assert.True(got.Overflow)
	assert.LessOrEqual(got.BytesRead, activityHintMaxReadBytes)
	require.Len(got.Hints, 1)
	assert.Equal("newest", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsResetsAfterReplacementAndTruncation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.WriteFile(path, []byte(hintRecord("first", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)

	replacement := path + ".new"
	require.NoError(os.WriteFile(replacement, []byte(hintRecord("replacement", now)), 0o644))
	require.NoError(os.Rename(replacement, path))
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("replacement", got.Hints[0].RawSessionID)

	require.NoError(os.WriteFile(path, []byte(hintRecord("short", now)), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("short", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsMissingThenCreatedAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	cursor := &activityHintCursor{}
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	assert.Empty(got.Hints)
	assert.False(cursor.initialized)

	require.NoError(os.WriteFile(path, []byte(
		hintRecord("same", now)+hintRecord("same", now),
	), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal("same", got.Hints[0].RawSessionID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readActivityHints(ctx, parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	assert.ErrorIs(err, context.Canceled)
}

func TestReadActivityHintsErrorNamesPathWithoutRecordContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(os.Mkdir(path, 0o755))

	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.Error(err)
	assert.Contains(err.Error(), fmt.Sprintf("%q", path))
	assert.NotContains(err.Error(), "private-prompt-sentinel")
}

func TestReadActivityHintsCapsDecodingAndKeepsNewestRecords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	var records strings.Builder
	for i := range activityHintMaxIDsPerPoll + 1 {
		records.WriteString(hintRecord(fmt.Sprintf("id-%05d", i), now))
	}
	require.NoError(os.WriteFile(path, []byte(records.String()), 0o644))
	decoder := &countingActivityHintDecoder{}
	cursor := &activityHintCursor{}

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		decoder, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(err)
	assert.True(got.Overflow)
	assert.Equal(activityHintMaxIDsPerPoll, got.RecordsDecoded)
	assert.Equal(activityHintMaxIDsPerPoll, decoder.decoded)
	assert.Len(got.Hints, activityHintMaxIDsPerPoll)
	assert.Equal("id-08192", got.Hints[0].RawSessionID)
	assert.Equal(int64(records.Len()), cursor.offset)
}

func TestReadActivityHintsDetectsSameInodeTruncateAndRegrow(t *testing.T) {
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	initial := hintRecord("initial", now) + strings.Repeat("x", 256)
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)
	require.NoError(err)
	initialInfo, err := os.Stat(path)
	require.NoError(err)

	rewritten := hintRecord("rewritten", now) + strings.Repeat("y", len(initial)+128)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(err)
	_, err = file.WriteString(rewritten)
	require.NoError(err)
	require.NoError(file.Close())
	rewrittenInfo, err := os.Stat(path)
	require.NoError(err)
	require.True(os.SameFile(initialInfo, rewrittenInfo))
	require.Greater(rewrittenInfo.Size(), int64(len(initial)))

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal(t, "rewritten", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsDetectsSameInodeEqualSizeRewrite(t *testing.T) {
	require := require.New(t)

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	initial := hintRecord("initial", now) + strings.Repeat("x", 256)
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))
	oldMTime := now.Add(-time.Hour)
	require.NoError(os.Chtimes(path, oldMTime, oldMTime))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)
	require.NoError(err)
	initialInfo, err := os.Stat(path)
	require.NoError(err)

	rewritten := hintRecord("rewrite", now) + strings.Repeat("y", 256)
	require.Len([]byte(rewritten), len(initial))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(err)
	_, err = file.WriteString(rewritten)
	require.NoError(err)
	require.NoError(file.Close())
	require.NoError(os.Chtimes(path, now, now))
	rewrittenInfo, err := os.Stat(path)
	require.NoError(err)
	require.True(os.SameFile(initialInfo, rewrittenInfo))
	require.Equal(initialInfo.Size(), rewrittenInfo.Size())
	require.NotEqual(initialInfo.ModTime(), rewrittenInfo.ModTime())

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(err)
	require.Len(got.Hints, 1)
	assert.Equal(t, "rewrite", got.Hints[0].RawSessionID)
}

func hintRecord(id string, timestamp time.Time) string {
	return fmt.Sprintf("%s %d\n", id, timestamp.Unix())
}
