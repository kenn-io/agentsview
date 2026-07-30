package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	info           os.FileInfo
	offset         int64
	boundaryDigest [sha256.Size]byte
	boundaryLength int
	partialOffset  int64
	initialized    bool
	hasPartial     bool
	droppingLine   bool
	lastUsed       uint64
}

type activityHintReadResult struct {
	Hints          []parser.ActivityHint
	BytesRead      int
	RecordsDecoded int
	Overflow       bool
	ByteOverflow   bool
	RecordOverflow bool
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
	if !info.Mode().IsRegular() {
		return activityHintReadResult{}, fmt.Errorf(
			"activity hint %q is not a regular file", source.Path,
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
	if !bootstrap && cursor.hasPartial && info.Size() > cursor.offset {
		start = cursor.partialOffset
	}
	shifted := false
	result := activityHintReadResult{}
	if bootstrap {
		result.ByteOverflow = info.Size() >
			int64(min(activityHintBootstrapBytes, maxBytes))
		start = max(
			int64(0),
			info.Size()-int64(min(activityHintBootstrapBytes, maxBytes)),
		)
		shifted = start > 0
	} else if unread := info.Size() - start; unread > int64(maxBytes) {
		start = info.Size() - int64(maxBytes)
		shifted = true
		result.ByteOverflow = true
		cursor.hasPartial = false
		cursor.droppingLine = false
	}
	result.Overflow = result.ByteOverflow

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
	digest, length, err := readActivityHintBoundary(file, cursor.offset)
	if err != nil {
		return activityHintReadResult{}, fmt.Errorf(
			"hash activity hint boundary %q: %w", source.Path, err,
		)
	}
	cursor.boundaryDigest = digest
	cursor.boundaryLength = length

	dataStart := start
	if shifted {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			cursor.hasPartial = false
			cursor.droppingLine = true
			return result, nil
		}
		data = data[newline+1:]
		dataStart += int64(newline + 1)
	}

	seen := make(map[string]struct{})
	cutoff := now.Add(-activityHintBootstrapLookback)
	futureCutoff := now.Add(time.Minute)
	var canceled error
	decoded, overflow := consumeNewestActivityHintLines(
		cursor, data, dataStart, maxRecords, func(line []byte) bool {
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
	result.RecordOverflow = overflow
	result.Overflow = result.Overflow || result.RecordOverflow
	return result, nil
}

func activityHintBoundaryMatches(
	path string,
	cursor *activityHintCursor,
) bool {
	if cursor.boundaryLength == 0 ||
		cursor.offset < int64(cursor.boundaryLength) {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	digest, length, err := readActivityHintBoundary(file, cursor.offset)
	return err == nil &&
		length == cursor.boundaryLength &&
		digest == cursor.boundaryDigest
}

func readActivityHintBoundary(
	file *os.File,
	offset int64,
) ([sha256.Size]byte, int, error) {
	length := int(min(offset, int64(activityHintBoundaryBytes)))
	if length == 0 {
		return [sha256.Size]byte{}, 0, nil
	}
	data := make([]byte, length)
	n, err := file.ReadAt(data, offset-int64(length))
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if n != length {
		return [sha256.Size]byte{}, 0, io.ErrUnexpectedEOF
	}
	return sha256.Sum256(data), length, nil
}

// consumeNewestActivityHintLines records the file offset of an incomplete
// trailing record and walks complete records backwards without materializing a
// slice per line. The next append rereads that record from disk so cursor state
// never retains record contents. Newest-first traversal keeps recent activity
// when the input exceeds the poll-wide decode budget.
func consumeNewestActivityHintLines(
	cursor *activityHintCursor,
	data []byte,
	dataStart int64,
	maxRecords int,
	consume func([]byte) bool,
) (decoded int, overflow bool) {
	if len(data) == 0 {
		return 0, false
	}
	cursor.hasPartial = false
	if cursor.droppingLine {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return 0, false
		}
		data = data[newline+1:]
		dataStart += int64(newline + 1)
		cursor.droppingLine = false
	}

	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		if len(data) > activityHintMaxLineBytes {
			cursor.droppingLine = true
			return 0, false
		}
		cursor.hasPartial = true
		cursor.partialOffset = dataStart
		return 0, false
	}

	trailing := data[lastNewline+1:]
	if len(trailing) > activityHintMaxLineBytes {
		cursor.droppingLine = true
	} else if len(trailing) > 0 {
		cursor.hasPartial = true
		cursor.partialOffset = dataStart + int64(lastNewline+1)
	}

	end := lastNewline
	for {
		previousNewline := bytes.LastIndexByte(data[:end], '\n')
		line := data[previousNewline+1 : end]
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
