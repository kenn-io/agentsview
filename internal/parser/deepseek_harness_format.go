package parser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/klauspost/compress/zstd"
)

const (
	deepSeekHarnessFormatVersion = 0
	deepSeekHarnessMaxSafeInt    = int64(1<<53 - 1)
	deepSeekHarnessMaxWindow     = 8 << 20
	deepSeekHarnessDecoderMemory = 64 << 20
)

type deepSeekHarnessHeader struct {
	ID              string
	CreatedAt       int64
	Cwd             string
	HasCwd          bool
	ParentSession   string
	SeedLength      int64
	HasSeedLength   bool
	Origin          string
	DelegationDepth int64
	AgentPreset     string
}

type deepSeekHarnessEvent struct {
	Type      string
	Seq       int64
	Time      int64
	Data      jsontext.Value
	Ignorable bool
	SurfaceOp jsontext.Value
}

type deepSeekHarnessScan struct {
	Header         deepSeekHarnessHeader
	EventCount     int64
	Truncated      bool
	MalformedLines int
}

type deepSeekHarnessUnsupportedError struct {
	message string
}

type deepSeekHarnessZstdLayout struct {
	CompleteFrames int
	FirstFrameEnd  int64
	Size           int64
	Torn           bool
}

func (err deepSeekHarnessUnsupportedError) Error() string {
	return err.message
}

var deepSeekHarnessKnownEvents = map[string]struct{}{
	"agent-preset/selected": {}, "agent/inbox/spliced": {},
	"approval/asked": {}, "approval/decided": {}, "approval/policy": {},
	"assistant/chunk": {}, "assistant/message": {},
	"command/done": {}, "command/run": {},
	"compaction/end": {}, "compaction/prune": {}, "compaction/start": {},
	"compaction/summary": {}, "feedback/record": {}, "goal/change": {},
	"hook/invoked": {}, "hook/result": {}, "llm/retry": {},
	"llm/retry-started": {}, "permission/preset": {}, "plan/mode": {},
	"request/context": {}, "request/header": {}, "sandbox/mode": {},
	"schedule/change": {}, "session/end-seed": {}, "session/title": {},
	"session/title-llm-request": {}, "step/end": {}, "step/start": {},
	"subagent/descriptor": {}, "todo/write": {},
	"tool-workflow/agent-end": {}, "tool-workflow/agent-start": {},
	"tool-workflow/run-end": {}, "tool-workflow/run-start": {},
	"tool/call": {}, "tool/code-dispatch": {},
	"tool/code-dispatch-start": {}, "tool/result": {},
	"turn/end": {}, "turn/start": {}, "user/message": {},
	"web/deepseek-search-llm-request": {},
}

var deepSeekHarnessSurfaceEvents = map[string]struct{}{
	"user/message": {}, "assistant/message": {}, "tool/result": {},
}

