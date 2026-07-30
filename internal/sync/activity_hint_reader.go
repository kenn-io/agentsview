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
)

type activityHintCursor struct {
	info         os.FileInfo
	offset       int64
	partial      []byte
	initialized  bool
	droppingLine bool
}

type activityHintReadResult struct {
	Hints     []parser.ActivityHint
	BytesRead int
	Overflow  bool
}

func readActivityHints(
	ctx context.Context,
	source parser.ActivityHintSource,
	decoder parser.ActivityHintProvider,
	cursor *activityHintCursor,
	now time.Time,
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

	bootstrap := !cursor.initialized
	start := cursor.offset
	shifted := false
	result := activityHintReadResult{}
	if bootstrap {
		start = max(int64(0), info.Size()-activityHintBootstrapBytes)
		shifted = start > 0
	} else if unread := info.Size() - cursor.offset; unread > activityHintMaxReadBytes {
		start = info.Size() - activityHintMaxReadBytes
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

	readLimit := min(info.Size()-start, int64(activityHintMaxReadBytes))
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
	consumeActivityHintLines(cursor, data, func(line []byte) {
		if canceled = ctx.Err(); canceled != nil {
			return
		}
		hint, ok := decoder.DecodeActivityHint(line)
		if !ok ||
			hint.Timestamp.Before(cutoff) ||
			hint.Timestamp.After(futureCutoff) {
			return
		}
		if _, ok := seen[hint.RawSessionID]; ok {
			return
		}
		if len(result.Hints) >= activityHintMaxIDsPerPoll {
			result.Overflow = true
			return
		}
		seen[hint.RawSessionID] = struct{}{}
		result.Hints = append(result.Hints, hint)
	})
	if canceled != nil {
		return activityHintReadResult{}, canceled
	}
	return result, nil
}

func consumeActivityHintLines(
	cursor *activityHintCursor,
	data []byte,
	consume func([]byte),
) {
	if cursor.droppingLine {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return
		}
		data = data[newline+1:]
		cursor.droppingLine = false
	}

	buffer := make([]byte, 0, len(cursor.partial)+len(data))
	buffer = append(buffer, cursor.partial...)
	buffer = append(buffer, data...)
	cursor.partial = nil

	for {
		newline := bytes.IndexByte(buffer, '\n')
		if newline < 0 {
			break
		}
		if newline <= activityHintMaxLineBytes {
			consume(buffer[:newline])
		}
		buffer = buffer[newline+1:]
	}
	if len(buffer) > activityHintMaxLineBytes {
		cursor.droppingLine = true
		return
	}
	cursor.partial = append(cursor.partial[:0], buffer...)
}
