package rawderive

import (
	"bytes"
	"context"
	"encoding/binary"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	maxParityGraphBytes = 32 << 20
	maxParityChildRows  = 100000
	maxParityJSONDepth  = 128
	maxParityJSONNodes  = 100000
)

var errParityLimit = errors.New("parity graph exceeds supported bounds")

type parityEncoder struct {
	buf      []byte
	err      error
	maxBytes int
}

func newParityEncoder(domain string) *parityEncoder {
	encoder := &parityEncoder{}
	encoder.string(domain)
	encoder.rawUint32(ParitySchemaVersion)
	return encoder
}

func newParityRecord() *parityEncoder { return &parityEncoder{} }

func newParityRecordWithLimit(maxBytes int) *parityEncoder {
	return &parityEncoder{maxBytes: maxBytes}
}

func (e *parityEncoder) append(value ...byte) {
	if e.err != nil {
		return
	}
	maxBytes := e.maxBytes
	if maxBytes == 0 {
		maxBytes = maxParityGraphBytes
	}
	if len(value) > maxBytes-len(e.buf) {
		e.err = errParityLimit
		return
	}
	e.buf = append(e.buf, value...)
}

func (e *parityEncoder) rawUint32(value int) {
	if value < 0 || uint64(value) > math.MaxUint32 {
		e.err = errParityLimit
		return
	}
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(value))
	e.append(encoded[:]...)
}

func (e *parityEncoder) scalar(kind byte, value []byte) {
	e.append(kind)
	e.rawUint32(len(value))
	e.append(value...)
}

func (e *parityEncoder) string(value string) {
	if !utf8.ValidString(value) {
		e.err = fmt.Errorf("invalid non-UTF-8 parity string")
		return
	}
	e.scalar('s', []byte(value))
}

func (e *parityEncoder) integer(value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(int64(value)))
	e.scalar('i', encoded[:])
}

func (e *parityEncoder) boolean(value bool) {
	encoded := byte(0)
	if value {
		encoded = 1
	}
	e.scalar('b', []byte{encoded})
}

func (e *parityEncoder) digest(value ParityDigest) { e.scalar('d', value[:]) }

func (e *parityEncoder) nullableString(value *string) {
	e.append('S')
	if value == nil {
		e.append(0)
		e.rawUint32(0)
		return
	}
	if !utf8.ValidString(*value) {
		e.err = fmt.Errorf("invalid non-UTF-8 parity string")
		return
	}
	e.append(1)
	e.rawUint32(len(*value))
	e.append([]byte(*value)...)
}

func (e *parityEncoder) nullableInt(value *int) {
	e.append('I')
	if value == nil {
		e.append(0)
		e.rawUint32(0)
		return
	}
	e.append(1)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(int64(*value)))
	e.rawUint32(len(encoded))
	e.append(encoded[:]...)
}

func (e *parityEncoder) nullableMoney(value *int64) {
	e.append('M')
	if value == nil {
		e.append(0)
		e.rawUint32(0)
		return
	}
	e.append(1)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(*value))
	e.rawUint32(len(encoded))
	e.append(encoded[:]...)
}

func (e *parityEncoder) nullableFloat(value *float64) {
	e.append('F')
	if value == nil {
		e.append(0)
		e.rawUint32(0)
		return
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) {
		e.err = fmt.Errorf("invalid non-finite parity float")
		return
	}
	e.append(1)
	bits := math.Float64bits(*value)
	if *value == 0 {
		bits = 0
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], bits)
	e.rawUint32(len(encoded))
	e.append(encoded[:]...)
}

func (e *parityEncoder) record(value []byte) {
	e.rawUint32(len(value))
	e.append(value...)
}

func (e *parityEncoder) beginRecord() int {
	start := len(e.buf)
	e.rawUint32(0)
	return start
}

