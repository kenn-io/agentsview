package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		literalActivityHintDecoder{}, cursor, now)

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
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	partial := strings.TrimSuffix(hintRecord("later", now), "\n")
	_, err = file.WriteString(partial)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	assert.Empty(t, got.Hints)
	assert.Equal(t, []byte(partial), cursor.partial)

	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("\n" + hintRecord("later", now))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "later", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsDropsOversizeLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := strings.Repeat("private-prompt-sentinel", activityHintMaxLineBytes/8) +
		"\n" + hintRecord("valid", now)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now)

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
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(strings.Repeat("x", activityHintMaxReadBytes+1024) +
		"\n" + hintRecord("newest", now))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)

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
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	oldInfo := cursor.info

	replacement := path + ".new"
	require.NoError(t, os.WriteFile(replacement, []byte(hintRecord("replacement", now)), 0o644))
	require.NoError(t, os.Rename(replacement, path))
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "replacement", got.Hints[0].RawSessionID)
	assert.False(t, os.SameFile(oldInfo, cursor.info))

	require.NoError(t, os.WriteFile(path, []byte(hintRecord("short", now)), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "short", got.Hints[0].RawSessionID)
}

func TestReadActivityHintsMissingThenCreatedAndCancellation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	cursor := &activityHintCursor{}
	got, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	assert.Empty(t, got.Hints)
	assert.False(t, cursor.initialized)

	require.NoError(t, os.WriteFile(path, []byte(
		hintRecord("same", now)+hintRecord("same", now),
	), 0o644))
	got, err = readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	require.NoError(t, err)
	require.Len(t, got.Hints, 1)
	assert.Equal(t, "same", got.Hints[0].RawSessionID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readActivityHints(ctx, parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, cursor, now)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestReadActivityHintsErrorNamesPathWithoutRecordContent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	require.NoError(t, os.Mkdir(path, 0o755))

	_, err := readActivityHints(t.Context(), parser.ActivityHintSource{Path: path},
		literalActivityHintDecoder{}, &activityHintCursor{}, now)

	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.NotContains(t, err.Error(), "private-prompt-sentinel")
}

func hintRecord(id string, timestamp time.Time) string {
	return fmt.Sprintf("%s %d\n", id, timestamp.Unix())
}
