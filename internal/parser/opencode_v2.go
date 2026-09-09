package parser

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// Current beta databases store metadata in session_v2. Early v2 previews used
// session alongside the v1 tables. Never query v1 children in the current layout.
func openCodeSessionTable(db *sql.DB) (string, error) {
	has, err := openCodeTableHasColumn(db, "session_v2", "id")
	if err != nil {
		return "", err
	}
	if has {
		return "session_v2", nil
	}
	return "session", nil
}

func openCodeSessionTableCached(db *sql.DB, dbPath string) (string, error) {
	state, cacheable := StatSQLiteContainerState(dbPath)
	openCodeSessionSchemaCacheMu.Lock()
	entry, hit := openCodeSessionSchemaCache[dbPath]
	openCodeSessionSchemaCacheMu.Unlock()
	if cacheable && hit && entry.state == state && entry.sessionTable != "" {
		return entry.sessionTable, nil
	}
	table, err := openCodeSessionTable(db)
	if err != nil || !cacheable {
		return table, err
	}
	openCodeSessionSchemaCacheMu.Lock()
	previous := openCodeSessionSchemaCache[dbPath]
	if previous.state != state {
		previous = openCodeSessionSchemaCacheEntry{state: state}
	}
	previous.sessionTable = table
	openCodeSessionSchemaCache[dbPath] = previous
	openCodeSessionSchemaCacheMu.Unlock()
	return table, nil
}

const openCodeV2BaseCountsExpr = `s.time_updated, COALESCE(pr.time_updated, 0), 0, 0, '', ''`

// OpenCode v2 stores complete message states, updated in place; seq is their
// original event order. Early previews select message format per session.
func openCodeV2SupportedCached(db *sql.DB, dbPath string) (bool, error) {
	state, cacheable := StatSQLiteContainerState(dbPath)
	openCodeSessionSchemaCacheMu.Lock()
	entry, hit := openCodeSessionSchemaCache[dbPath]
	openCodeSessionSchemaCacheMu.Unlock()
	if cacheable && hit && entry.state == state && entry.v2Once {
		return entry.hasV2, nil
	}
	has, err := openCodeTableHasColumn(db, "session_message", "data")
	if err != nil || !cacheable {
		return has, err
	}
	openCodeSessionSchemaCacheMu.Lock()
	previous := openCodeSessionSchemaCache[dbPath]
	if previous.state != state {
		previous = openCodeSessionSchemaCacheEntry{state: state}
	}
	previous.hasV2, previous.v2Once = has, true
	openCodeSessionSchemaCache[dbPath] = previous
	openCodeSessionSchemaCacheMu.Unlock()
	return has, nil
}

// Full discovery groups the projection table once. Polls and single-session
// fingerprints use the producer's session_id index and never read other sessions.
// Include seq in the identity because it determines transcript order.
func openCodeV2AggregateSQL(v2, single bool) (columns, joins string) {
	if !v2 {
		return ", 0, 0, ''", ""
	}
	if single {
		return `, COALESCE((SELECT MAX(time_updated) FROM session_message WHERE session_id = s.id), 0),
		(SELECT COUNT(*) FROM session_message WHERE session_id = s.id),
		(SELECT COALESCE(group_concat(id || ':' || seq || ':' || time_updated), '')
		 FROM (SELECT id, seq, time_updated FROM session_message WHERE session_id = s.id ORDER BY id))`, ""
	}
	return ", COALESCE(v.mx, 0), COALESCE(v.n, 0), COALESCE(v.ident, '')", `
	LEFT JOIN (
		SELECT session_id, MAX(time_updated) mx, COUNT(*) n,
		       group_concat(id || ':' || seq || ':' || time_updated) ident
		FROM (SELECT session_id, id, seq, time_updated FROM session_message ORDER BY session_id, id)
		GROUP BY session_id
	) v ON v.session_id = s.id`
}

type openCodeV2Message struct {
	Text    string         `json:"text"`
	Summary string         `json:"summary"`
	Command string         `json:"command"`
	Output  jsontext.Value `json:"output"`
	CallID  string         `json:"callID"`
	ShellID string         `json:"shellID"`
	Exit    int            `json:"exit"`
	Model   struct {
		ID string `json:"id"`
	} `json:"model"`
	Content []openCodeV2Content `json:"content"`
	Time    openCodeV2Time      `json:"time"`
}

type openCodeV2Time struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