func (e *parityEncoder) endRecord(start int) {
	if e.err != nil {
		return
	}
	length := len(e.buf) - start - 4
	if length < 0 || uint64(length) > math.MaxUint32 {
		e.err = errParityLimit
		return
	}
	binary.BigEndian.PutUint32(e.buf[start:start+4], uint32(length))
}

func (e *parityEncoder) records(values [][]byte) {
	e.rawUint32(len(values))
	for _, value := range values {
		e.record(value)
	}
}

func (e *parityEncoder) result() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.buf, nil
}

var parityTimestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05",
}

func normalizeParityTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	for _, layout := range parityTimestampLayouts {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), nil
		}
	}
	return "", fmt.Errorf("invalid parity timestamp")
}

func (e *parityEncoder) timestamp(value string) {
	normalized, err := normalizeParityTimestamp(value)
	if err != nil {
		e.err = err
		return
	}
	e.string(normalized)
}

func (e *parityEncoder) nullableTimestamp(value *string) {
	if value == nil {
		e.nullableString(nil)
		return
	}
	normalized, err := normalizeParityTimestamp(*value)
	if err != nil {
		e.err = err
		return
	}
	e.nullableString(&normalized)
}

type parityJSONNode struct {
	kind   byte
	value  string
	truth  bool
	array  []*parityJSONNode
	object map[string]*parityJSONNode
}

var errDuplicateJSONKey = errors.New("duplicate JSON object key")

type parityJSONLimits struct {
	maxDepth, maxNodes, maxExpandedBytes int
}

var defaultParityJSONLimits = parityJSONLimits{
	maxDepth:         maxParityJSONDepth,
	maxNodes:         maxParityJSONNodes,
	maxExpandedBytes: maxParityGraphBytes,
}

type parityJSONBudget struct {
	limits parityJSONLimits
	nodes  int
}

func canonicalParityJSON(ctx context.Context, raw []byte) ([]byte, bool, error) {
	return canonicalParityJSONWithLimits(ctx, raw, defaultParityJSONLimits)
}

func canonicalParityJSONWithLimits(
	ctx context.Context,
	raw []byte,
	limits parityJSONLimits,
) ([]byte, bool, error) {
	if len(raw) > maxParityGraphBytes {
		return nil, false, errParityLimit
	}
	if !utf8.Valid(raw) {
		return append([]byte(nil), raw...), false, nil
	}
	if err := validateParityJSONUnicode(ctx, raw); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		return append([]byte(nil), raw...), false, nil
	}
	if limits.maxDepth <= 0 || limits.maxNodes <= 0 || limits.maxExpandedBytes <= 0 ||
		limits.maxDepth > maxParityJSONDepth || limits.maxNodes > maxParityJSONNodes ||
		limits.maxExpandedBytes > maxParityGraphBytes {
		return nil, false, errParityLimit
	}
	decoder := stdjson.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	budget := &parityJSONBudget{limits: limits}
	node, err := readParityJSON(ctx, decoder, budget, 0)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, errParityLimit) {
			return nil, false, err
		}
		return append([]byte(nil), raw...), false, nil
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return append([]byte(nil), raw...), false, nil
	}
	encoded := newParityRecordWithLimit(limits.maxExpandedBytes)
	if err = encodeParityJSONNode(ctx, encoded, node); err != nil {
		return nil, false, err
	}
	canonical, err := encoded.result()
	return canonical, err == nil, err
}

