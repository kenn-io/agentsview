package parser

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func parseAmpSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	if !gjson.ValidBytes(data) {
		return nil, nil, fmt.Errorf("invalid JSON in %s", path)
	}

	root := gjson.ParseBytes(data)

	// The session ID must match the filename so that
	// FindAmpSourceFile can locate the file by ID. Prefer
	// the filename-derived ID; fall back to the JSON id only
	// when the filename doesn't yield a valid ID.
	threadID := ampThreadIDFromPath(path)
	if threadID == "" {
		threadID = root.Get("id").Str
		if !isValidAmpThreadID(threadID) {
			threadID = ""
		}
	}
	if threadID == "" {
		return nil, nil, fmt.Errorf(
			"missing or invalid id in %s", path,
		)
	}

	// Start time from created (epoch ms) when valid and positive.
	var startTime time.Time
	if created := root.Get("created"); created.Type == gjson.Number {
		if ms := created.Int(); ms > 0 {
			startTime = time.UnixMilli(ms)
		}
	}

	// End time from the most recent trace with a non-empty endTime.
	var endTime time.Time
	traces := root.Get("meta.traces")
	if traces.IsArray() {
		traceList := traces.Array()
		for _, v := range slices.Backward(traceList) {
			t := parseTimestamp(v.Get("endTime").Str)
			if !t.IsZero() {
				endTime = t
				break
			}
		}
	}

	// Project from env.initial.trees[0].displayName.
	project := root.Get("env.initial.trees.0.displayName").Str
	if project == "" {
		project = "amp"
	}

	// Title is used as FirstMessage when present.
	title := root.Get("title").Str

	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int
	)

	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		roleStr := msg.Get("role").Str
		if roleStr != "user" && roleStr != "assistant" {
			return true
		}

		role := RoleUser
		if roleStr == "assistant" {
			role = RoleAssistant
		}

		content, thinkingText, hasThinking, hasToolUse, tcs, trs :=
			ExtractTextContent(msg.Get("content"))
		trs = append(trs, extractAmpToolResults(msg.Get("content"))...)
		usage := msg.Get("usage")
		if strings.TrimSpace(content) == "" && len(trs) == 0 &&
			(role != RoleAssistant || !ampUsageHasTokenCounters(usage)) {
			return true
		}

		if role == RoleUser && firstMessage == "" {
			firstMessage = truncate(
				strings.ReplaceAll(content, "\n", " "),
				300,
			)
		}

		parsed := ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			HasThinking:   hasThinking,
			ThinkingText:  thinkingText,
			HasToolUse:    hasToolUse,
			ContentLength: len(content),
			ToolCalls:     tcs,
			ToolResults:   trs,
		}
		applyAmpTokenUsage(&parsed, usage)

		messages = append(messages, parsed)
		ordinal++
		return true
	})

	// Empty threads are non-interactive; skip them.
	if len(messages) == 0 {
		return nil, nil, nil
	}

	// Use title as FirstMessage when available.
	if title != "" {
		firstMessage = title
	}

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "amp:" + threadID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentAmp,
		FirstMessage:     firstMessage,
		StartedAt:        startTime,
		EndedAt:          endTime,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	accumulateMessageTokenUsage(sess, messages)

	return sess, messages, nil
}

func ampUsageHasTokenCounters(usage gjson.Result) bool {
	return usage.Get("inputTokens").Exists() ||
		usage.Get("outputTokens").Exists() ||
		usage.Get("cacheCreationInputTokens").Exists() ||
		usage.Get("cacheReadInputTokens").Exists()
}

// ampFoldsCacheCreationIntoInput reports whether an Amp usage model
// bills cache writes. Amp routes every prompt token into one of its
// three input buckets, but only Anthropic-family models are billed a
// cache-write premium. OpenAI-family threads report inputTokens as 0
// and classify the whole uncached prompt as cacheCreationInputTokens,
// so that portion is uncached input rather than a cache write.
//
// Only gpt-prefixed names are folded. An unrecognized or absent model
// keeps Amp's own bucket labels: without a known provider there is no
// evidence for reinterpreting them, and a model that cannot be priced
// contributes no cost either way.
func ampFoldsCacheCreationIntoInput(model string) bool {
	return strings.HasPrefix(model, "gpt-")
}

