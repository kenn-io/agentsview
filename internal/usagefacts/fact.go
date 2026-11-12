// Package usagefacts normalizes provider usage rows without pricing them or
// attaching mutable session metadata.
package usagefacts

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPlausibleTokens bounds one request-level token counter. Aggregate session
// totals may legitimately exceed it.
const MaxPlausibleTokens = 2_000_000

// CostStatusRecorded marks a cost that the harness computed from its own price
// table and wrote into its session store, such as OpenCode's per-message cost
// or Pi's usage.cost.total. It is evidence, not a price: harnesses write $0 for
// models they cannot price and use rates AgentsView does not control. See
// ReportedCostIsPrice for when it prices a row.
const CostStatusRecorded = "recorded"

// RecordedCostKey is the token-usage JSON key under which a parser stores a
// message's harness-recorded cost, as whole microdollars. It rides in the
// message's normalized usage object so every backend reads it with the token
// counters; FromMessage reports it with CostStatusRecorded.
const RecordedCostKey = "recorded_cost_microdollars"

// UnknownModel labels usage events that carry a cost but name no model, so
// their tokens and cost stay in totals instead of being dropped. It matches
// the label Cursor account imports already use for a missing model. The
// catalog has no rate for it, so such rows are priced only by their own cost.
const UnknownModel = "<unknown>"

// ReportedCostIsPrice reports whether a usage row's reported cost replaces the
// catalog price. catalogPriced is whether the pricing catalog resolved a rate
// for the row's model. Every status except CostStatusRecorded keeps the
// historical rule that a reported cost wins. A recorded cost prices the row
// only when the catalog cannot and the harness figure is above zero, because a
// recorded $0 usually means the harness had no rate either. Callers handle
// Copilot's authoritative session cost before asking.
func ReportedCostIsPrice(
	status string, reportedMicrodollars int64, catalogPriced bool,
) bool {
	if status != CostStatusRecorded {
		return true
	}
	return !catalogPriced && reportedMicrodollars > 0
}

// MessageInput is the session-independent message shape used for extraction.
type MessageInput struct {
	Ordinal                                        int
	Role, Timestamp, Model, ProviderID, TokenUsage string
	ClaudeMessageID, ClaudeRequestID               string
	SourceUUID                                     string
}

// EventInput is the session-independent usage-event shape used for extraction.
type EventInput struct {
	// AccountBilling counters may aggregate many requests and are not capped.
	AccountBilling                       bool
	MessageOrdinal                       *int
	Source, Timestamp, Model, ProviderID string
	CostStatus, CostSource, DedupKey     string
	InputTokens, OutputTokens            int64
	ReasoningTokens                      int64
	CacheCreationTokens                  int64
	CacheReadTokens                      int64
	ReportedCostMicrodollars             *int64
}

// ParsedTokenUsage is the sanitized token and billable tool-use payload.
// CacheCreation1hTokens is the 1-hour-TTL subset of CacheCreationTokens,
// taken from Anthropic's nested cache_creation breakdown; the flat
// cache_creation_input_tokens counter stays authoritative, so the subset
// never exceeds it.
type ParsedTokenUsage struct {
	// RecordedCostMicrodollars is the message's harness-recorded cost from
	// RecordedCostKey, or nil when the payload has none.
	RecordedCostMicrodollars *int64
	InputTokens              int64
	OutputTokens             int64
	ReasoningTokens          int64
	CacheCreationTokens      int64
	CacheCreation1hTokens    int64
	CacheReadTokens          int64
	WebSearchRequests        int64
}

// Fact is one normalized, unpriced usage or activity row.
type Fact struct {
	Source                   string
	MessageOrdinal           *int
	TimestampMillis          *int64
	TimestampNanos           *int64
	RawTimestamp             string
	UsesSessionStart         bool
	Model                    string
	ProviderID               string
	InputTokens              int64
	OutputTokens             int64
	ReasoningTokens          int64
	CacheCreationTokens      int64
	CacheCreation1hTokens    int64
	CacheReadTokens          int64
	WebSearchRequests        int64
	ReportedCostMicrodollars *int64
	CostStatus               string
	CostSource               string
	RequestScoped            bool
	ClaudeMessageID          string
	ClaudeRequestID          string
	SourceUUID               string
	UsageDedupKey            string
	TokenEligible            bool
	ActivityEligible         bool
}

