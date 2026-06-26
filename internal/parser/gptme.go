package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// parseSession parses a gptme conversation.jsonl file. gptme stores one
// message per line with role/content/timestamp fields. Assistant messages
// carry an optional metadata object with model and usage sub-fields.
func (p *gptmeProvider) parseSession(
	path, machine string,
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
	var (
		messages     []ParsedMessage
		startedAt    time.Time
		endedAt      time.Time
		ordinal      int
		userCount    int
		firstMsg     string
		currentModel string
	)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		role := gjson.Get(line, "role").Str
		ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() {
				startedAt = ts
			}
			endedAt = ts
		}

		// Carry model forward from assistant messages.
		if role == "assistant" {
			if m := gjson.Get(line, "metadata.model").Str; m != "" {
				currentModel = m
			}
		}

		switch role {
		case "system":
			// System messages are tool/context injections; skip them.
			continue

		case "user":
			content := strings.TrimSpace(gjson.Get(line, "content").Str)
			if content == "" {
				continue
			}
			if firstMsg == "" {
				firstMsg = truncate(
					strings.ReplaceAll(content, "\n", " "), 300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++
			userCount++

		case "assistant":
			content := strings.TrimSpace(gjson.Get(line, "content").Str)
			if content == "" {
				continue
			}
			pm := ParsedMessage{
				Ordinal:            ordinal,
				Role:               RoleAssistant,
				Content:            content,
				Timestamp:          ts,
				Model:              currentModel,
				ContentLength:      len(content),
				tokenPresenceKnown: true,
			}
			applyGptmeTokenUsage(&pm, line)
			messages = append(messages, pm)
			ordinal++

		case "tool":
			// gptme emits tool output as standalone transcript lines, but
			// the format does not expose a stable tool-call ID we can use
			// for ToolResults pairing. Keep the output visible by storing
			// it as assistant transcript content instead of hiding it as
			// a synthetic system/user message.
			content := strings.TrimSpace(gjson.Get(line, "content").Str)
			if content == "" {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading gptme %s: %w", path, err)
	}

	if len(messages) == 0 {
		return nil, nil, nil
	}

	sessionName := filepath.Base(filepath.Dir(path))
	sessionID := "gptme:" + sessionName

	project := gptmeProjectFromSessionName(sessionName)
	if project == "" {
		project = "unknown"
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentGptme,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
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

// applyGptmeTokenUsage reads metadata.usage from an assistant message
// and populates the ParsedMessage token accounting fields.
// gptme uses the field names:
//
//	metadata.usage.input_tokens
//	metadata.usage.output_tokens
//	metadata.usage.cache_read_tokens       (→ cache_read_input_tokens)
//	metadata.usage.cache_creation_tokens   (→ cache_creation_input_tokens)
func applyGptmeTokenUsage(pm *ParsedMessage, line string) {
	usage := gjson.Get(line, "metadata.usage")
	if !usage.Exists() {
		return
	}

	fInput := usage.Get("input_tokens")
	fOutput := usage.Get("output_tokens")
	fCacheRead := usage.Get("cache_read_tokens")
	fCacheWrite := usage.Get("cache_creation_tokens")

	if !fInput.Exists() && !fOutput.Exists() &&
		!fCacheRead.Exists() && !fCacheWrite.Exists() {
		return
	}

	input := int(fInput.Int())
	output := int(fOutput.Int())
	cacheRead := int(fCacheRead.Int())
	cacheWrite := int(fCacheWrite.Int())

	normalized := map[string]int{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheWrite,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	pm.TokenUsage = j
	pm.OutputTokens = output
	pm.HasOutputTokens = fOutput.Exists()
	pm.ContextTokens = input + cacheRead + cacheWrite
	pm.HasContextTokens = fInput.Exists() || fCacheRead.Exists() || fCacheWrite.Exists()
}

// gptmeProjectFromSessionName extracts a project name from a gptme
// session directory name. Names follow the pattern
// "YYYY-MM-DD-topic-words" or "YYYY-MM-DD-HHMMSS-topic-words".
// The leading date (and optional time) prefix is stripped.
func gptmeProjectFromSessionName(name string) string {
	parts := strings.SplitN(name, "-", 4)
	if len(parts) < 4 {
		return name
	}
	for _, p := range parts[:3] {
		if !gptmeAllDigits(p) {
			return name
		}
	}
	topic := parts[3]
	// Strip optional HHMMSS- segment from the topic part.
	sub := strings.SplitN(topic, "-", 2)
	if len(sub) == 2 && len(sub[0]) == 6 && gptmeAllDigits(sub[0]) {
		topic = sub[1]
	}
	return topic
}

func gptmeAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
