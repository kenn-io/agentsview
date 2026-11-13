package sessionmaterialize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/capture"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/parser"
)

type codexPosition struct {
	Rollout string `json:"thread_id"`
	Ordinal uint64 `json:"end_ordinal_exclusive"`
	Offset  uint64 `json:"end_byte_offset"`
}

type codexPage struct {
	name, thread, rollout string
	data                  []byte
	mode                  string
	base                  *codexPosition
	decodedSize           uint64
}

// inspectCodexPage inspects decoded metadata while retaining original bytes.
func inspectCodexPage(ctx context.Context, name string, data []byte) (codexPage, error) {
	basename := path.Base(name)
	key := parser.CodexSessionUUIDFromFilename(basename)
	thread := parser.CodexThreadIDFromSessionKey(key)
	rollout := strings.TrimPrefix(key, thread+"_")
	if !capture.ValidUUID(thread) || !capture.ValidUUID(rollout) {
		return codexPage{}, errors.New("invalid Codex rollout filename")
	}
	var reader io.Reader = bytes.NewReader(data)
	if strings.HasSuffix(basename, ".jsonl.zst") {
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(128<<20), zstd.WithDecoderMaxWindow(64<<20))
		if err != nil {
			return codexPage{}, err
		}
		defer decoder.Close()
		reader = decoder
	}
	buffered := bufio.NewReader(ctxio.Reader{Context: ctx, Reader: reader})
	var line []byte
	for {
		part, err := buffered.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > 16<<20 {
			return codexPage{}, errors.New("native Codex metadata exceeds size limit")
		}
		if err == nil || errors.Is(err, io.EOF) {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return codexPage{}, err
		}
	}
	remaining, err := io.Copy(io.Discard, io.LimitReader(buffered, maxSourceBytes-int64(len(line))+1))
	if err != nil {
		return codexPage{}, err
	}
	if remaining+int64(len(line)) > maxSourceBytes {
		return codexPage{}, errors.New("decoded Codex rollout exceeds size limit")
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			ID        string         `json:"id"`
			SessionID string         `json:"session_id"`
			Mode      jsontext.Value `json:"history_mode"`
			Base      *codexPosition `json:"history_base"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil || record.Type != "session_meta" {
		return codexPage{}, errors.New("invalid Codex session metadata")
	}
	mode := "legacy"
	if record.Payload.Mode != nil {
		if err := json.Unmarshal(record.Payload.Mode, &mode); err != nil || (mode != "legacy" && mode != "paginated") {
			return codexPage{}, errors.New("invalid native Codex history mode")
		}
	}
	_, stamp, _ := codexDate(name)
	if (mode == "paginated" || record.Payload.Base != nil || key != thread) && stamp == "" {
		return codexPage{}, errors.New("native Codex lineage requires canonical filename")
	}
	// Early revert pages recorded the rollout ID as id and the thread as
	// session_id; newer producers record the thread in id.
	if record.Payload.ID != thread && (record.Payload.ID != rollout || record.Payload.SessionID != thread) {
		return codexPage{}, errors.New("native Codex rollout identity differs")
	}
	if record.Payload.Base != nil && mode != "paginated" {
		return codexPage{}, errors.New("native Codex lineage requires paginated history")
	}
	if record.Payload.Base != nil && (!capture.ValidUUID(record.Payload.Base.Rollout) || record.Payload.Base.Ordinal == 0 || record.Payload.Base.Ordinal == ^uint64(0)) {
		return codexPage{}, errors.New("invalid Codex history base")
	}
	return codexPage{name: name, thread: record.Payload.ID, rollout: rollout, data: data, mode: mode, base: record.Payload.Base, decodedSize: uint64(len(line)) + uint64(remaining)}, nil
}

// codexLineage validates every ancestor before returning files in publication
// order, with the requested thread's newest page last.
func codexLineage(ctx context.Context, thread string, names []string, archived map[string]bool, read func(context.Context, string, int64) ([]byte, error)) ([]codexPage, error) {
	type choice struct {
		name        string
		rank        int
		ambiguous   bool
		invalidDate bool
	}
	byRollout := make(map[string]choice)
	var newest, newestStamp, newestRollout string
	var newestCanonical bool
	var invalidRequested bool
	for _, name := range names {
		key := parser.CodexSessionUUIDFromFilename(path.Base(name))
		if key == "" {
			continue
		}
		threadID := parser.CodexThreadIDFromSessionKey(key)
		rollout := strings.TrimPrefix(key, threadID+"_")
		date, stamp, dateErr := codexDate(name)
		if dateErr != nil && threadID == thread {
			invalidRequested = true
			continue
		}
		rank := 0
		if archived[name] {
			rank += 2
		}
		if strings.HasSuffix(name, ".zst") {
			rank++
		}
		canonical := stamp != ""
		if !canonical {
			stamp = date
		}
		current := byRollout[rollout]
		if current.name == "" || rank < current.rank {
			current = choice{name: name, rank: rank, invalidDate: dateErr != nil}
		} else if rank == current.rank && name != current.name {
			current.ambiguous = true
		}
		byRollout[rollout] = current
		if dateErr == nil && threadID == thread && (newest == "" || (canonical && !newestCanonical) || (canonical == newestCanonical && (stamp > newestStamp || (stamp == newestStamp && rollout > newestRollout)))) {
			newest, newestStamp, newestRollout, newestCanonical = name, stamp, rollout, canonical
		}
	}
	if newest == "" {
		if invalidRequested {
			return nil, errors.New("invalid Codex rollout date")
		}
		return nil, fmt.Errorf("native Codex transcript is unavailable: %w", errSourceUnavailable)
	}
	seen := make(map[string]bool)
	var pages []codexPage
	var cutoff *codexPosition
	var total int64
	rollout := newestRollout
	for {
		selected := byRollout[rollout]
		if selected.ambiguous {
			return nil, fmt.Errorf("ambiguous Codex rollout %s", rollout)
		}
		if selected.name == "" {
			return nil, fmt.Errorf("missing Codex ancestor %s", rollout)
		}
		if selected.invalidDate {
			return nil, errors.New("invalid Codex rollout date")
		}
		name := selected.name
		data, err := read(ctx, name, maxSourceBytes-total)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		page, err := inspectCodexPage(ctx, name, data)
		if err != nil {
			return nil, err
		}
		if cutoff == nil && page.mode == "paginated" && page.thread != thread {
			return nil, errors.New("native Codex rollout identity differs")
		}
		_, stamp, _ := codexDate(name)
		if cutoff != nil && stamp == "" {
			return nil, errors.New("native Codex lineage requires canonical filename")
		}
		if seen[page.rollout] {
			return nil, errors.New("native Codex lineage cycle")
		}
		seen[page.rollout] = true
		if cutoff != nil && (page.mode != "paginated" || cutoff.Offset > page.decodedSize) {
			return nil, errors.New("native Codex lineage cutoff or source differs")
		}
		pages = append(pages, page)
		if page.base == nil {
			break
		}
		cutoff = page.base
		rollout = cutoff.Rollout
	}
	slices.Reverse(pages)
	return pages, nil
}

// codexDate extracts the producer's date and canonical timestamp.
func codexDate(name string) (string, string, error) {
	base := path.Base(name)
	key := parser.CodexSessionUUIDFromFilename(base)
	if key == "" {
		return "", "", errors.New("invalid Codex rollout date")
	}
	stamp := strings.TrimPrefix(base[:strings.LastIndex(base, "-"+key)], "rollout-")
	for _, layout := range []string{"2006-01-02T15-04-05", "20060102"} {
		if date, err := time.Parse(layout, stamp); err == nil {
			canonical := ""
			if layout == "2006-01-02T15-04-05" {
				canonical = stamp
			}
			return date.Format("2006/01/02"), canonical, nil
		}
	}
	parts := strings.Split(path.Dir(name), "/")
	if len(parts) >= 3 {
		date, err := time.Parse("2006/01/02", strings.Join(parts[len(parts)-3:], "/"))
		if err == nil {
			return date.Format("2006/01/02"), "", nil
		}
	}
	return "", "", errors.New("invalid Codex rollout date")
}