// FromMessage returns a normalized fact for a usage- or activity-eligible row.
func FromMessage(in MessageInput) (Fact, bool) {
	tokenEligible := in.TokenUsage != "" && in.Model != "" &&
		in.Model != "<synthetic>"
	activityEligible := in.Role == "assistant" && in.Model != "<synthetic>"
	if !tokenEligible && !activityEligible {
		return Fact{}, false
	}
	parsed := ParseTokenUsage(in.TokenUsage)
	ordinal := in.Ordinal
	millis, nanos, raw, fallback := parseTimestamp(in.Timestamp)
	return Fact{
		Source: "message", MessageOrdinal: &ordinal,
		TimestampMillis: millis, TimestampNanos: nanos, RawTimestamp: raw,
		UsesSessionStart: fallback, Model: in.Model, ProviderID: in.ProviderID,
		InputTokens: parsed.InputTokens, OutputTokens: parsed.OutputTokens,
		ReasoningTokens:          parsed.ReasoningTokens,
		CacheCreationTokens:      parsed.CacheCreationTokens,
		CacheCreation1hTokens:    parsed.CacheCreation1hTokens,
		CacheReadTokens:          parsed.CacheReadTokens,
		WebSearchRequests:        parsed.WebSearchRequests,
		RequestScoped:            true,
		ReportedCostMicrodollars: parsed.RecordedCostMicrodollars,
		CostStatus:               recordedCostStatus(parsed.RecordedCostMicrodollars),
		ClaudeMessageID:          in.ClaudeMessageID,
		ClaudeRequestID:          in.ClaudeRequestID,
		SourceUUID:               in.SourceUUID,
		TokenEligible:            tokenEligible, ActivityEligible: activityEligible,
	}, true
}

func recordedCostStatus(cost *int64) string {
	if cost == nil {
		return ""
	}
	return CostStatusRecorded
}

// FromEvent returns a normalized fact for an eligible usage event.
func FromEvent(in EventInput) (Fact, bool) {
	eligible := in.Model != ""
	if !eligible {
		return Fact{}, false
	}
	millis, nanos, raw, fallback := parseTimestamp(in.Timestamp)
	clamp := clampTokens
	if in.Source == "session" || in.AccountBilling {
		clamp = floorTokens
	}
	return Fact{
		Source: in.Source, MessageOrdinal: in.MessageOrdinal,
		TimestampMillis: millis, TimestampNanos: nanos, RawTimestamp: raw,
		UsesSessionStart: fallback, Model: in.Model, ProviderID: in.ProviderID,
		InputTokens:  clamp(in.InputTokens),
		OutputTokens: clamp(in.OutputTokens),
		// Existing usage-event consumers normalize the four request counters
		// but preserve the provider's reasoning field verbatim.
		ReasoningTokens:          in.ReasoningTokens,
		CacheCreationTokens:      clamp(in.CacheCreationTokens),
		CacheReadTokens:          clamp(in.CacheReadTokens),
		ReportedCostMicrodollars: in.ReportedCostMicrodollars,
		CostStatus:               in.CostStatus,
		CostSource:               in.CostSource,
		RequestScoped:            in.MessageOrdinal != nil || SourceIsRequestScoped(in.Source),
		UsageDedupKey:            in.DedupKey,
		TokenEligible:            eligible, ActivityEligible: eligible,
	}, true
}

// EventDedupKey reproduces the archive row identity used to deduplicate
// session usage events against other usage sources such as Cursor imports.
func EventDedupKey(sessionID, source, dedupKey string, eventID int64) string {
	prefix := sessionID + ":" + source + ":"
	if dedupKey != "" {
		return prefix + dedupKey
	}
	return prefix + "id:" + strconv.FormatInt(eventID, 10)
}

// ParseTimestamp preserves malformed values, normalizes valid values to Unix
// milliseconds, and marks blank values for live session-start fallback.
func ParseTimestamp(value string) (millis *int64, raw string, usesSessionStart bool) {
	millis, _, raw, usesSessionStart = parseTimestamp(value)
	return millis, raw, usesSessionStart
}

