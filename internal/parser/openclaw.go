// ABOUTME: Parses OpenClaw JSONL session files into structured session data.
// ABOUTME: Handles OpenClaw's wrapped message format with toolResult role.
package parser

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// parseSession parses an OpenClaw JSONL session file.
// OpenClaw stores messages in a JSONL format with a session header
// line, message entries, compaction summaries, and metadata events.
func (p *openClawProvider) parseSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	builder := newOpenClawRecordBuilder()

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if err := builder.consume(line, false); err != nil {
			return nil, nil, err
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return builder.finish(
		path, project, machine, info,
		openClawAgentIDFromPath(path),
		OpenClawSessionID(filepath.Base(path)),
	)
}

// openClawRecordBuilder contains the format decoder shared by JSONL files and
// SQLite transcript rows. The source adapters own framing, identity, and
// error policy; this type only turns one OpenClaw record into normalized data.
type openClawRecordBuilder struct {
	messages      []ParsedMessage
	startedAt     time.Time
	endedAt       time.Time
	ordinal       int
	realUserCount int
	firstMsg      string
	sessionID     string
	cwd           string
}

func newOpenClawRecordBuilder() *openClawRecordBuilder {
	return &openClawRecordBuilder{}
}

func (b *openClawRecordBuilder) consume(line string, rejectInvalid bool) error {
	if !gjson.Valid(line) {
		if rejectInvalid {
			return errors.New("invalid OpenClaw event JSON")
		}
		return nil
	}

	entryType := gjson.Get(line, "type").Str
	if ts := parseOpenClawTimestamp(line); !ts.IsZero() {
		if b.startedAt.IsZero() || ts.Before(b.startedAt) {
			b.startedAt = ts
		}
		if ts.After(b.endedAt) {
			b.endedAt = ts
		}
	}

	switch entryType {
	case "session":
		if b.sessionID == "" {
			b.sessionID = gjson.Get(line, "id").Str
		}
		if b.cwd == "" {
			b.cwd = gjson.Get(line, "cwd").Str
		}
		return nil
	case "model_change", "thinking_level_change", "custom", "compaction":
		return nil
	case "message":
	default:
		return nil
	}

	msg := gjson.Get(line, "message")
	if !msg.Exists() {
		return nil
	}
	role := msg.Get("role").Str
	ts := parseTimestamp(msg.Get("timestamp").Str)
	if ts.IsZero() {
		ts = parseTimestamp(gjson.Get(line, "timestamp").Str)
	}

	switch role {
	case "user":
		content := msg.Get("content")
		text, thinkingText, hasThinking, hasToolUse, tcs, trs := ExtractTextContent(context.Background(), content)
		text = strings.TrimSpace(text)
		if text == "" && len(tcs) == 0 && len(trs) == 0 {
			return nil
		}
		if b.firstMsg == "" && text != "" {
			b.firstMsg = truncate(strings.ReplaceAll(
				stripOpenClawDatePrefix(text), "\n", " "), 300)
		}
		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			Content:       text,
			Timestamp:     ts,
			HasThinking:   hasThinking,
			ThinkingText:  thinkingText,
			HasToolUse:    hasToolUse,
			ContentLength: len(text),
			ToolCalls:     tcs,
			ToolResults:   trs,
		})
		b.ordinal++
		b.realUserCount++

	case "assistant":
		content := msg.Get("content")
		text, thinkingText, hasThinking, hasToolUse, tcs, trs := ExtractTextContent(context.Background(), content)
		text = strings.TrimSpace(text)
		if text == "" && len(tcs) == 0 && len(trs) == 0 {
			return nil
		}
		pm := ParsedMessage{
			Ordinal:            b.ordinal,
			Role:               RoleAssistant,
			Content:            text,
			Timestamp:          ts,
			HasThinking:        hasThinking,
			ThinkingText:       thinkingText,
			HasToolUse:         hasToolUse,
			ContentLength:      len(text),
			ToolCalls:          tcs,
			ToolResults:        trs,
			tokenPresenceKnown: true,
		}
		applyOpenClawAssistantUsage(&pm, msg)
		b.messages = append(b.messages, pm)
		b.ordinal++

	case "toolResult":
		toolCallID := msg.Get("toolCallId").Str
		if toolCallID == "" {
			return nil
		}
		content := msg.Get("content")
		resultText := extractToolResultText(content)
		contentLen := len(resultText)
		b.messages = append(b.messages, ParsedMessage{
			Ordinal:       b.ordinal,
			Role:          RoleUser,
			Content:       "",
			Timestamp:     ts,
			ContentLength: contentLen,
			ToolResults: []ParsedToolResult{{
				ToolUseID:     toolCallID,
				ContentLength: contentLen,
				ContentRaw:    content.Raw,
			}},
		})
		b.ordinal++
	}
	return nil
}

func (b *openClawRecordBuilder) finish(
	path, project, machine string, info os.FileInfo,
	agentID, fallbackSessionID string,
) (*ParsedSession, []ParsedMessage, error) {
	if len(b.messages) == 0 {
		return nil, nil, nil
	}
	sessionID := b.sessionID
	if sessionID == "" {
		sessionID = fallbackSessionID
	}
	if agentID == "" {
		agentID = "unknown"
	}
	if project == "" && b.cwd != "" {
		project = ExtractProjectFromCwd(b.cwd)
	}
	if project == "" {
		project = "openclaw"
	}
	sess := &ParsedSession{
		ID:               "openclaw:" + agentID + ":" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenClaw,
		FirstMessage:     b.firstMsg,
		StartedAt:        b.startedAt,
		EndedAt:          b.endedAt,
		MessageCount:     len(b.messages),
		UserMessageCount: b.realUserCount,
		File:             FileInfo{Path: path},
	}
	if info != nil {
		sess.File.Size = info.Size()
		sess.File.Mtime = info.ModTime().UnixNano()
	}
	accumulateMessageTokenUsage(sess, b.messages)
	return sess, b.messages, nil
}

