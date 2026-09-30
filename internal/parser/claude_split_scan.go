package parser

import (
	"bytes"
	"os"

	"github.com/tidwall/gjson"
)

// claudeSplitScanChunk bounds one backward read while locating the run.
const claudeSplitScanChunk = 64 << 10

// claudeSplitRecord classifies one transcript record seen by the
// backward scan.
type claudeSplitRecord int

const (
	// claudeSplitSkip is a record that produces no message, such as an
	// attachment, progress event, or system line. The chunk merge in
	// claudeParseSessionFrom ignores these, so they do not break a run.
	claudeSplitSkip claudeSplitRecord = iota
	// claudeSplitRun is an assistant record sharing the run's message id.
	claudeSplitRun
	// claudeSplitBoundary is a user record or an assistant record with a
	// different message id: the run cannot extend past it.
	claudeSplitBoundary
	// claudeSplitUnknown is a record the scan cannot classify. The
	// caller keeps the whole-transcript fallback.
	claudeSplitUnknown
)

// ClaudeSplitVerdict is the backward scan's answer about the records just
// before a stored offset.
type ClaudeSplitVerdict int

const (
	// ClaudeSplitUnknown means the scan could not classify the records,
	// such as a malformed line; only a whole-transcript parse is safe.
	ClaudeSplitUnknown ClaudeSplitVerdict = iota
	// ClaudeSplitNone means the last message-producing record before the
	// offset is not part of the run, so the full parser would not merge
	// across the offset either and the appended window is a plain append.
	// Parallel tool calls produce this shape: a tool_result user record
	// sits between two assistant records that share message.id.
	ClaudeSplitNone
	// ClaudeSplitFound means the run straddles the offset and starts at
	// the returned byte offset.
	ClaudeSplitFound
)

// ClaudeSplitRunStart reports whether the same-message.id assistant run
// continued after offset already began before it, and where. Claude Code
// writes one API response as several JSONL records that share message.id,
// and a sync that stops inside the run stores only the records it read,
// collapsed into one message. Re-parsing from the run's first record
// reproduces the merge a full parse performs over the whole file.
//
// The scan walks records backwards from offset, skipping records that
// produce no message, and stops at the first record that ends the run.
func ClaudeSplitRunStart(
	path string, offset int64, messageID string,
) (int64, ClaudeSplitVerdict) {
	if offset <= 0 || messageID == "" {
		return 0, ClaudeSplitUnknown
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, ClaudeSplitUnknown
	}
	defer f.Close()

	reader := claudeBackwardReader{f: f, pos: offset}
	runStart := int64(-1)
	for {
		line, lineStart, ok, err := reader.prev()
		if err != nil {
			return 0, ClaudeSplitUnknown
		}
		if !ok {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		switch claudeSplitRecordClass(line, messageID) {
		case claudeSplitUnknown:
			return 0, ClaudeSplitUnknown
		case claudeSplitRun:
			runStart = lineStart
		case claudeSplitBoundary:
			if runStart < 0 {
				return 0, ClaudeSplitNone
			}
			return runStart, ClaudeSplitFound
		case claudeSplitSkip:
		}
	}
	if runStart < 0 {
		return 0, ClaudeSplitNone
	}
	return runStart, ClaudeSplitFound
}

func claudeSplitRecordClass(line []byte, messageID string) claudeSplitRecord {
	if !gjson.ValidBytes(line) {
		return claudeSplitUnknown
	}
	switch gjson.GetBytes(line, "type").Str {
	case "assistant":
		if gjson.GetBytes(line, "message.id").Str == messageID {
			return claudeSplitRun
		}
		return claudeSplitBoundary
	case "user":
		return claudeSplitBoundary
	}
	return claudeSplitSkip
}

// claudeBackwardReader yields complete records from the end of a region
// of a file. Each chunk of the region is read once; the line currently
// assembled is read separately, so a record that spans a chunk boundary
// is returned whole.
type claudeBackwardReader struct {
	f   *os.File
	pos int64 // exclusive end of the region not yet returned
	buf []byte
	lo  int64 // file offset of buf[0]
}

// prev returns the complete line ending immediately before pos. The
// second result is the line's start offset.
func (r *claudeBackwardReader) prev() ([]byte, int64, bool, error) {
	if r.pos <= 0 {
		return nil, 0, false, nil
	}
	for {
		if r.buf != nil && r.pos > r.lo {
			limit := min(int64(len(r.buf)), r.pos-r.lo)
			if idx := bytes.LastIndexByte(r.buf[:limit], '\n'); idx >= 0 {
				newline := r.lo + int64(idx)
				line, err := r.line(newline+1, r.pos)
				if err != nil {
					return nil, 0, false, err
				}
				r.pos = newline
				return line, newline + 1, true, nil
			}
			if r.lo == 0 {
				line, err := r.line(0, r.pos)
				if err != nil {
					return nil, 0, false, err
				}
				r.pos = 0
				return line, 0, true, nil
			}
		}
		hi := r.pos
		if r.buf != nil {
			hi = r.lo
		}
		lo := max(int64(0), hi-claudeSplitScanChunk)
		buf := make([]byte, hi-lo)
		if _, err := r.f.ReadAt(buf, lo); err != nil {
			return nil, 0, false, err
		}
		r.buf, r.lo = buf, lo
	}
}

func (r *claudeBackwardReader) line(start, end int64) ([]byte, error) {
	line := make([]byte, end-start)
	if len(line) == 0 {
		return line, nil
	}
	if _, err := r.f.ReadAt(line, start); err != nil {
		return nil, err
	}
	return line, nil
}