// ParseTimestampNanos returns the sub-millisecond nanosecond component for a
// valid timestamp. Combined with ParseTimestamp's Unix milliseconds, this is
// an exact ordering key across the full RFC3339 year range.
func ParseTimestampNanos(value string) *int64 {
	_, nanos, _, _ := parseTimestamp(value)
	return nanos
}

func parseTimestamp(
	value string,
) (millis, nanos *int64, raw string, usesSessionStart bool) {
	if value == "" {
		return nil, nil, "", true
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, nil, value, false
	}
	valueMillis := parsed.UnixMilli()
	valueNanos := int64(parsed.Nanosecond() % int(time.Millisecond))
	return &valueMillis, &valueNanos, value, false
}

// ParseTokenUsage extracts and clamps usage counters from the stored provider
// payload. Leading complete fields remain available in a truncated payload.
func ParseTokenUsage(tokenJSON string) ParsedTokenUsage {
	var result ParsedTokenUsage
	i := skipJSONSpace(tokenJSON, 0)
	if i >= len(tokenJSON) || tokenJSON[i] != '{' {
		return result
	}
	i++
	for i < len(tokenJSON) {
		i = skipJSONSpace(tokenJSON, i)
		if i >= len(tokenJSON) || tokenJSON[i] == '}' {
			break
		}
		if tokenJSON[i] == ',' {
			i++
			continue
		}
		if tokenJSON[i] != '"' {
			next, ok := skipJSONValue(tokenJSON, i)
			if !ok || next <= i {
				i++
			} else {
				i = next
			}
			continue
		}
		key, next, ok := parseJSONString(tokenJSON, i)
		if !ok {
			break
		}
		i = skipJSONSpace(tokenJSON, next)
		if i >= len(tokenJSON) || tokenJSON[i] != ':' {
			continue
		}
		i = skipJSONSpace(tokenJSON, i+1)
		if key == "cache_creation" {
			valueNext, valid := skipJSONValue(tokenJSON, i)
			if !valid {
				break
			}
			value, ok := objectInt(
				tokenJSON[i:valueNext], "ephemeral_1h_input_tokens")
			if ok && value > 0 {
				result.CacheCreation1hTokens = clampTokens(value)
			}
			i = valueNext
			continue
		}
		if key == RecordedCostKey {
			value, valueNext, valid := parseTokenInt(tokenJSON, i)
			if valid && value >= 0 {
				result.RecordedCostMicrodollars = &value
			}
			if valueNext <= i {
				i++
			} else {
				i = valueNext
			}
			continue
		}
		if isTokenCounterKey(key) {
			value, valueNext, valid := parseTokenInt(tokenJSON, i)
			if valid {
				value = clampTokens(value)
				switch key {
				case "input_tokens":
					result.InputTokens = value
				case "output_tokens":
					result.OutputTokens = value
				case "reasoning_tokens":
					result.ReasoningTokens = value
				case "cache_creation_input_tokens":
					result.CacheCreationTokens = value
				case "cache_read_input_tokens":
					result.CacheReadTokens = value
				}
			}
			if valueNext <= i {
				i++
			} else {
				i = valueNext
			}
			continue
		}
		valueNext, valid := skipJSONValue(tokenJSON, i)
		if !valid {
			break
		}
		i = valueNext
	}
	if result.CacheCreation1hTokens > result.CacheCreationTokens {
		result.CacheCreation1hTokens = result.CacheCreationTokens
	}
	result.WebSearchRequests = parseWebSearchRequests(tokenJSON)
	return result
}

func clampTokens(value int64) int64 {
	switch {
	case value < 0:
		return 0
	case value > MaxPlausibleTokens:
		return MaxPlausibleTokens
	default:
		return value
	}
}

// ClampPlausibleTokens bounds a request-level token count and floors negatives.
func ClampPlausibleTokens(value int64) int64 {
	return clampTokens(value)
}

