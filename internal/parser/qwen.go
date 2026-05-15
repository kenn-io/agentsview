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
			content := qwenJoinedParts(root.Get("message.parts"), false)
			if strings.TrimSpace(content) == "" {
				continue
			}
			// A new user turn closes any pending tool-call-only run.
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
			if strings.TrimSpace(content) == "" && !hasThinking {
				continue
			}

			pending.absorb(qwenAssistantEntry{
				content:     content,
				thinking:    thinking,
				hasThinking: hasThinking,
				timestamp:   ts,
				model:       root.Get("model").Str,
				usage:       root.Get("usageMetadata"),
			})

			if strings.TrimSpace(content) != "" {
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

// qwenAssistantEntry holds the fields extracted from a single
// `type=assistant` JSONL line, ready to be folded into a turn buffer.
type qwenAssistantEntry struct {
	content     string
	thinking    string
	hasThinking bool
	timestamp   time.Time
	model       string
	usage       gjson.Result
}

// qwenAssistantBuffer accumulates one logical assistant turn across
// one or more JSONL entries. Tool-call-only iterations contribute
// thinking and token usage; the closing text-bearing entry (or EOF /
// next user turn) flushes the buffer to a single ParsedMessage.
type qwenAssistantBuffer struct {
	pending     bool
	content     string
	thinking    []string
	hasThinking bool
	timestamp   time.Time
	model       string

	sumOutput   int
	hasOutput   bool
	peakInput   int
	peakCache   int
	peakContext int
	hasContext  bool
}

func (b *qwenAssistantBuffer) absorb(e qwenAssistantEntry) {
	b.pending = true
	if e.content != "" {
		b.content = e.content
	}
	if e.thinking != "" {
		b.thinking = append(b.thinking, e.thinking)
	}
	if e.hasThinking {
		b.hasThinking = true
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

	input := int(inputField.Int())
	output := int(outputField.Int())
	cacheRead := int(cacheReadField.Int())

	if outputField.Exists() {
		b.hasOutput = true
		b.sumOutput += output
	}
	if inputField.Exists() || cacheReadField.Exists() {
		b.hasContext = true
		if ctx := input + cacheRead; ctx >= b.peakContext {
			b.peakContext = ctx
			b.peakInput = input
			b.peakCache = cacheRead
		}
	}
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
		ContentLength: len(b.content),
		Model:         b.model,
	}

	if b.hasOutput || b.hasContext {
		normalized := map[string]int{
			"input_tokens":            b.peakInput,
			"output_tokens":           b.sumOutput,
			"cache_read_input_tokens": b.peakCache,
		}
		if j, err := json.Marshal(normalized); err == nil {
			msg.TokenUsage = j
		}
		msg.OutputTokens = b.sumOutput
		msg.HasOutputTokens = b.hasOutput
		msg.ContextTokens = b.peakContext
		msg.HasContextTokens = b.hasContext
	}

	*b = qwenAssistantBuffer{}
	return msg, true
}
