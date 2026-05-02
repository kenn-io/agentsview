package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const forgeDBFilename = ".forge.db"

// ForgeSession bundles a parsed session with its messages.
type ForgeSession struct {
	Session  ParsedSession
	Messages []ParsedMessage
}

// ForgeSessionMeta is lightweight metadata for a session,
// used to detect changes without parsing messages.
type ForgeSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

// FindForgeDBPath returns the Forge SQLite database path when present.
func FindForgeDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, forgeDBFilename)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// ListForgeSessionMeta returns lightweight metadata for all
// conversations without parsing message bodies. Used by the sync
// engine to detect which sessions have changed.
func ListForgeSessionMeta(dbPath string) ([]ForgeSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openForgeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT conversation_id,
		       COALESCE(updated_at, created_at)
		FROM conversations
		WHERE context IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("listing forge conversations: %w", err)
	}
	defer rows.Close()

	var metas []ForgeSessionMeta
	for rows.Next() {
		var id string
		var updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning forge session meta: %w", err)
		}
		metas = append(metas, ForgeSessionMeta{
			SessionID:   id,
			VirtualPath: dbPath + "#" + id,
			FileMtime:   parseForgeTimestamp(updatedAt).UnixNano(),
		})
	}
	return metas, rows.Err()
}

// ParseForgeDB opens the Forge SQLite database read-only and returns
// all conversations with parsed messages.
func ParseForgeDB(dbPath, machine string) ([]ForgeSession, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openForgeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	convos, err := loadForgeConversations(db)
	if err != nil {
		return nil, fmt.Errorf("loading forge conversations: %w", err)
	}

	var results []ForgeSession
	for _, c := range convos {
		parsed, msgs, err := buildForgeSession(c, dbPath, machine)
		if err != nil {
			log.Printf("forge conversation %s: %v", c.id, err)
			continue
		}
		if parsed == nil {
			continue
		}
		results = append(results, ForgeSession{Session: *parsed, Messages: msgs})
	}
	return results, nil
}

// ParseForgeSession parses a single conversation by ID from the Forge database.
func ParseForgeSession(dbPath, conversationID, machine string) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("forge db not found: %s", dbPath)
	}

	db, err := openForgeDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	c, err := loadOneForgeConversation(db, conversationID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading forge conversation %s: %w", conversationID, err)
	}
	return buildForgeSession(c, dbPath, machine)
}

func openForgeDB(dbPath string) (*sql.DB, error) {
	dsn := dbPath + "?mode=ro&_journal_mode=WAL&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening forge db %s: %w", dbPath, err)
	}
	return db, nil
}

type forgeConversationRow struct {
	id        string
	title     string
	context   string
	createdAt string
	updatedAt string
	metrics   string
}