type openCodeV2Content struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Time  openCodeV2Time `json:"time"`
	State struct {
		Structured jsontext.Value `json:"structured"`
		Metadata   jsontext.Value `json:"metadata"`
		Status     string         `json:"status"`
		Input      jsontext.Value `json:"input"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
}

func loadOpenCodeV2Messages(db *sql.DB, sessionID, cwd string) ([]ParsedMessage, bool, string, error) {
	rows, err := db.Query(`SELECT id, type, time_created, data FROM session_message WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, false, "", fmt.Errorf("loading opencode v2 messages: %w", err)
	}
	defer rows.Close()
	var parsed []ParsedMessage
	present := false
	hash := sha256.New()
	for rows.Next() {
		var id, kind, raw string
		var created int64
		if err := rows.Scan(&id, &kind, &created, &raw); err != nil {
			return nil, false, "", fmt.Errorf("scanning opencode v2 message: %w", err)
		}
		present = true
		fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%s\x00", id, kind, created, raw)
		var data openCodeV2Message
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, true, "", fmt.Errorf("decoding opencode v2 message %s: %w", id, err)
		}
		pm := ParsedMessage{Ordinal: len(parsed), Timestamp: millisToTime(created), SourceUUID: id}
		switch kind {
		case "user":
			pm.Role, pm.Content = RoleUser, data.Text
		case "assistant":
			pm.Role = RoleAssistant
			var texts []string
			for _, item := range data.Content {
				switch item.Type {
				case "text":
					if item.Text != "" {
						texts = append(texts, item.Text)
					}
				case "reasoning":
					if item.Text != "" {
						pm.HasThinking = true
						texts = append(texts, "[Thinking]\n"+item.Text+"\n[/Thinking]")
					}
				case "tool":
					pm.HasToolUse = true
					pm.ToolCalls = append(pm.ToolCalls, openCodeV2ToolCall(item, cwd))
				}
			}
			pm.Content = strings.Join(texts, "\n")
			applyOpenCodeTokenUsage(&pm, openCodeMessageData{ModelID: data.Model.ID}, raw, nil)
		case "system", "synthetic", "compaction":
			pm.Role, pm.IsSystem, pm.Content = RoleUser, true, data.Text
			if kind == "compaction" {
				pm.Content = data.Summary
			}
		case "shell":
			pm.Role, pm.HasToolUse = RoleUser, true
			pm.Content = data.Command
			input, err := json.Marshal(map[string]string{"command": data.Command})
			if err != nil {
				return nil, true, "", err
			}
			id, name := data.CallID, "bash"
			output := gjson.ParseBytes(data.Output).Str
			if data.ShellID != "" {
				id, name = data.ShellID, "shell"
				output = gjson.GetBytes(data.Output, "output").Str
			}
			call := ParsedToolCall{ToolUseID: id, ToolName: name, Category: NormalizeToolCategory(name), InputJSON: string(input)}
			if data.Time.Completed != 0 {
				status := "completed"
				if data.Exit != 0 {
					status = "errored"
				}
				call.ResultEvents = []ParsedToolResultEvent{{ToolUseID: id, Status: status, Content: output, Timestamp: millisToTime(data.Time.Completed)}}
			}
			pm.ToolCalls = []ParsedToolCall{call}
		default:
			// Agent/model switches are session controls, not transcript messages.
			continue
		}
		if strings.TrimSpace(pm.Content) == "" && !pm.HasToolUse && len(pm.TokenUsage) == 0 {
			continue
		}
		pm.ContentLength = len(pm.Content)
		parsed = append(parsed, pm)
	}
	return parsed, present, fmt.Sprintf("opencode-v2:%x", hash.Sum(nil)), rows.Err()
}

func openCodeV2ToolCall(item openCodeV2Content, cwd string) ParsedToolCall {
	call := ParsedToolCall{
		ToolUseID: item.ID, ToolName: item.Name, Category: NormalizeToolCategory(item.Name),
		InputJSON: string(item.State.Input),
	}
	if item.State.Status == "pending" || item.State.Status == "streaming" {
		call.InputJSON = ""
	}
	if item.Name == "skill" {
		call.SkillName = gjson.Get(call.InputJSON, "name").Str
	} else {
		call.SkillName = inferOpenCodeSkillName(item.Name, call.InputJSON, cwd)
	}
	if item.State.Status == "completed" || item.State.Status == "error" {
		var texts []string
		for _, content := range item.State.Content {
			if content.Type == "text" {
				texts = append(texts, content.Text)
			}
		}
		// The v2 read tool returns text files as structured UTF-8 attachments.
		structured := string(item.State.Structured)
		if item.Name == "read" && gjson.Get(structured, "encoding").Str == "utf8" {
			if content := gjson.Get(structured, "content").Str; content != "" {
				texts = append(texts, content)
			}
		}
		status := "completed"
		shellFailed := (item.Name == "shell" || item.Name == "bash") &&
			(gjson.Get(string(item.State.Metadata), "exit").Int() != 0 || gjson.Get(structured, "exit").Int() != 0)
		if item.State.Status == "error" || shellFailed {
			status = "errored"
			if item.State.Error.Message != "" {
				texts = append(texts, item.State.Error.Message)
			}
		}
		call.ResultEvents = []ParsedToolResultEvent{{
			ToolUseID: item.ID, Status: status, Content: strings.Join(texts, "\n"),
			Timestamp: millisToTime(item.Time.Completed),
		}}
	}
	return call
}
