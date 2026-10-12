// Package mirror reads and appends Codex cloud mirror files.
//
// A mirror file holds one thread's fetched records as JSON lines inside
// zstd frames. Each append writes exactly one frame and syncs it, so a
// crash can only tear the final frame. Readers ignore a torn tail; the next
// append truncates it.
package mirror

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/zstdframe"
)

// Kind names what a record holds.
type Kind string

const (
	KindThread           Kind = "thread"
	KindTurn             Kind = "turn"
	KindItem             Kind = "item"
	KindPlanUsageRow     Kind = "plan_usage_row"
	KindPlanUsageOutcome Kind = "plan_usage_outcome"
	KindTokenUsage       Kind = "token_usage"
	KindTurnCompleted    Kind = "turn_completed"
)

// Record is one line of a mirror file.
type Record struct {
	Kind      Kind           `json:"kind"`
	FetchedAt time.Time      `json:"fetched_at"`
	Data      jsontext.Value `json:"data"`
}

// AppendResult reports what Append found and wrote.
type AppendResult struct {
	// Offset is the end of the last complete frame after the append.
	Offset int64
	// Recovered holds records from complete frames found after the stored
	// offset: frames written by an earlier append whose state was never
	// saved. With a stored offset of 0 it holds every earlier record.
	Recovered []Record
	// Truncated reports that a torn final frame was removed.
	Truncated bool
}

// Append verifies the frames after storedOffset, truncates a torn tail,
// and writes records as one frame. A storedOffset of 0, past the end of the
// file, or not on a frame boundary scans the whole file. Callers serialize
// appends to one file.
func Append(path string, storedOffset int64, records []Record) (result AppendResult, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return AppendResult{}, fmt.Errorf("create mirror directory: %w", err)
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return AppendResult{}, fmt.Errorf("open mirror file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close mirror file: %w", closeErr)
		}
	}()

	from, layout, err := inspect(f, storedOffset)
	if err != nil {
		return AppendResult{}, err
	}
	end := layout.End(from)
	result.Recovered, err = decodeRange(context.Background(), f, from, end)
	if err != nil {
		return AppendResult{}, err
	}
	if layout.Torn {
		if err := f.Truncate(end); err != nil {
			return AppendResult{}, fmt.Errorf("truncate torn mirror frame: %w", err)
		}
		result.Truncated = true
	}
	result.Offset = end
	if len(records) == 0 {
		if result.Truncated {
			return result, syncFile(f)
		}
		return result, nil
	}

	frame, err := encodeRecords(records)
	if err != nil {
		return AppendResult{}, err
	}
	if _, err := f.WriteAt(frame, end); err != nil {
		return AppendResult{}, fmt.Errorf("write mirror frame: %w", err)
	}
	if err := syncFile(f); err != nil {
		return AppendResult{}, err
	}
	if created {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return AppendResult{}, err
		}
	}
	result.Offset = end + int64(len(frame))
	return result, nil
}

// ReadAll returns the records in every complete frame. A torn final frame
// contributes nothing.
func ReadAll(ctx context.Context, path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mirror file: %w", err)
	}
	defer f.Close()
	from, layout, err := inspect(f, 0)
	if err != nil {
		return nil, err
	}
	return decodeRange(ctx, f, from, layout.End(from))
}

// inspect inspects from storedOffset when it is a usable frame boundary
// and from the start of the file otherwise. A clean walk from storedOffset
// is trusted. A torn or invalid walk is checked against a walk from the
// start, because an offset inside a frame can look like a torn tail, and
// truncating there would cut a complete frame.
func inspect(f *os.File, storedOffset int64) (int64, zstdframe.Layout, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, zstdframe.Layout{}, fmt.Errorf("stat mirror file: %w", err)
	}
	size := info.Size()
	if storedOffset > 0 && storedOffset <= size {
		layout, err := zstdframe.Inspect(f, size, storedOffset)
		if err == nil && !layout.Torn {
			return storedOffset, layout, nil
		}
		if _, ok := errors.AsType[*zstdframe.FormatError](err); err != nil && !ok {
			return 0, zstdframe.Layout{}, fmt.Errorf("inspect mirror file: %w", err)
		}
	}
	full, err := zstdframe.Inspect(f, size, 0)
	if err != nil {
		return 0, zstdframe.Layout{}, fmt.Errorf("inspect mirror file: %w", err)
	}
	for index, frameEnd := range full.FrameEnds {
		if frameEnd == storedOffset {
			full.FrameEnds = full.FrameEnds[index+1:]
			full.CompleteFrames = len(full.FrameEnds)
			return storedOffset, full, nil
		}
	}
	return 0, full, nil
}

func decodeRange(ctx context.Context, f *os.File, from, end int64) ([]Record, error) {
	if end <= from {
		return nil, nil
	}
	decoder, err := zstdframe.NewDecoder(io.NewSectionReader(f, from, end-from))
	if err != nil {
		return nil, fmt.Errorf("open mirror frames: %w", err)
	}
	defer decoder.Close()
	values := jsontext.NewDecoder(decoder)
	var records []Record
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var record Record
		err := json.UnmarshalDecode(values, &record)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode mirror record %d: %w", len(records), err)
		}
		records = append(records, record)
	}
}

func encodeRecords(records []Record) ([]byte, error) {
	var buf bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode mirror record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return zstdframe.EncodeFrame(buf.Bytes())
}

func syncFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync mirror file: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open mirror directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync mirror directory: %w", err)
	}
	return nil
}
