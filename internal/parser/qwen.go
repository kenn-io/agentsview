package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// DiscoverQwenSessions finds Qwen Code chat transcripts under the
// projects root. The directory structure is:
// <projectsDir>/<encoded-project>/chats/<session-id>.jsonl
func DiscoverQwenSessions(projectsDir string) []DiscoveredFile {
	if projectsDir == "" {
		return nil
	}

	projectEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range projectEntries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}

		projectDir := filepath.Join(projectsDir, entry.Name())
		chatsDir := filepath.Join(projectDir, "chats")
		chatEntries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}

		project := GetProjectName(entry.Name())
		for _, chat := range chatEntries {
			if chat.IsDir() || !strings.HasSuffix(chat.Name(), ".jsonl") {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(chatsDir, chat.Name()),
				Project: project,
				Agent:   AgentQwen,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindQwenSourceFile locates a Qwen session file by its raw session
// ID (without the "qwen:" prefix).
func FindQwenSourceFile(projectsDir, rawID string) string {
	if projectsDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	projectEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, entry := range projectEntries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}

		candidate := filepath.Join(
			projectsDir, entry.Name(), "chats", rawID+".jsonl",
		)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// ParseQwenSession parses a Qwen Code JSONL chat transcript.
//
// Qwen emits one `type=assistant` line per model output, including
// every tool-call iteration in a multi-step turn. Each iteration's
// `message.parts` typically contains only a thought + a functionCall,
// with the final iteration carrying the user-facing text. Counting
// every iteration as a distinct message inflates MessageCount well
// beyond what other agents report for the same turn. To keep counts
// comparable across agents, consecutive tool-call-only assistant
// entries are coalesced into the next text-bearing assistant entry,
// aggregating their thinking text and token usage. A trailing run of
// tool-call-only entries with no text follow-up is emitted as a single
// coalesced assistant message so the data isn't lost.
func ParseQwenSession(
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

	var (
		sessionID    string
		cwd          string
		firstMessage string
		startedAt    time.Time
		endedAt      time.Time
		ordinal      int
		userCount    int
		messages     []ParsedMessage
		pending      qwenAssistantBuffer
	)

	flushPending := func() {
		if msg, ok := pending.flush(ordinal); ok {
			messages = append(messages, msg)
			ordinal++
		}
	}

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		root := gjson.Parse(line)
		if sessionID == "" {
			sessionID = root.Get("sessionId").Str
		}
		if cwd == "" {
			cwd = root.Get("cwd").Str
		}

		ts := parseTimestamp(root.Get("timestamp").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if endedAt.IsZero() || ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch root.Get("type").Str {
		case "user":
			if root.Get("message.role").Str != "user" {
				continue
			}
			parts := root.Get("message.parts")
			content := qwenJoinedParts(parts, false)
			toolResults := qwenExtractToolResults(parts)
			if strings.TrimSpace(content) == "" {
				// Tool-result-only user entries are part of the
				// surrounding assistant turn; fold them into the
				// pending buffer so the coalesced turn carries its
				// own ToolResults without breaking message ordering.
				if len(toolResults) > 0 {
					pending.absorbToolResults(toolResults)
				}
				continue
			}
			// A new user text turn closes any pending tool-call-only run.
			flushPending()
			if firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(content, "\n", " "),
					300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
				ToolResults:   toolResults,
			})
			ordinal++
			userCount++

		case "assistant":
			if root.Get("message.role").Str != "model" {
				continue
			}

			parts := root.Get("message.parts")
			content := qwenJoinedParts(parts, false)
			thinking := qwenJoinedParts(parts, true)
			hasThinking := qwenHasThought(parts)
			toolCalls := qwenExtractToolCalls(parts)
			if strings.TrimSpace(content) == "" && !hasThinking &&
				len(toolCalls) == 0 {
				continue
			}

			pending.absorb(qwenAssistantEntry{
				content:     content,
				thinking:    thinking,
				hasThinking: hasThinking,
				toolCalls:   toolCalls,
				timestamp:   ts,
				model:       root.Get("model").Str,
				usage:       root.Get("usageMetadata"),
			})

			// Only flush on a "closing" entry: text with no tool call.
			// An entry that carries both text and a functionCall is an
			// intermediate iteration awaiting its tool result; flushing
			// here would orphan the upcoming functionResponse onto a new
			// pending assistant turn and inflate MessageCount.
			if strings.TrimSpace(content) != "" && len(toolCalls) == 0 {
				flushPending()
			}
		}
	}

	flushPending()

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading qwen %s: %w", path, err)
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if project == "" && cwd != "" {
		project = ExtractProjectFromCwd(cwd)
	}

	sess := &ParsedSession{
		ID:               "qwen:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentQwen,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
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

func qwenJoinedParts(parts gjson.Result, thoughtsOnly bool) string {
	if !parts.IsArray() {
		return ""
	}

	var textParts []string
	parts.ForEach(func(_, part gjson.Result) bool {
		isThought := part.Get("thought").Bool()
		if isThought != thoughtsOnly {
			return true
		}
		text := part.Get("text").Str
		if text != "" {
			textParts = append(textParts, text)
		}
		return true
	})
	return strings.Join(textParts, "\n")
}

func qwenHasThought(parts gjson.Result) bool {
	if !parts.IsArray() {
		return false
	}

	hasThought := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("thought").Bool() {
			hasThought = true
			return false
		}
		return true
	})
	return hasThought
}

