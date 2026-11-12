package sessionmaterialize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
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
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(128<<20), zstd.WithDecoderMaxWindow(32<<20))
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
			return codexPage{}, errors.New("Codex metadata exceeds size limit")
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
			Mode      string         `json:"history_mode"`
			Base      *codexPosition `json:"history_base"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil || record.Type != "session_meta" {
		return codexPage{}, errors.New("invalid Codex session metadata")
	}
	// Early revert pages recorded the rollout ID as id and the thread as
	// session_id; newer producers record the thread in id.
	if record.Payload.ID != thread && !(record.Payload.ID == rollout && record.Payload.SessionID == thread) {
		return codexPage{}, errors.New("Codex rollout identity differs")
	}
	if record.Payload.Base != nil && record.Payload.Mode != "paginated" {
		return codexPage{}, errors.New("Codex lineage requires paginated history")
	}
	if record.Payload.Base != nil && (!capture.ValidUUID(record.Payload.Base.Rollout) || record.Payload.Base.Ordinal == 0 || record.Payload.Base.Ordinal == ^uint64(0)) {
		return codexPage{}, errors.New("invalid Codex history base")
	}
	return codexPage{name: name, thread: thread, rollout: rollout, data: data, mode: record.Payload.Mode, base: record.Payload.Base, decodedSize: uint64(len(line)) + uint64(remaining)}, nil
}

// codexLineage validates every ancestor before returning files in publication
// order, with the requested thread's newest page last.
func codexLineage(ctx context.Context, thread string, names []string, read func(context.Context, string, int64) ([]byte, error)) ([]codexPage, error) {
	var newest string
	for _, name := range names {
		if _, err := codexDate(name); err != nil {
			continue
		}
		key := parser.CodexSessionUUIDFromFilename(path.Base(name))
		if parser.CodexThreadIDFromSessionKey(key) != thread {
			continue
		}
		base := strings.TrimSuffix(path.Base(name), ".zst")
		if newest != "" && base == strings.TrimSuffix(path.Base(newest), ".zst") && strings.TrimSuffix(name, ".zst") != strings.TrimSuffix(newest, ".zst") {
			return nil, errors.New("ambiguous Codex rollout location")
		}
		date, _ := codexDate(name)
		newestDate, _ := codexDate(newest)
		if newest == "" || date > newestDate || (date == newestDate && (base > strings.TrimSuffix(path.Base(newest), ".zst") || (base == strings.TrimSuffix(path.Base(newest), ".zst") && !strings.HasSuffix(name, ".zst")))) {
			newest = name
		}
	}
	if newest == "" {
		return nil, errors.New("native Codex transcript is unavailable")
	}
	seen := make(map[string]bool)
	var pages []codexPage
	var cutoff *codexPosition
	var total int64
	name := newest
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := read(ctx, name, maxSourceBytes-total)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		page, err := inspectCodexPage(ctx, name, data)
		if err != nil {
			return nil, err
		}
		if seen[page.rollout] {
			return nil, errors.New("Codex lineage cycle")
		}
		seen[page.rollout] = true
		if cutoff != nil && (page.rollout != cutoff.Rollout || page.mode != "paginated" || cutoff.Offset > page.decodedSize) {
			return nil, errors.New("Codex lineage cutoff or source differs")
		}
		pages = append(pages, page)
		if page.base == nil {
			break
		}
		cutoff = page.base
		name = ""
		for _, candidate := range names {
			if _, err := codexDate(candidate); err != nil {
				continue
			}
			key := parser.CodexSessionUUIDFromFilename(path.Base(candidate))
			threadID := parser.CodexThreadIDFromSessionKey(key)
			rollout := strings.TrimPrefix(key, threadID+"_")
			if rollout != cutoff.Rollout {
				continue
			}
			if name != "" && strings.TrimSuffix(name, ".zst") != strings.TrimSuffix(candidate, ".zst") {
				return nil, fmt.Errorf("ambiguous Codex rollout %s", rollout)
			}
			if name == "" || !strings.HasSuffix(candidate, ".zst") {
				name = candidate
			}
		}
		if name == "" {
			return nil, fmt.Errorf("missing Codex ancestor %s", cutoff.Rollout)
		}
	}
	for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
		pages[i], pages[j] = pages[j], pages[i]
	}
	return pages, nil
}

// codexDate extracts the producer's timestamp for its native dated layout.
func codexDate(name string) (string, error) {
	base := path.Base(name)
	key := parser.CodexSessionUUIDFromFilename(base)
	if key == "" || !strings.HasPrefix(base, "rollout-") {
		return "", errors.New("invalid Codex rollout date")
	}
	ext := ".jsonl"
	if strings.HasSuffix(base, ".jsonl.zst") {
		ext += ".zst"
	}
	if !strings.HasSuffix(base, "-"+key+ext) {
		return "", errors.New("invalid Codex rollout date")
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(base, "rollout-"), "-"+key+ext)
	for _, layout := range []string{"2006-01-02T15-04-05", "20060102"} {
		if date, err := time.Parse(layout, stamp); err == nil {
			return date.Format("2006/01/02"), nil
		}
	}
	parts := strings.Split(path.Dir(name), "/")
	if len(parts) >= 3 {
		date, err := time.Parse("2006/01/02", strings.Join(parts[len(parts)-3:], "/"))
		if err == nil {
			return date.Format("2006/01/02"), nil
		}
	}
	return "", errors.New("invalid Codex rollout date")
}