func readParityJSON(
	ctx context.Context,
	decoder *stdjson.Decoder,
	budget *parityJSONBudget,
	depth int,
) (*parityJSONNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if budget.nodes >= budget.limits.maxNodes {
		return nil, errParityLimit
	}
	budget.nodes++
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case nil:
		return &parityJSONNode{kind: 'n'}, nil
	case bool:
		return &parityJSONNode{kind: 'b', truth: value}, nil
	case string:
		return &parityJSONNode{kind: 's', value: value}, nil
	case stdjson.Number:
		normalized, err := normalizeParityJSONNumber(string(value))
		if err != nil {
			return nil, err
		}
		return &parityJSONNode{kind: 'd', value: normalized}, nil
	case stdjson.Delim:
		switch value {
		case '[':
			if depth >= budget.limits.maxDepth {
				return nil, errParityLimit
			}
			node := &parityJSONNode{kind: 'a'}
			for decoder.More() {
				child, err := readParityJSON(ctx, decoder, budget, depth+1)
				if err != nil {
					return nil, err
				}
				node.array = append(node.array, child)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end, err := decoder.Token()
			if err != nil || end != stdjson.Delim(']') {
				return nil, fmt.Errorf("invalid JSON array")
			}
			return node, nil
		case '{':
			if depth >= budget.limits.maxDepth {
				return nil, errParityLimit
			}
			node := &parityJSONNode{kind: 'o', object: make(map[string]*parityJSONNode)}
			for decoder.More() {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("invalid JSON object key")
				}
				if _, exists := node.object[key]; exists {
					return nil, errDuplicateJSONKey
				}
				child, err := readParityJSON(ctx, decoder, budget, depth+1)
				if err != nil {
					return nil, err
				}
				node.object[key] = child
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end, err := decoder.Token()
			if err != nil || end != stdjson.Delim('}') {
				return nil, fmt.Errorf("invalid JSON object")
			}
			return node, nil
		}
	}
	return nil, fmt.Errorf("invalid JSON token")
}

func validateParityJSONUnicode(ctx context.Context, raw []byte) error {
	inString := false
	nextContextCheck := 0
	for index := 0; index < len(raw); index++ {
		if index >= nextContextCheck {
			if err := ctx.Err(); err != nil {
				return err
			}
			nextContextCheck = index + 1024
		}
		value := raw[index]
		if !inString {
			if value == '"' {
				inString = true
			}
			continue
		}
		switch value {
		case '"':
			inString = false
		case '\\':
			index++
			if index >= len(raw) {
				return fmt.Errorf("invalid JSON string escape")
			}
			if raw[index] != 'u' {
				continue
			}
			code, ok := parseParityJSONHex(raw, index+1)
			if !ok {
				return fmt.Errorf("invalid JSON Unicode escape")
			}
			index += 4
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
					return fmt.Errorf("unpaired JSON high surrogate")
				}
				low, valid := parseParityJSONHex(raw, index+3)
				if !valid || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("unpaired JSON high surrogate")
				}
				index += 6
			case code >= 0xdc00 && code <= 0xdfff:
				return fmt.Errorf("unpaired JSON low surrogate")
			}
		default:
			if value < 0x20 {
				return fmt.Errorf("unescaped JSON control character")
			}
		}
	}
	if inString {
		return fmt.Errorf("unterminated JSON string")
	}
	return nil
}

func parseParityJSONHex(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, digit := range raw[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func normalizeParityJSONNumber(value string) (string, error) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	mantissa, exponentText := value, "0"
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa, exponentText = value[:index], value[index+1:]
	}
	dot := strings.IndexByte(mantissa, '.')
	fractionDigits := 0
	coefficient := mantissa
	if dot >= 0 {
		fractionDigits = len(mantissa) - dot - 1
		coefficient = mantissa[:dot] + mantissa[dot+1:]
	}
	coefficient = strings.TrimLeft(coefficient, "0")
	if coefficient == "" {
		return "0", nil
	}
	trailing := len(coefficient) - len(strings.TrimRight(coefficient, "0"))
	coefficient = strings.TrimRight(coefficient, "0")
	if len(exponentText) > 4096 {
		return "", errParityLimit
	}
	exponent := new(big.Int)
	if _, ok := exponent.SetString(exponentText, 10); !ok {
		return "", fmt.Errorf("invalid JSON number")
	}
	exponent.Sub(exponent, big.NewInt(int64(fractionDigits)))
	exponent.Add(exponent, big.NewInt(int64(trailing)))
	if negative {
		coefficient = "-" + coefficient
	}
	return coefficient + "e" + exponent.String(), nil
}