// applyAmpTokenUsage normalizes an Amp per-inference usage object into
// the Anthropic-style shape expected by the usage and cost queries.
//
// Amp reports the prompt split across three buckets plus their total:
//
//	totalInputTokens = inputTokens + cacheCreationInputTokens +
//	                   cacheReadInputTokens
//
// Anthropic-family threads already carry Anthropic semantics and pass
// through unchanged. OpenAI-family threads fold cache creation into
// uncached input and emit no cache-creation bucket, mirroring
// applyCodexTokenUsage: the cost formula treats input_tokens as the
// uncached remainder, and OpenAI does not bill cache writes.
//
//	inputTokens (+ cacheCreationInputTokens for gpt-*) → input_tokens
//	outputTokens                                       → output_tokens
//	cacheCreationInputTokens                           → cache_creation_input_tokens
//	cacheReadInputTokens                               → cache_read_input_tokens
func applyAmpTokenUsage(msg *ParsedMessage, usage gjson.Result) {
	if !usage.Exists() {
		return
	}

	msg.Model = usage.Get("model").Str

	// Each inference carries its own timestamp on newer threads. Without
	// it every inference in a long-lived thread falls back to the
	// session start and lands in one daily bucket.
	if ts := parseTimestamp(usage.Get("timestamp").Str); !ts.IsZero() {
		msg.Timestamp = ts
	}

	input := int(usage.Get("inputTokens").Int())
	output := int(usage.Get("outputTokens").Int())
	cacheCreation := int(usage.Get("cacheCreationInputTokens").Int())
	cacheRead := int(usage.Get("cacheReadInputTokens").Int())

	normalized := map[string]int{
		"input_tokens":            input,
		"output_tokens":           output,
		"cache_read_input_tokens": cacheRead,
	}
	if ampFoldsCacheCreationIntoInput(msg.Model) {
		normalized["input_tokens"] = input + cacheCreation
	} else {
		normalized["cache_creation_input_tokens"] = cacheCreation
	}

	j, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	msg.TokenUsage = j

	msg.OutputTokens = output
	msg.HasOutputTokens = usage.Get("outputTokens").Exists()
	// Presence comes from the fields themselves: an inference whose
	// prompt is entirely cached reports inputTokens as 0, which is
	// real usage rather than missing data.
	msg.HasContextTokens = usage.Get("inputTokens").Exists() ||
		usage.Get("cacheCreationInputTokens").Exists() ||
		usage.Get("cacheReadInputTokens").Exists()
	msg.ContextTokens = input + cacheCreation + cacheRead
}

// AmpThreadID extracts the id field from raw Amp thread JSON
// data without fully parsing.
func AmpThreadID(data []byte) string {
	return gjson.GetBytes(data, "id").Str
}

// ampThreadIDFromPath derives a thread ID from the filename
// (e.g. "T-abc123.json" → "T-abc123"). Returns "" if the
// filename doesn't match the expected pattern.
func ampThreadIDFromPath(path string) string {
	name := filepath.Base(path)
	stem := strings.TrimSuffix(name, ".json")
	if stem == name {
		return ""
	}
	if !isValidAmpThreadID(stem) {
		return ""
	}
	return stem
}

func serializeAmpResult(result gjson.Result) string {
	if !result.Exists() || result.Type == gjson.Null {
		return ""
	}

	if result.Type == gjson.String {
		return result.Str
	}

	if result.IsObject() {
		// Priority order is intentional: Bash commonly uses "output",
		// Read uses "content", and Edit uses "diff". If shapes overlap,
		// prefer the most common display fields.
		knownFieldSeen := false
		for _, key := range []string{"output", "content", "diff"} {
			if field := result.Get(key); field.Exists() {
				knownFieldSeen = true
				if s := serializeAmpResult(field); s != "" {
					return s
				}
			}
		}

		success := result.Get("success")
		if success.Exists() {
			if success.Bool() {
				return "success"
			}
			return "failed"
		}

		// If a known display field was present but empty, return empty
		// rather than falling back to noisy raw JSON metadata.
		if knownFieldSeen {
			return ""
		}

		return result.Raw
	}

	if result.IsArray() {
		items := result.Array()
		if len(items) == 0 {
			return ""
		}

		if items[0].Type == gjson.String {
			// Only apply "string list" formatting when all items are strings.
			// Mixed-type arrays should round-trip via Raw rather than silently
			// dropping or mangling non-string elements.
			lines := make([]string, 0, len(items))
			for _, item := range items {
				if item.Type != gjson.String {
					return result.Raw
				}
				lines = append(lines, item.Str)
			}
			return strings.Join(lines, "\n")
		}

		if items[0].IsObject() {
			// Only treat as binary/image if the first element looks like an
			// image block. Generic arrays of objects (search results, file
			// listings, etc.) should round-trip as raw JSON instead.
			if items[0].Get("type").Str == "image" {
				return "[binary content]"
			}
			return result.Raw
		}

		return result.Raw
	}

	return result.Raw
}

func extractAmpToolResults(content gjson.Result) []ParsedToolResult {
	if !content.IsArray() {
		return nil
	}

	var results []ParsedToolResult
	for _, block := range content.Array() {
		if block.Get("type").Str != "tool_result" {
			continue
		}

		if block.Get("tool_use_id").Str != "" {
			// Canonical schema is handled by shared extractor.
			continue
		}

		toolUseID := block.Get("toolUseID").Str
		if toolUseID == "" {
			continue
		}

		var text string
		hasResult := false
		result := block.Get("run.result")
		if result.Exists() && result.Type != gjson.Null {
			text = serializeAmpResult(result)
			hasResult = true
		} else {
			switch block.Get("run.status").Str {
			case "error":
				text = block.Get("run.error.message").Str
				if text == "" {
					text = "[unknown error]"
				}
			case "cancelled":
				text = "[cancelled]"
			}
		}
		// Skip blocks with no result and no error/cancelled status.
		// Preserve blocks where run.result existed but serialized to empty
		// (e.g. empty string, empty array) so the tool call is not left pending.
		if text == "" && !hasResult {
			continue
		}

		quoted, err := json.Marshal(text)
		if err != nil {
			continue
		}

		results = append(results, ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentRaw:    string(quoted),
			ContentLength: len(text),
		})
	}

	return results
}
