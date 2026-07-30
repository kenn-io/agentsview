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
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	padding := strings.Repeat("x", activityHintBootstrapBytes) + "\n"
	content := padding +
		hintRecord("old", now.Add(-25*time.Hour)) +
		hintRecord("recent", now.Add(-23*time.Hour))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	cursor := &activityHintCursor{}

	got, err := readActivityHints(t.Context(),
		parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "recent", got.Hints[0].RawSessionID)
	assert.LessOrEqual(t, got.BytesRead, activityHintBootstrapBytes)
	assert.Equal(t, int64(len(content)), cursor.offset)
	assert.True(t, cursor.initialized)
}

func TestReadActivityHintsRetainsPartialAndDeduplicates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(hintRecord("first", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	partial := strings.TrimSuffix(hintRecord("later", now), "\n")
	_, err = file.WriteString(partial)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	assert.Empty(t, got.Hints)
	assert.True(t, cursor.hasPartial)
	assert.Equal(
		t, int64(len(hintRecord("first", now))), cursor.partialOffset,
	)

	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("\n" + hintRecord("later", now))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "later", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsDoesNotRetainHistoryContent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	const prompt = "private-prompt-sentinel"
	content := hintRecord("seed", now) + prompt + "\n" + prompt
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	cursor := &activityHintCursor{}

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "seed", got.Hints[0].RawSessionID)
	assert.False(t, activityHintCursorRetainsText(cursor, prompt))

	appendFile(t, path, "\n"+hintRecord("later", now))
	got, err = readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "later", got.Hints[0].RawSessionID)
	assert.False(t, activityHintCursorRetainsText(cursor, prompt))
}

func TestReadActivityHintsDropsOversizeLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := strings.Repeat("private-prompt-sentinel", activityHintMaxLineBytes/8) +
		"\n" + hintRecord("valid", now)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "valid", got.Hints[0].RawSessionID)
	assert.NotContains(t, fmt.Sprint(err), "private-prompt-sentinel")
}

func TestReadActivityHintsIncrementalOverflowKeepsNewestTail(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(hintRecord("seed", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(strings.Repeat("x", activityHintMaxReadBytes+1024) +
		"\n" + hintRecord("newest", now))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.NoError(t, err)
	assert.True(t, got.Overflow)
	assert.LessOrEqual(t, got.BytesRead, activityHintMaxReadBytes)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "newest", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsResetsAfterReplacementAndTruncation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(hintRecord("first", now)), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)

	replacement := path + ".new"
	require.NoError(t, os.WriteFile(replacement, []byte(hintRecord("replacement", now)), 0o644))
	require.NoError(t, os.Rename(replacement, path))
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "replacement", got.Hints[0].RawSessionID)

	require.NoError(t, os.WriteFile(path, []byte(hintRecord("short", now)), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "short", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsMissingThenCreatedAndCancellation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	cursor := &activityHintCursor{}
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	assert.Empty(t, got.Hints)
	assert.False(t, cursor.initialized)

	require.NoError(t, os.WriteFile(path, []byte(
		hintRecord("same", now)+hintRecord("same", now),
	), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "same", got.Hints[0].RawSessionID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readActivityHints(ctx, parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestReadActivityHintsErrorNamesPathWithoutRecordContent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.Mkdir(path, 0o755))

	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll)

	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", path))
	assert.NotContains(t, err.Error(), "private-prompt-sentinel")
}

func TestReadActivityHintsCapsDecodingAndKeepsNewestRecords(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	var records strings.Builder
	for i := range activityHintMaxIDsPerPoll + 1 {
		records.WriteString(hintRecord(fmt.Sprintf("id-%05d", i), now))
	}
	require.NoError(t, os.WriteFile(path, []byte(records.String()), 0o644))
	decoder := &countingActivityHintDecoder{}
	cursor := &activityHintCursor{}

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		decoder, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(t, err)
	assert.True(t, got.Overflow)
	assert.Equal(t, activityHintMaxIDsPerPoll, got.RecordsDecoded)
	assert.Equal(t, activityHintMaxIDsPerPoll, decoder.decoded)
	assert.Len(t, got.Hints, activityHintMaxIDsPerPoll)
	assert.Equal(t, "id-08192", got.Hints[0].RawSessionID)
	assert.Equal(t, int64(records.Len()), cursor.offset)
}

func TestReadActivityHintsDetectsSameInodeTruncateAndRegrow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	initial := hintRecord("initial", now) + strings.Repeat("x", 256)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)
	require.NoError(t, err)
	initialInfo, err := os.Stat(path)
	require.NoError(t, err)

	rewritten := hintRecord("rewritten", now) + strings.Repeat("y", len(initial)+128)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = file.WriteString(rewritten)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	rewrittenInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(initialInfo, rewrittenInfo))
	require.Greater(t, rewrittenInfo.Size(), int64(len(initial)))

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "rewritten", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsDetectsSameInodeEqualSizeRewrite(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	initial := hintRecord("initial", now) + strings.Repeat("x", 256)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
	oldMTime := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, oldMTime, oldMTime))
	cursor := &activityHintCursor{}
	_, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)
	require.NoError(t, err)
	initialInfo, err := os.Stat(path)
	require.NoError(t, err)

	rewritten := hintRecord("rewrite", now) + strings.Repeat("y", 256)
	require.Len(t, []byte(rewritten), len(initial))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = file.WriteString(rewritten)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.Chtimes(path, now, now))
	rewrittenInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(initialInfo, rewrittenInfo))
	require.Equal(t, initialInfo.Size(), rewrittenInfo.Size())
	require.NotEqual(t, initialInfo.ModTime(), rewrittenInfo.ModTime())

	got, err := readActivityHints(
		t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now,
		activityHintMaxReadBytes, activityHintMaxIDsPerPoll,
	)

	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "rewrite", got.Hints[0].RawSessionID)
}

func hintRecord(id string, timestamp time.Time) string {
	return fmt.Sprintf("%s %d\n", id, timestamp.Unix())
}