// qwenExtractToolCalls pulls functionCall parts out of a Qwen
// `message.parts` array into ParsedToolCall entries. Qwen uses the
// Gemini-style shape: {"functionCall": {"id": ..., "name": ..., "args": {...}}}.
func qwenExtractToolCalls(parts gjson.Result) []ParsedToolCall {
	if !parts.IsArray() {
		return nil
	}
	var calls []ParsedToolCall
	parts.ForEach(func(_, part gjson.Result) bool {
		fc := part.Get("functionCall")
		if !fc.Exists() {
			return true
		}
		name := fc.Get("name").Str
		if name == "" {
			return true
		}
		calls = append(calls, ParsedToolCall{
			ToolUseID: fc.Get("id").Str,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: fc.Get("args").Raw,
		})
		return true
	})
	return calls
}

// qwenExtractToolResults pulls functionResponse parts out of a Qwen
// user `message.parts` array into ParsedToolResult entries. Qwen emits
// tool results as user messages with parts like
// {"functionResponse": {"id": ..., "name": ..., "response": {"output": ...}}}.
// The typical "output" payload is unwrapped so the shared content
// decoders (which expect a string, array, or iFlow-nested object) can
// surface the result text; less common response shapes fall back to
// the raw response object.
func qwenExtractToolResults(parts gjson.Result) []ParsedToolResult {
	if !parts.IsArray() {
		return nil
	}
	var results []ParsedToolResult
	parts.ForEach(func(_, part gjson.Result) bool {
		fr := part.Get("functionResponse")
		if !fr.Exists() {
			return true
		}
		content := fr.Get("response.output")
		if !content.Exists() {
			content = fr.Get("response")
		}
		results = append(results, ParsedToolResult{
			ToolUseID:     fr.Get("id").Str,
			ContentLength: toolResultContentLength(content),
			ContentRaw:    content.Raw,
		})
		return true
	})
	return results
}

// qwenAssistantEntry holds the fields extracted from a single
// `type=assistant` JSONL line, ready to be folded into a turn buffer.
type qwenAssistantEntry struct {
	content     string
	thinking    string
	hasThinking bool
	toolCalls   []ParsedToolCall
	timestamp   time.Time
	model       string
	usage       gjson.Result
}