func floorTokens(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// CodexCloudRequestSource marks a Codex cloud token increase that equals the
// latest model call, so it is exactly one request and price bands apply.
const CodexCloudRequestSource = "codex-cloud-request"

// SourceIsRequestScoped reports whether a usage source represents one
// provider request even when the provider cannot attach it to a message.
// Posit Assistant sidecar events ("posit-assistant-" + kind, e.g.
// posit-assistant-keepalive, posit-assistant-classifier) each record one
// provider request, so the whole prefix is request-scoped.
func SourceIsRequestScoped(source string) bool {
	return slices.Contains(RequestScopedSources, source) ||
		strings.HasPrefix(source, RequestScopedSourcePrefix)
}

// RequestScopedSources are the usage sources SourceIsRequestScoped accepts
// by name; it also accepts every source starting with
// RequestScopedSourcePrefix. SQL that classifies rows reads both.
var RequestScopedSources = []string{
	"message", "goose-request", "session-store", "usage-record",
	"deepseek-harness", CodexCloudRequestSource,
}

// RequestScopedSourcePrefix names the Posit Assistant sidecar sources.
const RequestScopedSourcePrefix = "posit-assistant-"

func isTokenCounterKey(key string) bool {
	switch key {
	case "input_tokens", "output_tokens", "reasoning_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens":
		return true
	default:
		return false
	}
}

func parseWebSearchRequests(tokenJSON string) int64 {
	const serverToolUseKey = "server_tool_use"
	if !strings.Contains(tokenJSON, serverToolUseKey) {
		return 0
	}
	serverToolUse, ok := objectRawValue(tokenJSON, serverToolUseKey)
	if !ok {
		return 0
	}
	requests, ok := objectInt(serverToolUse, "web_search_requests")
	if !ok || requests < 0 {
		return 0
	}
	return requests
}

func objectRawValue(tokenJSON, want string) (string, bool) {
	i := skipJSONSpace(tokenJSON, 0)
	if i >= len(tokenJSON) || tokenJSON[i] != '{' {
		return "", false
	}
	for i++; i < len(tokenJSON); {
		i = skipJSONSpace(tokenJSON, i)
		if i >= len(tokenJSON) || tokenJSON[i] == '}' {
			return "", false
		}
		if tokenJSON[i] == ',' {
			i++
			continue
		}
		if tokenJSON[i] != '"' {
			return "", false
		}
		key, next, ok := parseJSONString(tokenJSON, i)
		if !ok {
			return "", false
		}
		i = skipJSONSpace(tokenJSON, next)
		if i >= len(tokenJSON) || tokenJSON[i] != ':' {
			return "", false
		}
		i = skipJSONSpace(tokenJSON, i+1)
		valueNext, ok := skipJSONValue(tokenJSON, i)
		if !ok || valueNext <= i {
			return "", false
		}
		if key == want {
			return tokenJSON[i:valueNext], true
		}
		i = valueNext
	}
	return "", false
}

func objectInt(object, want string) (int64, bool) {
	i := skipJSONSpace(object, 0)
	if i >= len(object) || object[i] != '{' {
		return 0, false
	}
	for i++; i < len(object); {
		i = skipJSONSpace(object, i)
		if i >= len(object) || object[i] == '}' {
			return 0, false
		}
		if object[i] == ',' {
			i++
			continue
		}
		if object[i] != '"' {
			return 0, false
		}
		key, next, ok := parseJSONString(object, i)
		if !ok {
			return 0, false
		}
		i = skipJSONSpace(object, next)
		if i >= len(object) || object[i] != ':' {
			return 0, false
		}
		i = skipJSONSpace(object, i+1)
		if key == want {
			value, _, valid := parseTokenInt(object, i)
			return value, valid
		}
		valueNext, valid := skipJSONValue(object, i)
		if !valid || valueNext <= i {
			return 0, false
		}
		i = valueNext
	}
	return 0, false
}

func parseJSONString(input string, i int) (string, int, bool) {
	if i >= len(input) || input[i] != '"' {
		return "", i, false
	}
	plain := true
	for j := i + 1; j < len(input); j++ {
		switch c := input[j]; {
		case c == '\\':
			if j+1 >= len(input) {
				return "", len(input), false
			}
			plain = false
			j++
		case c == '"':
			if plain && utf8.ValidString(input[i+1:j]) {
				return input[i+1 : j], j + 1, true
			}
			var value string
			if err := json.Unmarshal([]byte(input[i:j+1]), &value, jsontext.AllowInvalidUTF8(true)); err != nil {
				return "", j + 1, false
			}
			return value, j + 1, true
		case c < 0x20:
			plain = false
		}
	}
	return "", len(input), false
}

func parseTokenInt(input string, i int) (int64, int, bool) {
	if i >= len(input) {
		return 0, i, false
	}
	if input[i] == '"' {
		value, next, ok := parseJSONString(input, i)
		if !ok {
			return 0, next, false
		}
		parsed, valid := parseTokenIntLiteral(strings.TrimSpace(value))
		return parsed, next, valid
	}
	start := i
	if input[i] == '-' {
		i++
	}
	digitStart := i
	for i < len(input) && input[i] >= '0' && input[i] <= '9' {
		i++
	}
	if i == digitStart {
		next, ok := skipJSONValue(input, start)
		if ok {
			return 0, next, false
		}
		return 0, start, false
	}
	parsed, ok := parseTokenIntLiteral(input[start:i])
	return parsed, i, ok
}

func parseTokenIntLiteral(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return parsed, true
	}
	if errors.Is(err, strconv.ErrRange) {
		if strings.HasPrefix(value, "-") {
			return -1 << 63, true
		}
		return 1<<63 - 1, true
	}
	return 0, false
}

