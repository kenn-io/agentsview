package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	activityHintBootstrapBytes    = 4 << 20
	activityHintBootstrapLookback = 24 * time.Hour
	activityHintMaxReadBytes      = 4 << 20
	activityHintMaxLineBytes      = 1 << 20
	activityHintMaxIDsPerPoll     = 8192
	activityHintBoundaryBytes     = 64
)

type activityHintCursor struct {
	info         os.FileInfo
	offset       int64
	boundary     []byte
	partial      []byte
	initialized  bool
	droppingLine bool
}

type activityHintReadResult struct {
	Hints          []parser.ActivityHint
	BytesRead      int
	RecordsDecoded int
	Overflow       bool
}

func readActivityHints(
	ctx context.Context,
	source parser.ActivityHintSource,
	decoder parser.ActivityHintProvider,
	cursor *activityHintCursor,
	now time.Time,
	maxBytes int,
	maxRecords int,
) (activityHintReadResult, error) {
	if err := ctx.Err(); err != nil {
		return activityHintReadResult{}, err
	}
	info, err := os.Stat(source.Path)
	if errors.Is(err, os.ErrNotExist) {
		*cursor = activityHintCursor{}
		return activityHintReadResult{}, nil
	}
	if err != nil {
		return activityHintReadResult{}, fmt.Errorf(
			"stat activity hint %q: %w", source.Path, err,
		)
	}

	if cursor.initialized &&
		(cursor.info == nil ||
			!os.SameFile(cursor.info, info) ||
			info.Size() < cursor.offset) {
		*cursor = activityHintCursor{}
	}
	if cursor.initialized && info.Size() >= cursor.offset &&
		(info.Size() > cursor.offset ||
			cursor.info != nil &&
				!info.ModTime().Equal(cursor.info.ModTime())) {
		if !activityHintBoundaryMatches(source.Path, cursor) {
			*cursor = activityHintCursor{}
		} else if info.Size() == cursor.offset {
			cursor.info = info
		}
	}

	bootstrap := !cursor.initialized
	start := cursor.offset
	shifted := false
	result := activityHintReadResult{}
	if bootstrap {
		start = max(
			int64(0),
			info.Size()-int64(min(activityHintBootstrapBytes, maxBytes)),
		)
		shifted = start > 0
	} else if unread := info.Size() - cursor.offset; unread > int64(maxBytes) {
		start = info.Size() - int64(maxBytes)
		shifted = true
		result.Overflow = true
		cursor.partial = nil
		cursor.droppingLine = false
	}

	file, err := os.Open(source.Path)
	if err != nil {
		return activityHintReadResult{}, fmt.Errorf(
			"open activity hint %q: %w", source.Path, err,
		)
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return activityHintReadResult{}, fmt.Errorf(
			"seek activity hint %q: %w", source.Path, err,
		)
	}

	previousOffset := cursor.offset
	previousBoundary := append([]byte(nil), cursor.boundary...)
	wasInitialized := cursor.initialized
	readLimit := min(info.Size()-start, int64(maxBytes))
	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return activityHintReadResult{}, fmt.Errorf(
			"read activity hint %q: %w", source.Path, err,
		)
	}
	if err := ctx.Err(); err != nil {
		return activityHintReadResult{}, err
	}

	result.BytesRead = len(data)
	cursor.info = info
	cursor.offset = start + int64(len(data))
	cursor.initialized = true
	if wasInitialized && start == previousOffset {
		previousBoundary = append(previousBoundary, data...)
		cursor.boundary = retainActivityHintBoundary(previousBoundary)
	} else {
		cursor.boundary = retainActivityHintBoundary(data)
	}

	if shifted {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			cursor.partial = nil
			cursor.droppingLine = true
			return result, nil
		}
		data = data[newline+1:]
	}

	seen := make(map[string]struct{})
	cutoff := now.Add(-activityHintBootstrapLookback)
	futureCutoff := now.Add(time.Minute)
	var canceled error
	decoded, overflow := consumeNewestActivityHintLines(
		cursor, data, maxRecords, func(line []byte) bool {
			if err := ctx.Err(); err != nil {
				canceled = err
				return false
			}
			hint, ok := decoder.DecodeActivityHint(line)
			if !ok ||
				hint.Timestamp.Before(cutoff) ||
				hint.Timestamp.After(futureCutoff) {
				return true
			}
			if _, ok := seen[hint.RawSessionID]; ok {
				return true
			}
			seen[hint.RawSessionID] = struct{}{}
			result.Hints = append(result.Hints, hint)
			return true
		})
	if canceled != nil {
		return activityHintReadResult{}, canceled
	}
	result.RecordsDecoded = decoded
	result.Overflow = result.Overflow || overflow
	return result, nil
}

func activityHintBoundaryMatches(
	path string,
	cursor *activityHintCursor,
) bool {
	if len(cursor.boundary) == 0 ||
		cursor.offset < int64(len(cursor.boundary)) {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	got := make([]byte, len(cursor.boundary))
	n, err := file.ReadAt(got, cursor.offset-int64(len(got)))
	return n == len(got) && err == nil && bytes.Equal(got, cursor.boundary)
}

func retainActivityHintBoundary(data []byte) []byte {
	if len(data) > activityHintBoundaryBytes {
		data = data[len(data)-activityHintBoundaryBytes:]
	}
	return append([]byte(nil), data...)
}

// consumeNewestActivityHintLines retains an incomplete trailing record for the
// next append and walks complete records backwards without materializing a
// slice per line. Newest-first traversal keeps recent activity when the input
// exceeds the poll-wide decode budget.
func consumeNewestActivityHintLines(
	cursor *activityHintCursor,
	data []byte,
	maxRecords int,
	consume func([]byte) bool,
) (decoded int, overflow bool) {
	if cursor.droppingLine {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return 0, false
		}
		data = data[newline+1:]
		cursor.droppingLine = false
	}

	buffer := make([]byte, 0, len(cursor.partial)+len(data))
	buffer = append(buffer, cursor.partial...)
	buffer = append(buffer, data...)
	cursor.partial = nil

	lastNewline := bytes.LastIndexByte(buffer, '\n')
	if lastNewline < 0 {
		if len(buffer) > activityHintMaxLineBytes {
			cursor.droppingLine = true
			return 0, false
		}
		cursor.partial = append(cursor.partial[:0], buffer...)
		return 0, false
	}

	trailing := buffer[lastNewline+1:]
	if len(trailing) > activityHintMaxLineBytes {
		cursor.droppingLine = true
	} else {
		cursor.partial = append(cursor.partial[:0], trailing...)
	}

	end := lastNewline
	for {
		previousNewline := bytes.LastIndexByte(buffer[:end], '\n')
		line := buffer[previousNewline+1 : end]
		if len(line) <= activityHintMaxLineBytes {
			if decoded >= maxRecords {
				return decoded, true
			}
			decoded++
			if !consume(line) {
				return decoded, false
			}
		}
		if previousNewline < 0 {
			return decoded, false
		}
		end = previousNewline
	}
}