// qwenAssistantBuffer accumulates one logical assistant turn across
// one or more JSONL entries. Tool-call-only iterations contribute
// thinking, tool calls, and token usage; the closing text-bearing
// entry (or EOF / next user text turn) flushes the buffer to a single
// ParsedMessage. Intermediate tool-result user entries fold their
// ParsedToolResult entries into the same buffer so the coalesced turn
// retains its tool-use evidence.
type qwenAssistantBuffer struct {
	pending     bool
	content     string
	thinking    []string
	hasThinking bool
	toolCalls   []ParsedToolCall
	toolResults []ParsedToolResult
	timestamp   time.Time
	model       string

	sumOutput    int
	sumUncached  int
	sumCacheRead int
	hasOutput    bool
	peakPrompt   int
	hasContext   bool
}

func (b *qwenAssistantBuffer) absorb(e qwenAssistantEntry) {
	b.pending = true
	if e.content != "" {
		if b.content != "" {
			b.content += "\n" + e.content
		} else {
			b.content = e.content
		}
	}
	if e.thinking != "" {
		b.thinking = append(b.thinking, e.thinking)
	}
	if e.hasThinking {
		b.hasThinking = true
	}
	if len(e.toolCalls) > 0 {
		b.toolCalls = append(b.toolCalls, e.toolCalls...)
	}
	if !e.timestamp.IsZero() {
		b.timestamp = e.timestamp
	}
	if e.model != "" {
		b.model = e.model
	}

	if !e.usage.Exists() {
		return
	}
	inputField := e.usage.Get("promptTokenCount")
	outputField := e.usage.Get("candidatesTokenCount")
	cacheReadField := e.usage.Get("cachedContentTokenCount")
	if !inputField.Exists() && !outputField.Exists() &&
		!cacheReadField.Exists() {
		return
	}

	// Qwen reports promptTokenCount as the full input count with the
	// cached portion already included (totalTokenCount = prompt +
	// candidates). For a turn coalesced from multiple model calls,
	// normalized input/cache usage sums the per-call uncached and cache-
	// read counts so we report total tokens billed across iterations.
	// ContextTokens stays at the peak prompt to reflect the largest
	// single-call context window (cached + uncached) without double-
	// counting cached tokens across calls.
	prompt := int(inputField.Int())
	output := int(outputField.Int())
	cacheRead := int(cacheReadField.Int())

	if outputField.Exists() {
		b.hasOutput = true
		b.sumOutput += output
	}
	if inputField.Exists() || cacheReadField.Exists() {
		b.hasContext = true
		b.sumUncached += max(prompt-cacheRead, 0)
		b.sumCacheRead += cacheRead
		if prompt > b.peakPrompt {
			b.peakPrompt = prompt
		}
	}
}

func (b *qwenAssistantBuffer) absorbToolResults(trs []ParsedToolResult) {
	if len(trs) == 0 {
		return
	}
	b.pending = true
	b.toolResults = append(b.toolResults, trs...)
}

func (b *qwenAssistantBuffer) flush(ordinal int) (ParsedMessage, bool) {
	if !b.pending {
		return ParsedMessage{}, false
	}

	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleAssistant,
		Content:       b.content,
		ThinkingText:  strings.Join(b.thinking, "\n"),
		Timestamp:     b.timestamp,
		HasThinking:   b.hasThinking,
		HasToolUse:    len(b.toolCalls) > 0,
		ContentLength: len(b.content),
		Model:         b.model,
		ToolCalls:     b.toolCalls,
		ToolResults:   b.toolResults,
	}

	if b.hasOutput || b.hasContext {
		normalized := map[string]int{
			"input_tokens":            b.sumUncached,
			"output_tokens":           b.sumOutput,
			"cache_read_input_tokens": b.sumCacheRead,
		}
		if j, err := json.Marshal(normalized); err == nil {
			msg.TokenUsage = j
		}
		msg.OutputTokens = b.sumOutput
		msg.HasOutputTokens = b.hasOutput
		msg.ContextTokens = b.peakPrompt
		msg.HasContextTokens = b.hasContext
	}

	*b = qwenAssistantBuffer{}
	return msg, true
}
