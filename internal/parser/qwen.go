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
	)

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

			content := qwenJoinedParts(root.Get("message.parts"), false)
			thinking := qwenJoinedParts(root.Get("message.parts"), true)
			hasThinking := qwenHasThought(root.Get("message.parts"))
			if strings.TrimSpace(content) == "" && !hasThinking {
				continue
			}

			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				ThinkingText:  thinking,
				Timestamp:     ts,
				HasThinking:   hasThinking,
				ContentLength: len(content),
				Model:         root.Get("model").Str,
			}
			applyQwenTokenUsage(&msg, root.Get("usageMetadata"))
			messages = append(messages, msg)
			ordinal++
		}
	}

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

func applyQwenTokenUsage(msg *ParsedMessage, usage gjson.Result) {
	if !usage.Exists() {
		return
	}

	inputField := usage.Get("promptTokenCount")
	outputField := usage.Get("candidatesTokenCount")
	cacheReadField := usage.Get("cachedContentTokenCount")
	if !inputField.Exists() && !outputField.Exists() && !cacheReadField.Exists() {
		return
	}

	input := int(inputField.Int())
	output := int(outputField.Int())
	cacheRead := int(cacheReadField.Int())

	normalized := map[string]int{
		"input_tokens":            input,
		"output_tokens":           output,
		"cache_read_input_tokens": cacheRead,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}

	msg.TokenUsage = j
	msg.OutputTokens = output
	msg.HasOutputTokens = outputField.Exists()
	msg.ContextTokens = input + cacheRead
	msg.HasContextTokens = inputField.Exists() || cacheReadField.Exists()
}
