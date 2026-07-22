package parser

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/tidwall/gjson"
)

// countingReader wraps an io.Reader and counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// lineReader reads JSONL files line by line, skipping lines that
// exceed maxLen rather than aborting. The buffer starts small and
// grows on demand up to maxLen. After iteration, call Err() to
// check for I/O errors (as opposed to normal EOF).
type lineReader struct {
	r         *bufio.Reader
	cr        *countingReader
	maxLen    int
	buf       []byte
	err       error
	bytesRead int64 // total bytes consumed (from countingReader)
}

const maxPooledLineBufferSize = 256 << 10

var lineReaderPool = sync.Pool{
	New: func() any {
		cr := new(countingReader)
		return &lineReader{
			r:  bufio.NewReaderSize(cr, initialScanBufSize),
			cr: cr,
		}
	},
}

func newLineReader(r io.Reader, maxLen int) *lineReader {
	lr := lineReaderPool.Get().(*lineReader)
	lr.cr.r = r
	lr.cr.n = 0
	lr.r.Reset(lr.cr)
	lr.maxLen = maxLen
	lr.buf = lr.buf[:0]
	lr.err = nil
	lr.bytesRead = 0
	return lr
}

func releaseLineReader(lr *lineReader) {
	// Do not let an exceptional long line pin a multi-megabyte backing
	// array. sync.Pool may discard the remaining workspace at any GC.
	if cap(lr.buf) > maxPooledLineBufferSize {
		lr.buf = nil
	} else {
		lr.buf = lr.buf[:0]
	}
	lr.r.Reset(nil)
	lr.cr.r = nil
	lr.cr.n = 0
	lr.maxLen = 0
	lr.err = nil
	lr.bytesRead = 0
	lineReaderPool.Put(lr)
}

// next returns the next line (without trailing newline) and true,
// or ("", false) at EOF or read error. After the loop, call Err()
// to distinguish EOF from I/O failure.
func (lr *lineReader) next() (string, bool) {
	line, ok := lr.nextBytes()
	if !ok {
		return "", false
	}
	return string(line), true
}

// nextBytes returns the next line as storage borrowed from the reader. The
// bytes remain valid only until the next call. Callers that retain a line must
// copy it; callers that only classify or discard it can avoid allocating a
// string proportional to the source record.
func (lr *lineReader) nextBytes() ([]byte, bool) {
	for {
		line, err := lr.readLineBytes()
		if err != nil {
			if err != io.EOF {
				lr.err = err
			}
			return nil, false
		}
		if len(line) != 0 {
			return line, true
		}
		// Empty line or skipped oversized line — continue.
	}
}

// Err returns the first non-EOF read error encountered, or nil.
func (lr *lineReader) Err() error {
	return lr.err
}

// readLine reads a full line, returning "" for blank/oversized
// lines and a non-nil error only at EOF or read failure.
// updateBytesRead computes total bytes consumed by
// subtracting any buffered-but-not-consumed data from the
// countingReader total.
func (lr *lineReader) updateBytesRead() {
	if lr.cr != nil {
		lr.bytesRead = lr.cr.n - int64(lr.r.Buffered())
	}
}

func (lr *lineReader) readLineBytes() ([]byte, error) {
	lr.buf = lr.buf[:0]
	oversized := false

	for {
		chunk, isPrefix, err := lr.r.ReadLine()
		if err != nil {
			if len(lr.buf) > 0 && err == io.EOF {
				lr.updateBytesRead()
				break
			}
			return nil, err
		}

		if oversized {
			if !isPrefix {
				lr.updateBytesRead()
				return nil, nil // done skipping
			}
			continue
		}

		// ReadLine's common case is a complete line backed by the
		// bufio.Reader buffer. Convert it directly instead of allocating
		// a second initialScanBufSize scratch buffer for every file.
		if len(lr.buf) == 0 && !isPrefix {
			lr.updateBytesRead()
			if len(chunk) > lr.maxLen {
				return nil, nil
			}
			return chunk, nil
		}

		lr.buf = append(lr.buf, chunk...)

		if len(lr.buf) > lr.maxLen {
			oversized = true
			lr.buf = lr.buf[:0]
			if !isPrefix {
				lr.updateBytesRead()
				return nil, nil
			}
			continue
		}

		if !isPrefix {
			lr.updateBytesRead()
			break
		}
	}

	return lr.buf, nil
}

// readJSONLFrom opens a JSONL file, seeks to offset, and
// calls fn for each valid JSON line. Returns the number of
// bytes consumed (relative to offset) and any I/O error. The
// returned byte count covers only complete lines, so callers
// can use offset+consumed as a safe resume point even if the
// file had a partially written line at EOF.
func readJSONLFrom(
	path string, offset int64, fn func(line string),
) (consumed int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf(
			"seek %s to %d: %w", path, offset, err,
		)
	}

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if gjson.Valid(line) {
			fn(line)
			// Track offset through last valid JSON line
			// so partial lines at EOF are not skipped.
			consumed = lr.bytesRead
		}
	}
	return consumed, lr.Err()
}