func scanDeepSeekHarnessLog(
	ctx context.Context, path string,
	consume func(deepSeekHarnessHeader, deepSeekHarnessEvent) error,
) (deepSeekHarnessScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return deepSeekHarnessScan{}, err
	}
	defer f.Close()

	snapshot, err := f.Stat()
	if err != nil {
		return deepSeekHarnessScan{}, err
	}
	var source io.Reader = io.NewSectionReader(f, 0, snapshot.Size())
	var decoder *zstd.Decoder
	isZstd := strings.HasSuffix(path, ".zstd")
	var zstdLayout deepSeekHarnessZstdLayout
	if isZstd {
		zstdLayout, err = inspectDeepSeekHarnessZstdFrames(f)
		if err != nil {
			return deepSeekHarnessScan{}, err
		}
		if zstdLayout.CompleteFrames == 0 {
			return deepSeekHarnessScan{}, errors.New("empty or header-less DeepSeek Harness zstd log")
		}
		if err := validateDeepSeekHarnessZstdHeaderFrame(
			ctx, f, zstdLayout.FirstFrameEnd,
		); err != nil {
			return deepSeekHarnessScan{}, err
		}
		decoder, err = newDeepSeekHarnessZstdDecoder(
			io.NewSectionReader(f, 0, zstdLayout.Size),
		)
		if err != nil {
			return deepSeekHarnessScan{}, fmt.Errorf("open zstd stream: %w", err)
		}
		defer decoder.Close()
		source = decoder
	}

	reader := newDeepSeekHarnessLineReader(&contextReader{ctx: ctx, r: source})
	headerLine, complete, lineErr := reader.next()
	if lineErr != nil {
		return deepSeekHarnessScan{}, classifyDeepSeekHarnessPhysicalError(path, lineErr)
	}
	if !complete || len(headerLine) == 0 {
		return deepSeekHarnessScan{}, errors.New("empty or header-less DeepSeek Harness session log")
	}
	header, err := parseDeepSeekHarnessHeader(headerLine)
	if err != nil {
		return deepSeekHarnessScan{}, err
	}
	if err := validateDeepSeekHarnessPathIdentity(path, header); err != nil {
		return deepSeekHarnessScan{}, err
	}

	scan := deepSeekHarnessScan{Header: header, Truncated: zstdLayout.Torn}
	var issue error
	lineNo := 1
	for {
		line, complete, readErr := reader.next()
		if !complete {
			if isZstd && !zstdLayout.Torn {
				if len(line) > 0 {
					return deepSeekHarnessScan{}, errors.New(
						"corrupt DeepSeek Harness zstd log: complete frame contains a torn JSONL record",
					)
				}
				if readErr != nil {
					return deepSeekHarnessScan{}, classifyDeepSeekHarnessPhysicalError(path, readErr)
				}
				break
			}
			if len(line) > 0 {
				scan.Truncated = true
			}
			if readErr != nil {
				if isDeepSeekHarnessTornPhysicalError(readErr) {
					scan.Truncated = true
				} else {
					return deepSeekHarnessScan{}, classifyDeepSeekHarnessPhysicalError(path, readErr)
				}
			}
			break
		}
		lineNo++
		if readErr != nil {
			if issue == nil {
				issue = fmt.Errorf("event row %d: %w", lineNo, readErr)
			}
			continue
		}
		if len(line) == 0 {
			if issue == nil {
				issue = fmt.Errorf("empty event row at line %d", lineNo)
			}
			continue
		}
		decoded, decodeErr := decodeDeepSeekHarnessRecord(line)
		if decodeErr != nil {
			if _, ok := errors.AsType[deepSeekHarnessUnsupportedError](decodeErr); ok {
				return deepSeekHarnessScan{}, decodeErr
			}
			if issue == nil {
				issue = fmt.Errorf("event row %d: %w", lineNo, decodeErr)
			}
			continue
		}
		containsTurnEnd := slices.ContainsFunc(decoded, func(event deepSeekHarnessEvent) bool {
			return event.Type == "turn/end"
		})
		if issue != nil {
			if containsTurnEnd {
				return deepSeekHarnessScan{}, fmt.Errorf(
					"corrupt committed DeepSeek Harness log: %w", issue,
				)
			}
			continue
		}
		for index, event := range decoded {
			expected := scan.EventCount + int64(index)
			if event.Seq != expected {
				issue = fmt.Errorf(
					"seq gap at line %d (expected %d, got %d)",
					lineNo, expected, event.Seq,
				)
				break
			}
		}
		if issue != nil && containsTurnEnd {
			return deepSeekHarnessScan{}, fmt.Errorf(
				"corrupt committed DeepSeek Harness log: %w", issue,
			)
		}
		if issue != nil {
			continue
		}
		accepted := 0
		if consume != nil {
			for _, event := range decoded {
				if err := consume(header, event); err != nil {
					if _, ok := errors.AsType[deepSeekHarnessUnsupportedError](err); ok {
						return deepSeekHarnessScan{}, err
					}
					issue = fmt.Errorf("event row %d: %w", lineNo, err)
					break
				}
				accepted++
			}
		} else {
			accepted = len(decoded)
		}
		scan.EventCount += int64(accepted)
		if issue != nil && containsTurnEnd {
			return deepSeekHarnessScan{}, fmt.Errorf(
				"corrupt committed DeepSeek Harness log: %w", issue,
			)
		}
	}
	if issue != nil {
		scan.Truncated = true
		scan.MalformedLines = 1
	}
	return scan, nil
}

func newDeepSeekHarnessZstdDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(
		r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(deepSeekHarnessDecoderMemory),
		zstd.WithDecoderMaxWindow(deepSeekHarnessMaxWindow),
	)
}

func inspectDeepSeekHarnessZstdFrames(
	f *os.File,
) (deepSeekHarnessZstdLayout, error) {
	info, err := f.Stat()
	if err != nil {
		return deepSeekHarnessZstdLayout{}, err
	}
	size := info.Size()
	layout := deepSeekHarnessZstdLayout{Size: size}
	for offset := int64(0); offset < size; {
		start := offset
		magic, complete, err := deepSeekHarnessReadUintAt(f, size, offset, 4)
		if err != nil {
			return deepSeekHarnessZstdLayout{}, err
		}
		if !complete {
			layout.Torn = true
			break
		}
		if magic != 0xfd2fb528 {
			return deepSeekHarnessZstdLayout{}, fmt.Errorf(
				"corrupt DeepSeek Harness zstd log: invalid frame magic at byte %d", offset,
			)
		}
		offset += 4
		descriptor, complete, err := deepSeekHarnessReadUintAt(f, size, offset, 1)
		if err != nil {
			return deepSeekHarnessZstdLayout{}, err
		}
		if !complete {
			layout.Torn = true
			break
		}
		offset++
		if descriptor&0x18 != 0 {
			return deepSeekHarnessZstdLayout{}, fmt.Errorf(
				"corrupt DeepSeek Harness zstd log: reserved frame-header bit at byte %d",
				offset-1,
			)
		}
		if descriptor&0x04 == 0 {
			return deepSeekHarnessZstdLayout{}, fmt.Errorf(
				"unsupported DeepSeek Harness zstd frame without checksum at byte %d", start,
			)
		}
		contentSizeFlag := descriptor >> 6
		singleSegment := descriptor&0x20 != 0
		dictionaryFlag := descriptor & 0x03
		dictionaryBytes := dictionaryFlag
		if dictionaryFlag == 3 {
			dictionaryBytes = 4
		}
		contentSizeBytes := uint64(0)
		if contentSizeFlag == 0 {
			if singleSegment {
				contentSizeBytes = 1
			}
		} else {
			contentSizeBytes = 1 << contentSizeFlag
		}
		remainingHeader := int64(dictionaryBytes + contentSizeBytes)
		if !singleSegment {
			remainingHeader++
		}
		if remainingHeader > size-offset {
			layout.Torn = true
			break
		}
		offset += remainingHeader

		frameComplete := false
		for {
			blockHeader, complete, err := deepSeekHarnessReadUintAt(f, size, offset, 3)
			if err != nil {
				return deepSeekHarnessZstdLayout{}, err
			}
			if !complete {
				layout.Torn = true
				break
			}
			offset += 3
			lastBlock := blockHeader&1 != 0
			blockType := (blockHeader >> 1) & 0x03
			blockSize := blockHeader >> 3
			if blockType == 0x03 {
				return deepSeekHarnessZstdLayout{}, fmt.Errorf(
					"corrupt DeepSeek Harness zstd log: reserved block type at byte %d", offset-3,
				)
			}
			payloadBytes := int64(blockSize)
			if blockType == 0x01 {
				payloadBytes = 1
			}
			if payloadBytes > size-offset {
				layout.Torn = true
				break
			}
			offset += payloadBytes
			if lastBlock {
				frameComplete = true
				break
			}
		}
		if !frameComplete {
			break
		}
		if 4 > size-offset {
			layout.Torn = true
			break
		}
		offset += 4
		layout.CompleteFrames++
		if layout.CompleteFrames == 1 {
			layout.FirstFrameEnd = offset
		}
	}
	return layout, nil
}