// applyOpenClawAssistantUsage copies the assistant turn's model id
// and per-message token counts into pm so the usage dashboard can
// attribute cost. OpenClaw uses its own usage shape — short field
// names (input, output, cacheRead, cacheWrite) under message.usage,
// with provider/model on message itself. We map the token fields
// onto the agentsview-native input_tokens/output_tokens/
// cache_creation_input_tokens/cache_read_input_tokens keys that
// internal/db/usage.go reads.
//
// Cost (message.usage.cost.total) is intentionally not propagated:
// agentsview re-prices via the model_pricing table (loaded from
// LiteLLM), so trusting the gateway's at-request cost would skew
// totals against the canonical pricing source. The model name is
// the load-bearing field for accurate pricing lookup.
//
// Defensive about missing fields — older sessions may carry a model
// without a usage block, or a usage block without cost; either is
// fine.
func applyOpenClawAssistantUsage(
	pm *ParsedMessage, msg gjson.Result,
) {
	if model := msg.Get("model").Str; model != "" {
		pm.Model = model
	}

	usage := msg.Get("usage")
	if !usage.Exists() {
		return
	}

	var (
		input      int
		output     int
		cacheRead  int
		cacheWrite int

		hasInput      bool
		hasOutput     bool
		hasCacheRead  bool
		hasCacheWrite bool
	)
	if f := usage.Get("input"); f.Exists() {
		input = int(f.Int())
		hasInput = true
	}
	if f := usage.Get("output"); f.Exists() {
		output = int(f.Int())
		hasOutput = true
	}
	if f := usage.Get("cacheRead"); f.Exists() {
		cacheRead = int(f.Int())
		hasCacheRead = true
	}
	if f := usage.Get("cacheWrite"); f.Exists() {
		cacheWrite = int(f.Int())
		hasCacheWrite = true
	}

	if !hasInput && !hasOutput && !hasCacheRead && !hasCacheWrite {
		return
	}

	normalized := map[string]int{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheWrite,
	}
	j, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	pm.TokenUsage = j
	pm.OutputTokens = output
	pm.HasOutputTokens = hasOutput
	pm.ContextTokens = input + cacheRead + cacheWrite
	pm.HasContextTokens = hasInput || hasCacheRead || hasCacheWrite
}

// extractToolResultText extracts plain text from an OpenClaw
// tool result content field (which is an array of blocks).
func extractToolResultText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.Str
	}
	if !content.IsArray() {
		return ""
	}

	var parts []string
	content.ForEach(func(_, block gjson.Result) bool {
		// OpenClaw tool-result content blocks (type "toolResult") carry
		// the rendered text inline under "text", the same field plain
		// "text" blocks use. This matches what DecodeContent reads, so
		// the measured length and the stored/decoded content agree.
		switch block.Get("type").Str {
		case "text", "toolResult":
			if t := block.Get("text").Str; t != "" {
				parts = append(parts, t)
			}
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// IsOpenClawSessionFile reports whether a filename is an OpenClaw
// session file. It matches active files (*.jsonl) and the known
// archive suffixes: .jsonl.deleted.<ts>, .jsonl.reset.<ts>, and
// .jsonl.full.bak.
func IsOpenClawSessionFile(name string) bool {
	if strings.HasSuffix(name, ".jsonl") {
		return true
	}
	idx := strings.Index(name, ".jsonl.")
	if idx <= 0 {
		return false
	}
	suffix := name[idx+len(".jsonl."):]
	return strings.HasPrefix(suffix, "deleted.") ||
		strings.HasPrefix(suffix, "reset.") ||
		suffix == "full.bak"
}

// OpenClawSessionID extracts the session UUID from an OpenClaw
// session filename, stripping any archive suffix.
// "abc.jsonl" → "abc"
// "abc.jsonl.deleted.2026-02-19T08-59-24.951Z" → "abc"
// "abc.jsonl.full.bak" → "abc"
func OpenClawSessionID(name string) string {
	if idx := strings.Index(name, ".jsonl"); idx > 0 {
		return name[:idx]
	}
	return strings.TrimSuffix(name, ".jsonl")
}

// openClawAgentIDFromPath extracts the agent subdirectory name
// from an OpenClaw session file path. The expected layout is
// <agentsDir>/<agentId>/sessions/<sessionId>.jsonl, so the
// agent ID is the grandparent directory of the file.
func openClawAgentIDFromPath(path string) string {
	// path = .../agents/<agentId>/sessions/<file>.jsonl
	sessionsDir := filepath.Dir(path)     // .../agents/<agentId>/sessions
	agentDir := filepath.Dir(sessionsDir) // .../agents/<agentId>
	name := filepath.Base(agentDir)
	if name == "" || name == "." || name == "/" {
		return "unknown"
	}
	return name
}

// stripOpenClawDatePrefix removes the gateway-injected date
// prefix from user messages. OpenClaw prepends timestamps like
// "[Wed 2026-02-18 11:21 GMT+1] " to messages received via
// Telegram/channels. We strip this so session titles are clean.
func stripOpenClawDatePrefix(s string) string {
	if len(s) < 2 || s[0] != '[' {
		return s
	}
	idx := strings.Index(s, "] ")
	if idx < 0 || idx > 40 {
		return s
	}
	return strings.TrimSpace(s[idx+2:])
}

// parseOpenClawTimestamp extracts and parses the timestamp from
// any OpenClaw JSONL entry.
func parseOpenClawTimestamp(line string) time.Time {
	tsStr := gjson.Get(line, "timestamp").Str
	return parseTimestamp(tsStr)
}