func skipJSONSpace(input string, i int) int {
	for i < len(input) && (input[i] == ' ' || input[i] == '\n' ||
		input[i] == '\r' || input[i] == '\t') {
		i++
	}
	return i
}

func skipJSONValue(input string, i int) (int, bool) {
	i = skipJSONSpace(input, i)
	if i >= len(input) {
		return i, false
	}
	switch input[i] {
	case '"':
		_, next, ok := parseJSONString(input, i)
		return next, ok
	case '{', '[':
		return skipJSONComposite(input, i)
	case 't':
		if strings.HasPrefix(input[i:], "true") {
			return i + len("true"), true
		}
	case 'f':
		if strings.HasPrefix(input[i:], "false") {
			return i + len("false"), true
		}
	case 'n':
		if strings.HasPrefix(input[i:], "null") {
			return i + len("null"), true
		}
	default:
		return skipJSONNumber(input, i)
	}
	return i, false
}

func skipJSONComposite(input string, i int) (int, bool) {
	var stack []byte
	switch input[i] {
	case '{':
		stack = append(stack, '}')
	case '[':
		stack = append(stack, ']')
	default:
		return i, false
	}
	for i++; i < len(input); {
		switch input[i] {
		case '"':
			_, next, ok := parseJSONString(input, i)
			if !ok {
				return next, false
			}
			i = next
		case '{':
			stack = append(stack, '}')
			i++
		case '[':
			stack = append(stack, ']')
			i++
		case '}', ']':
			if len(stack) == 0 || input[i] != stack[len(stack)-1] {
				return i + 1, false
			}
			stack = stack[:len(stack)-1]
			i++
			if len(stack) == 0 {
				return i, true
			}
		default:
			i++
		}
	}
	return len(input), false
}

func skipJSONNumber(input string, i int) (int, bool) {
	start := i
	if input[i] == '-' {
		i++
	}
	digitStart := i
	for i < len(input) && input[i] >= '0' && input[i] <= '9' {
		i++
	}
	if i == digitStart {
		return start, false
	}
	if i < len(input) && input[i] == '.' {
		i++
		fractionStart := i
		for i < len(input) && input[i] >= '0' && input[i] <= '9' {
			i++
		}
		if i == fractionStart {
			return start, false
		}
	}
	if i < len(input) && (input[i] == 'e' || input[i] == 'E') {
		i++
		if i < len(input) && (input[i] == '+' || input[i] == '-') {
			i++
		}
		exponentStart := i
		for i < len(input) && input[i] >= '0' && input[i] <= '9' {
			i++
		}
		if i == exponentStart {
			return start, false
		}
	}
	return i, true
}