func deepSeekHarnessReadUintAt(
	f *os.File, size, offset int64, width int,
) (uint64, bool, error) {
	if offset < 0 || int64(width) > size-offset {
		return 0, false, nil
	}
	var data [8]byte
	if _, err := f.ReadAt(data[:width], offset); err != nil {
		return 0, false, err
	}
	var value uint64
	for index := range width {
		value |= uint64(data[index]) << (8 * index)
	}
	return value, true, nil
}

func validateDeepSeekHarnessZstdHeaderFrame(
	ctx context.Context, f *os.File, end int64,
) error {
	decoder, err := newDeepSeekHarnessZstdDecoder(io.NewSectionReader(f, 0, end))
	if err != nil {
		return fmt.Errorf("open DeepSeek Harness zstd header frame: %w", err)
	}
	defer decoder.Close()
	reader := newDeepSeekHarnessLineReader(&contextReader{ctx: ctx, r: decoder})
	line, complete, readErr := reader.next()
	if readErr != nil || !complete || len(line) == 0 {
		return errors.New("corrupt DeepSeek Harness zstd log: first frame has no complete header line")
	}
	line, complete, readErr = reader.next()
	if readErr != nil || complete || len(line) != 0 {
		return errors.New("corrupt DeepSeek Harness zstd log: first frame is not exactly one header line")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type deepSeekHarnessLineReader struct {
	reader *bufio.Reader
}

func newDeepSeekHarnessLineReader(r io.Reader) *deepSeekHarnessLineReader {
	return &deepSeekHarnessLineReader{reader: bufio.NewReaderSize(r, 64*1024)}
}

func (r *deepSeekHarnessLineReader) next() ([]byte, bool, error) {
	line := make([]byte, 0, 64*1024)
	tooLong := false
	for {
		part, err := r.reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(part) > maxLineSize {
				tooLong = true
				line = nil
			} else {
				line = append(line, part...)
			}
		}
		switch {
		case err == nil:
			if tooLong {
				return nil, true, errors.New("event row exceeds 64 MiB limit")
			}
			return bytes.TrimSuffix(line, []byte{'\n'}), true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if tooLong {
				return nil, false, errors.New("event row exceeds 64 MiB limit")
			}
			return line, false, nil
		default:
			if tooLong {
				return nil, false, err
			}
			return line, false, err
		}
	}
}

func isDeepSeekHarnessTornPhysicalError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

func classifyDeepSeekHarnessPhysicalError(path string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
}

func parseDeepSeekHarnessHeader(line []byte) (deepSeekHarnessHeader, error) {
	fields, err := decodeDeepSeekHarnessObject(line)
	if err != nil {
		return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header is not valid JSON")
	}
	rawVersion, ok := fields["version"]
	if !ok {
		return deepSeekHarnessHeader{}, errors.New("invalid DeepSeek Harness header: missing version")
	}
	version, err := deepSeekHarnessJSONNumber(rawVersion)
	if err != nil {
		return deepSeekHarnessHeader{}, errors.New("invalid DeepSeek Harness header: version is not a number")
	}
	numericVersion, numericErr := strconv.ParseFloat(version.String(), 64)
	if numericErr != nil || numericVersion != deepSeekHarnessFormatVersion {
		return deepSeekHarnessHeader{}, deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
			"unsupported DeepSeek Harness session format version %s", version,
		)}
	}
	typeName, err := deepSeekHarnessRequiredString(fields, "type")
	if err != nil || typeName != "session" {
		return deepSeekHarnessHeader{}, errors.New("first line is not a DeepSeek Harness session header")
	}
	if _, retired := fields["sandboxMode"]; retired {
		return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header uses retired sandboxMode field")
	}
	if _, retired := fields["approvalPolicy"]; retired {
		return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header uses retired approvalPolicy field")
	}
	id, err := deepSeekHarnessRequiredString(fields, "id")
	if err != nil || id == "" {
		return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header has invalid id")
	}
	createdAt, err := deepSeekHarnessRequiredSafeInt(fields, "createdAt", true)
	if err != nil {
		return deepSeekHarnessHeader{}, fmt.Errorf("invalid DeepSeek Harness header: %w", err)
	}
	depth, err := deepSeekHarnessRequiredSafeInt(fields, "delegationDepth", true)
	if err != nil {
		return deepSeekHarnessHeader{}, fmt.Errorf("invalid DeepSeek Harness header: %w", err)
	}
	header := deepSeekHarnessHeader{
		ID: id, CreatedAt: createdAt, DelegationDepth: depth,
	}
	if raw, ok := fields["cwd"]; ok {
		header.Cwd, err = deepSeekHarnessString(raw)
		if err != nil || !isDeepSeekHarnessAbsolutePath(header.Cwd) {
			return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header has invalid cwd")
		}
		header.HasCwd = true
	}
	if raw, ok := fields["parentSession"]; ok {
		header.ParentSession, err = deepSeekHarnessString(raw)
		if err != nil || header.ParentSession == "" {
			return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header has invalid parentSession")
		}
	}
	if _, ok := fields["seedLength"]; ok {
		header.SeedLength, err = deepSeekHarnessRequiredSafeInt(fields, "seedLength", true)
		if err != nil {
			return deepSeekHarnessHeader{}, fmt.Errorf("invalid DeepSeek Harness header: %w", err)
		}
		header.HasSeedLength = true
	}
	if raw, ok := fields["origin"]; ok {
		header.Origin, err = deepSeekHarnessString(raw)
		if err != nil || header.Origin != "subagent" {
			return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header has invalid origin")
		}
	}
	if raw, ok := fields["agentPreset"]; ok {
		header.AgentPreset, err = deepSeekHarnessString(raw)
		if err != nil {
			return deepSeekHarnessHeader{}, errors.New("DeepSeek Harness header has invalid agentPreset")
		}
	}
	return header, nil
}