func loadForgeConversations(db *sql.DB) ([]forgeConversationRow, error) {
	rows, err := db.Query(`
		SELECT conversation_id,
		       COALESCE(title, ''),
		       COALESCE(context, ''),
		       created_at,
		       COALESCE(updated_at, created_at),
		       COALESCE(metrics, '')
		FROM conversations
		WHERE context IS NOT NULL
		ORDER BY COALESCE(updated_at, created_at)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convos []forgeConversationRow
	for rows.Next() {
		var c forgeConversationRow
		if err := rows.Scan(&c.id, &c.title, &c.context, &c.createdAt, &c.updatedAt, &c.metrics); err != nil {
			return nil, err
		}
		convos = append(convos, c)
	}
	return convos, rows.Err()
}

func loadOneForgeConversation(db *sql.DB, conversationID string) (forgeConversationRow, error) {
	row := db.QueryRow(`
		SELECT conversation_id,
		       COALESCE(title, ''),
		       COALESCE(context, ''),
		       created_at,
		       COALESCE(updated_at, created_at),
		       COALESCE(metrics, '')
		FROM conversations
		WHERE conversation_id = ?
	`, conversationID)

	var c forgeConversationRow
	err := row.Scan(&c.id, &c.title, &c.context, &c.createdAt, &c.updatedAt, &c.metrics)
	return c, err
}

func buildForgeSession(c forgeConversationRow, dbPath, machine string) (*ParsedSession, []ParsedMessage, error) {
	root := gjson.Parse(c.context)
	messagesRoot := root.Get("messages")
	if !messagesRoot.IsArray() {
		return nil, nil, nil
	}

	var (
		messages      []ParsedMessage
		ordinal       int
		realUserCount int
		firstMsg      string
		cwd           string
	)

	messagesRoot.ForEach(func(_, item gjson.Result) bool {
		if cwd == "" {
			cwd = extractForgeCurrentWorkingDirectory(item)
		}

		textMsg := item.Get("message.text")
		if textMsg.Exists() {
			role := strings.ToLower(textMsg.Get("role").Str)
			model := textMsg.Get("model").Str
			ts := parseTimestamp(textMsg.Get("timestamp").Str)
			usageRaw, ctxTokens, outTokens, hasCtx, hasOut := forgeUsageJSON(item.Get("usage"))

			switch role {
			case "system":
				return true
			case "user":
				content := strings.TrimSpace(textMsg.Get("content").Str)
				if content == "" {
					content = strings.TrimSpace(textMsg.Get("raw_content.Text").Str)
				}
				if content == "" {
					return true
				}
				if firstMsg == "" {
					firstMsg = truncate(strings.ReplaceAll(content, "\n", " "), 300)
				}
				messages = append(messages, ParsedMessage{
					Ordinal:            ordinal,
					Role:               RoleUser,
					Content:            content,
					Timestamp:          ts,
					ContentLength:      len(content),
					Model:              model,
					TokenUsage:         usageRaw,
					ContextTokens:      ctxTokens,
					OutputTokens:       outTokens,
					HasContextTokens:   hasCtx,
					HasOutputTokens:    hasOut,
					tokenPresenceKnown: hasCtx || hasOut,
				})
				ordinal++
				realUserCount++
			case "assistant":
				body := strings.TrimSpace(textMsg.Get("content").Str)
				if body == "" {
					body = strings.TrimSpace(textMsg.Get("raw_content.Text").Str)
				}
				thinking := collectForgeReasoning(textMsg.Get("reasoning_details"))
				hasThinking := thinking != ""
				toolCalls := collectForgeToolCalls(textMsg.Get("tool_calls"))
				display := body
				if hasThinking {
					display = "[Thinking]\n" + thinking + "\n[/Thinking]"
					if body != "" {
						display += "\n" + body
					}
				}
				if display == "" && len(toolCalls) == 0 {
					return true
				}
				messages = append(messages, ParsedMessage{
					Ordinal:            ordinal,
					Role:               RoleAssistant,
					Content:            display,
					ThinkingText:       thinking,
					Timestamp:          ts,
					HasThinking:        hasThinking,
					HasToolUse:         len(toolCalls) > 0,
					ContentLength:      len(body) + len(thinking),
					ToolCalls:          toolCalls,
					Model:              model,
					TokenUsage:         usageRaw,
					ContextTokens:      ctxTokens,
					OutputTokens:       outTokens,
					HasContextTokens:   hasCtx,
					HasOutputTokens:    hasOut,
					tokenPresenceKnown: hasCtx || hasOut,
				})
				ordinal++
			}
			return true
		}

		toolMsg := item.Get("message.tool")
		if toolMsg.Exists() {
			callID := toolMsg.Get("call_id").Str
			if callID == "" {
				return true
			}
			content := forgeToolOutputText(toolMsg.Get("output"))
			quoted, _ := json.Marshal(content)
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       "",
				ContentLength: len(content),
				ToolResults: []ParsedToolResult{{
					ToolUseID:     callID,
					ContentLength: len(content),
					ContentRaw:    string(quoted),
				}},
			})
			ordinal++
		}
		return true
	})

	if len(messages) == 0 {
		return nil, nil, nil
	}

	project := ExtractProjectFromCwd(cwd)
	startedAt := parseForgeTimestamp(c.createdAt)
	endedAt := parseForgeTimestamp(c.updatedAt)
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	fileInfo := FileInfo{Path: dbPath + "#" + c.id, Mtime: endedAt.UnixNano()}
	if info, err := os.Stat(dbPath); err == nil {
		fileInfo.Size = info.Size()
		if fileInfo.Mtime == 0 {
			fileInfo.Mtime = info.ModTime().UnixNano()
		}
	}

	sess := &ParsedSession{
		ID:               "forge:" + c.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentForge,
		Cwd:              cwd,
		DisplayName:      c.title,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: realUserCount,
		File:             fileInfo,
	}
	applyForgeMetrics(sess, c.metrics)
	if !sess.aggregateTokenPresenceKnown {
		accumulateMessageTokenUsage(sess, messages)
	}
	return sess, messages, nil
}

func collectForgeReasoning(details gjson.Result) string {
	if !details.IsArray() {
		return ""
	}
	var parts []string
	details.ForEach(func(_, item gjson.Result) bool {
		if text := strings.TrimSpace(item.Get("text").Str); text != "" {
			parts = append(parts, text)
		}
		return true
	})
	return strings.Join(parts, "\n\n")
}

func collectForgeToolCalls(toolCalls gjson.Result) []ParsedToolCall {
	if !toolCalls.IsArray() {
		return nil
	}
	var parsed []ParsedToolCall
	toolCalls.ForEach(func(_, tc gjson.Result) bool {
		name := tc.Get("name").Str
		if name == "" {
			return true
		}
		ptc := ParsedToolCall{
			ToolUseID: tc.Get("call_id").Str,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: tc.Get("arguments").Raw,
		}
		switch name {
		case "skill":
			ptc.SkillName = tc.Get("arguments.name").Str
			if ptc.SkillName == "" {
				ptc.SkillName = tc.Get("arguments.skill").Str
			}
		case "task":
			if rawSubID := tc.Get("arguments.session_id").Str; rawSubID != "" {
				ptc.SubagentSessionID = "forge:" + rawSubID
			}
		}
		parsed = append(parsed, ptc)
		return true
	})
	return parsed
}

func forgeUsageJSON(usage gjson.Result) ([]byte, int, int, bool, bool) {
	prompt := usage.Get("prompt_tokens.actual")
	completion := usage.Get("completion_tokens.actual")
	cached := usage.Get("cached_tokens.actual")

	hasPrompt := prompt.Exists()
	hasCompletion := completion.Exists()
	hasCached := cached.Exists()
	hasContext := hasPrompt || hasCached
	hasOutput := hasCompletion

	if !hasContext && !hasOutput {
		return nil, 0, 0, false, false
	}

	payload := make(map[string]int)
	if hasPrompt {
		payload["input_tokens"] = int(prompt.Int())
	}
	if hasCached {
		payload["cache_read_input_tokens"] = int(cached.Int())
	}
	if hasCompletion {
		payload["output_tokens"] = int(completion.Int())
	}
	raw, _ := json.Marshal(payload)
	return raw, int(prompt.Int()) + int(cached.Int()), int(completion.Int()), hasContext, hasOutput
}

func applyForgeMetrics(sess *ParsedSession, metricsRaw string) {
	metrics := gjson.Parse(metricsRaw)
	if !metrics.Exists() {
		return
	}
	output := metrics.Get("output_tokens")
	input := metrics.Get("input_tokens")
	cached := metrics.Get("cached_input_tokens")
	if output.Exists() {
		sess.TotalOutputTokens = int(output.Int())
		sess.HasTotalOutputTokens = true
	}
	if input.Exists() || cached.Exists() {
		sess.PeakContextTokens = int(input.Int()) + int(cached.Int())
		sess.HasPeakContextTokens = true
	}
	sess.aggregateTokenPresenceKnown = sess.HasTotalOutputTokens || sess.HasPeakContextTokens
}

func forgeToolOutputText(output gjson.Result) string {
	var parts []string
	values := output.Get("values")
	if values.IsArray() {
		values.ForEach(func(_, item gjson.Result) bool {
			if text := item.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
			return true
		})
	}
	if len(parts) > 0 {
		return strings.Join(parts, "")
	}
	if text := output.Get("text").Str; text != "" {
		return text
	}
	if raw := strings.TrimSpace(output.Raw); raw != "" && raw != "null" {
		return raw
	}
	return ""
}

func extractForgeCurrentWorkingDirectory(item gjson.Result) string {
	content := item.Get("message.text.content").Str
	if content == "" {
		content = item.Get("message.text.raw_content.Text").Str
	}
	if content == "" {
		return ""
	}
	startTag := "<current_working_directory>"
	endTag := "</current_working_directory>"
	start := strings.Index(content, startTag)
	if start < 0 {
		return ""
	}
	start += len(startTag)
	end := strings.Index(content[start:], endTag)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(content[start : start+end])
}

func parseForgeTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