func encodeParityJSONNode(
	ctx context.Context,
	encoder *parityEncoder,
	node *parityJSONNode,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoder.append(node.kind)
	switch node.kind {
	case 'n':
		return encoder.err
	case 'b':
		encoder.boolean(node.truth)
	case 's', 'd':
		encoder.string(node.value)
	case 'a':
		encoder.rawUint32(len(node.array))
		for _, child := range node.array {
			start := encoder.beginRecord()
			if err := encodeParityJSONNode(ctx, encoder, child); err != nil {
				return err
			}
			encoder.endRecord(start)
		}
	case 'o':
		keys := make([]string, 0, len(node.object))
		index := 0
		for key := range node.object {
			if index%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			keys = append(keys, key)
			index++
		}
		sort.Strings(keys)
		if err := ctx.Err(); err != nil {
			return err
		}
		encoder.rawUint32(len(keys))
		for _, key := range keys {
			encoder.string(key)
			start := encoder.beginRecord()
			if err := encodeParityJSONNode(ctx, encoder, node.object[key]); err != nil {
				return err
			}
			encoder.endRecord(start)
		}
	}
	return encoder.err
}

func (e *parityEncoder) json(ctx context.Context, raw []byte) {
	canonical, valid, err := canonicalParityJSON(ctx, raw)
	if err != nil {
		e.err = err
		return
	}
	if valid {
		e.append('j')
	} else {
		e.append('r')
	}
	e.rawUint32(len(canonical))
	e.append(canonical...)
}

func cloneParityPrepared(value ingest.PreparedSession) ingest.PreparedSession {
	value.Session.FirstMessage = cloneParityPointer(value.Session.FirstMessage)
	value.Session.DisplayName = cloneParityPointer(value.Session.DisplayName)
	value.Session.SessionName = cloneParityPointer(value.Session.SessionName)
	value.Session.StartedAt = cloneParityPointer(value.Session.StartedAt)
	value.Session.EndedAt = cloneParityPointer(value.Session.EndedAt)
	value.Session.ParentSessionIDs = append([]string(nil), value.Session.ParentSessionIDs...)
	value.Session.ParentSessionID = cloneParityPointer(value.Session.ParentSessionID)
	value.Session.ParserParentSessionID = cloneParityPointer(value.Session.ParserParentSessionID)
	value.Session.SignalsPendingSince = cloneParityPointer(value.Session.SignalsPendingSince)
	value.Session.ContextPressureMax = cloneParityPointer(value.Session.ContextPressureMax)
	value.Session.HealthScore = cloneParityPointer(value.Session.HealthScore)
	value.Session.HealthGrade = cloneParityPointer(value.Session.HealthGrade)
	value.Session.QualitySignals = cloneParityPointer(value.Session.QualitySignals)
	value.Session.DeletedAt = cloneParityPointer(value.Session.DeletedAt)
	value.Session.DeletionCause = cloneParityPointer(value.Session.DeletionCause)
	value.Session.SourceMissingAt = cloneParityPointer(value.Session.SourceMissingAt)
	value.Session.TerminationStatus = cloneParityPointer(value.Session.TerminationStatus)
	value.Session.FilePath = cloneParityPointer(value.Session.FilePath)
	value.Session.FileSize = cloneParityPointer(value.Session.FileSize)
	value.Session.FileMtime = cloneParityPointer(value.Session.FileMtime)
	value.Session.LastEntryUUID = cloneParityPointer(value.Session.LastEntryUUID)
	value.Session.ClaudeLinearParse = cloneParityPointer(value.Session.ClaudeLinearParse)
	value.Session.FileInode = cloneParityPointer(value.Session.FileInode)
	value.Session.FileDevice = cloneParityPointer(value.Session.FileDevice)
	value.Session.FileHash = cloneParityPointer(value.Session.FileHash)
	value.Session.LocalModifiedAt = cloneParityPointer(value.Session.LocalModifiedAt)
	value.Session.TranscriptRevision = cloneParityPointer(value.Session.TranscriptRevision)

	value.Messages = append([]db.Message(nil), value.Messages...)
	for i := range value.Messages {
		message := &value.Messages[i]
		message.TokenUsage = append(message.TokenUsage[:0:0], message.TokenUsage...)
		message.ToolResults = append([]db.ToolResult(nil), message.ToolResults...)
		message.ToolCalls = append([]db.ToolCall(nil), message.ToolCalls...)
		for j := range message.ToolCalls {
			call := &message.ToolCalls[j]
			call.ResultEvents = append([]db.ToolResultEvent(nil), call.ResultEvents...)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.RawContentDigest = append(event.RawContentDigest[:0:0], event.RawContentDigest...)
				event.SummaryParticipates = cloneParityPointer(event.SummaryParticipates)
			}
		}
	}
	value.UsageEvents = append([]db.UsageEvent(nil), value.UsageEvents...)
	for i := range value.UsageEvents {
		value.UsageEvents[i].MessageOrdinal = cloneParityPointer(value.UsageEvents[i].MessageOrdinal)
		value.UsageEvents[i].Cost = cloneParityPointer(value.UsageEvents[i].Cost)
	}
	value.Signals.FullState = cloneParitySignalState(value.Signals.FullState)
	value.Signals.SignalsPendingSince = cloneParityPointer(value.Signals.SignalsPendingSince)
	value.Signals.ContextPressureMax = cloneParityPointer(value.Signals.ContextPressureMax)
	value.Signals.HealthScore = cloneParityPointer(value.Signals.HealthScore)
	value.Signals.HealthGrade = cloneParityPointer(value.Signals.HealthGrade)
	value.Findings = append([]db.SecretFinding(nil), value.Findings...)
	for i := range value.Findings {
		value.Findings[i].CallIndex = cloneParityPointer(value.Findings[i].CallIndex)
		value.Findings[i].EventIndex = cloneParityPointer(value.Findings[i].EventIndex)
	}
	return value
}

func cloneParityPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneParitySignalState(value *db.SessionSignalState) *db.SessionSignalState {
	if value == nil {
		return nil
	}
	copy := *value
	copy.State = append([]byte(nil), value.State...)
	return &copy
}

// PrepareParityCandidate runs the shared ingest pipeline for one replayed result.
func PrepareParityCandidate(
	ctx context.Context,
	parsed parser.ParseResult,
	prior *ingest.PreparedSession,
	options ingest.ContentOptions,
	observedAt time.Time,
) (ingest.PreparedSession, error) {
	candidate, err := ingest.PrepareCandidate(ctx, parsed, options)
	if err != nil {
		return ingest.PreparedSession{}, err
	}
	var history *ingest.PriorSession
	if prior != nil {
		clonedPrior := cloneParityPrepared(*prior)
		history = &ingest.PriorSession{Session: clonedPrior.Session, Messages: clonedPrior.Messages}
	}
	decision, err := ingest.ReconcileProviderHistory(ctx, candidate, history, options)
	if err != nil {
		return ingest.PreparedSession{}, err
	}
	var prepared ingest.PreparedSession
	if decision.Action == ingest.HistoryPreserve && prior != nil {
		prepared = cloneParityPrepared(*prior)
	} else {
		prepared, err = ingest.Finalize(ctx, decision.Candidate, options)
		if err != nil {
			return ingest.PreparedSession{}, err
		}
	}
	prepared.Signals = ingest.RefreshSignalRecencyAt(
		prepared.Session, prepared.Messages, prepared.Signals, observedAt,
	)
	ingest.ApplySignalFields(&prepared.Session, prepared.Signals)
	prepared.Session.DataVersion = db.CurrentDataVersion()
	return prepared, ctx.Err()
}