func validateDeepSeekHarnessPathIdentity(
	path string, header deepSeekHarnessHeader,
) error {
	encodedID := filepath.Base(filepath.Dir(path))
	if encodeDeepSeekHarnessSegment(header.ID) != encodedID {
		return fmt.Errorf("DeepSeek Harness header id does not match source path")
	}
	projectDir := filepath.Base(filepath.Dir(filepath.Dir(path)))
	wantProject := "_no-cwd"
	if header.HasCwd {
		wantProject = deepSeekHarnessProjectKey(header.Cwd)
	}
	if projectDir != wantProject {
		return fmt.Errorf("DeepSeek Harness header cwd does not match source path")
	}
	return nil
}

func isDeepSeekHarnessAbsolutePath(path string) bool {
	if filepath.IsAbs(path) || strings.HasPrefix(path, `/`) ||
		strings.HasPrefix(path, `\`) {
		return true
	}
	return len(path) >= 3 &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z') &&
		path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

func decodeDeepSeekHarnessRecord(line []byte) ([]deepSeekHarnessEvent, error) {
	fields, err := decodeDeepSeekHarnessObject(line)
	if err != nil {
		return nil, errors.New("row is not valid JSON object")
	}
	typeName, err := deepSeekHarnessRequiredString(fields, "type")
	if err != nil {
		return nil, err
	}
	switch typeName {
	case "text-chunks", "reasoning-chunks", "tool-call-chunks":
		return expandDeepSeekHarnessPackedRow(typeName, fields)
	default:
		event, err := parseDeepSeekHarnessEvent(fields)
		if err != nil {
			return nil, err
		}
		return []deepSeekHarnessEvent{event}, nil
	}
}

func parseDeepSeekHarnessEvent(
	fields map[string]jsontext.Value,
) (deepSeekHarnessEvent, error) {
	for key := range fields {
		switch key {
		case "type", "seq", "time", "data", "ignorable", "surfaceOp", "sourceEventSeqs":
		default:
			return deepSeekHarnessEvent{}, errors.New("event has an invalid envelope field")
		}
	}
	typeName, _ := deepSeekHarnessRequiredString(fields, "type")
	seq, err := deepSeekHarnessRequiredSafeInt(fields, "seq", true)
	if err != nil {
		return deepSeekHarnessEvent{}, err
	}
	timestamp, err := deepSeekHarnessRequiredSafeInt(fields, "time", false)
	if err != nil {
		return deepSeekHarnessEvent{}, err
	}
	data, ok := fields["data"]
	if !ok {
		return deepSeekHarnessEvent{}, errors.New("event has no data field")
	}
	ignorable := false
	if raw, ok := fields["ignorable"]; ok {
		if string(raw) != "true" {
			return deepSeekHarnessEvent{}, errors.New("event ignorable must be true when present")
		}
		ignorable = true
	}
	if _, known := deepSeekHarnessKnownEvents[typeName]; !known && !ignorable {
		return deepSeekHarnessEvent{}, deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
			"unsupported required event type %q", typeName,
		)}
	}
	_, surface := deepSeekHarnessSurfaceEvents[typeName]
	surfaceOp, hasSurfaceOp := fields["surfaceOp"]
	sourceEventSeqs, hasSourceEventSeqs := fields["sourceEventSeqs"]
	if surface && !hasSurfaceOp {
		return deepSeekHarnessEvent{}, fmt.Errorf("surface event %q has no surfaceOp", typeName)
	}
	if !surface && hasSurfaceOp {
		return deepSeekHarnessEvent{}, fmt.Errorf("non-surface event %q has surfaceOp", typeName)
	}
	if !surface && hasSourceEventSeqs {
		return deepSeekHarnessEvent{}, fmt.Errorf(
			"non-surface event %q has sourceEventSeqs", typeName,
		)
	}
	if hasSourceEventSeqs {
		if _, err := deepSeekHarnessSafeIntArray(sourceEventSeqs, true); err != nil {
			return deepSeekHarnessEvent{}, errors.New("event sourceEventSeqs is invalid")
		}
	}
	if hasSurfaceOp {
		if err := validateDeepSeekHarnessSurfaceOp(surfaceOp); err != nil {
			return deepSeekHarnessEvent{}, err
		}
	}
	return deepSeekHarnessEvent{
		Type: typeName, Seq: seq, Time: timestamp, Data: data,
		Ignorable: ignorable, SurfaceOp: surfaceOp,
	}, nil
}

func validateDeepSeekHarnessSurfaceOp(raw jsontext.Value) error {
	var appendOp string
	if json.Unmarshal(raw, &appendOp) == nil {
		if appendOp == "append" {
			return nil
		}
		return deepSeekHarnessUnsupportedError{message: fmt.Sprintf(
			"unsupported surface operation %q", appendOp,
		)}
	}
	fields, err := decodeDeepSeekHarnessObject(raw)
	if err != nil {
		return errors.New("invalid surface operation")
	}
	if !deepSeekHarnessExactKeys(fields, "op", "start", "end") {
		return errors.New("invalid surface replacement")
	}
	op, err := deepSeekHarnessRequiredString(fields, "op")
	if err != nil || op != "replace" {
		return errors.New("invalid surface replacement")
	}
	if _, err := deepSeekHarnessRequiredSafeInt(fields, "start", true); err != nil {
		return err
	}
	if _, err := deepSeekHarnessRequiredSafeInt(fields, "end", true); err != nil {
		return err
	}
	return nil
}

func expandDeepSeekHarnessPackedRow(
	typeName string, fields map[string]jsontext.Value,
) ([]deepSeekHarnessEvent, error) {
	if !deepSeekHarnessExactKeys(fields, "type", "seq0", "time0", "data") {
		return nil, errors.New("packed chunk row has unexpected fields")
	}
	seq0, err := deepSeekHarnessRequiredSafeInt(fields, "seq0", true)
	if err != nil {
		return nil, err
	}
	time0, err := deepSeekHarnessRequiredSafeInt(fields, "time0", false)
	if err != nil {
		return nil, err
	}
	data, err := decodeDeepSeekHarnessObject(fields["data"])
	if err != nil {
		return nil, errors.New("packed chunk data is not an object")
	}
	expectedKeys := []string{"turn", "step", "index", "dt", "texts"}
	payloadKey := "texts"
	if typeName == "tool-call-chunks" {
		expectedKeys = []string{"turn", "step", "index", "id", "dt", "args"}
		if _, ok := data["name"]; ok {
			expectedKeys = append(expectedKeys, "name")
		}
		payloadKey = "args"
	}
	if !deepSeekHarnessExactKeys(data, expectedKeys...) {
		return nil, errors.New("packed chunk data has unexpected fields")
	}
	turn, err := deepSeekHarnessRequiredSafeInt(data, "turn", true)
	if err != nil {
		return nil, err
	}
	step, err := deepSeekHarnessRequiredSafeInt(data, "step", true)
	if err != nil {
		return nil, err
	}
	index, err := deepSeekHarnessRequiredSafeInt(data, "index", true)
	if err != nil {
		return nil, err
	}
	values, err := deepSeekHarnessStringArray(data[payloadKey])
	if err != nil || len(values) == 0 {
		return nil, errors.New("packed chunk payload must be a non-empty string array")
	}
	if int64(len(values)-1) > deepSeekHarnessMaxSafeInt-seq0 {
		return nil, errors.New("packed chunk sequence exceeds safe integer range")
	}
	deltas, err := deepSeekHarnessSafeIntArray(data["dt"], false)
	if err != nil || len(deltas) != len(values)-1 {
		return nil, errors.New("packed chunk dt length does not match payload")
	}
	id, name := "", ""
	if typeName == "tool-call-chunks" {
		id, err = deepSeekHarnessRequiredString(data, "id")
		if err != nil || id == "" {
			return nil, errors.New("packed tool chunk has invalid id")
		}
		if raw, ok := data["name"]; ok {
			name, err = deepSeekHarnessString(raw)
			if err != nil {
				return nil, errors.New("packed tool chunk has invalid name")
			}
		}
	}
	events := make([]deepSeekHarnessEvent, 0, len(values))
	timestamp := time0
	for i, value := range values {
		if i > 0 {
			timestamp += deltas[i-1]
			if timestamp < -deepSeekHarnessMaxSafeInt || timestamp > deepSeekHarnessMaxSafeInt {
				return nil, errors.New("packed chunk time is not a safe integer")
			}
		}
		chunk := map[string]any{"index": index}
		switch typeName {
		case "text-chunks":
			chunk["type"], chunk["text"] = "text-delta", value
		case "reasoning-chunks":
			chunk["type"], chunk["text"] = "reasoning-delta", value
		default:
			chunk["type"] = "tool-call-delta"
			chunk["id"], chunk["argumentsDelta"] = id, value
			if _, ok := data["name"]; ok {
				chunk["name"] = name
			}
		}
		encoded, _ := json.Marshal(map[string]any{
			"turn": turn, "step": step, "chunk": chunk,
		})
		events = append(events, deepSeekHarnessEvent{
			Type: "assistant/chunk", Seq: seq0 + int64(i),
			Time: timestamp, Data: encoded,
		})
	}
	return events, nil
}

func decodeDeepSeekHarnessObject(raw []byte) (map[string]jsontext.Value, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("expected JSON object")
	}
	return fields, nil
}

func deepSeekHarnessExactKeys(fields map[string]jsontext.Value, keys ...string) bool {
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func deepSeekHarnessRequiredString(
	fields map[string]jsontext.Value, key string,
) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("missing %s", key)
	}
	value, err := deepSeekHarnessString(raw)
	if err != nil {
		return "", fmt.Errorf("%s is not a string", key)
	}
	return value, nil
}

func deepSeekHarnessString(raw jsontext.Value) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func deepSeekHarnessRequiredSafeInt(
	fields map[string]jsontext.Value, key string, nonNegative bool,
) (int64, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	number, err := deepSeekHarnessJSONNumber(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not an integer", key)
	}
	numeric, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(numeric, 0) || math.IsNaN(numeric) ||
		numeric != math.Trunc(numeric) ||
		numeric < -float64(deepSeekHarnessMaxSafeInt) ||
		numeric > float64(deepSeekHarnessMaxSafeInt) ||
		(nonNegative && numeric < 0) ||
		(numeric == 0 && strings.HasPrefix(strings.TrimSpace(string(raw)), "-")) {
		return 0, fmt.Errorf("%s is not a safe integer", key)
	}
	return int64(numeric), nil
}

func deepSeekHarnessJSONNumber(raw jsontext.Value) (jsontext.Value, error) {
	if raw.Kind() != jsontext.KindNumber {
		return nil, errors.New("value is not a JSON number")
	}
	return raw, nil
}

func deepSeekHarnessSafeIntArray(raw jsontext.Value, nonNegative bool) ([]int64, error) {
	var values []jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(values))
	for _, rawValue := range values {
		value, err := deepSeekHarnessRequiredSafeInt(
			map[string]jsontext.Value{"value": rawValue}, "value", nonNegative,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

func deepSeekHarnessStringArray(raw jsontext.Value) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func encodeDeepSeekHarnessSegment(raw string) string {
	if raw == "." {
		return "~002E"
	}
	if raw == ".." {
		return "~002E~002E"
	}
	var out strings.Builder
	for _, code := range utf16.Encode([]rune(raw)) {
		ch := byte(code)
		if code < 128 && isDeepSeekHarnessUnescapedSegmentByte(ch) {
			out.WriteByte(ch)
		} else {
			fmt.Fprintf(&out, "~%04X", code)
		}
	}
	return out.String()
}

func isDeepSeekHarnessUnescapedSegmentByte(ch byte) bool {
	return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' ||
		ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-'
}

func decodeDeepSeekHarnessSegment(encoded string) (string, error) {
	if encoded == "" {
		return "", errors.New("empty encoded session id")
	}
	units := make([]uint16, 0, len(encoded))
	for i := 0; i < len(encoded); {
		if encoded[i] != '~' {
			ch := encoded[i]
			if !isDeepSeekHarnessUnescapedSegmentByte(ch) {
				return "", errors.New("invalid unescaped session id byte")
			}
			units = append(units, uint16(ch))
			i++
			continue
		}
		if i+5 > len(encoded) {
			return "", errors.New("incomplete session id escape")
		}
		decoded := make([]byte, 2)
		if _, err := hex.Decode(decoded, []byte(encoded[i+1:i+5])); err != nil {
			return "", errors.New("invalid session id escape")
		}
		units = append(units, uint16(decoded[0])<<8|uint16(decoded[1]))
		i += 5
	}
	value := string(utf16.Decode(units))
	if encodeDeepSeekHarnessSegment(value) != encoded {
		return "", errors.New("non-canonical encoded session id")
	}
	return value, nil
}

func deepSeekHarnessProjectKey(cwd string) string {
	var readable strings.Builder
	separatorRun := false
	for _, code := range utf16.Encode([]rune(cwd)) {
		ch := byte(code)
		if code < 128 && (ch == '/' || ch == '\\' || ch == ':') {
			if !separatorRun {
				readable.WriteByte('-')
			}
			separatorRun = true
			continue
		}
		if code < 128 && ch != '~' &&
			(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' ||
				ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			readable.WriteByte(ch)
		} else {
			fmt.Fprintf(&readable, "~%04X", code)
		}
		separatorRun = false
	}
	slug := strings.TrimLeft(readable.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > 251 {
		slug = slug[:251]
	}
	return "--" + slug + "--"
}
