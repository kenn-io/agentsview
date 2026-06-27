// ABOUTME: Parses QwenPaw sessions/<name>.json files into structured session data.
// ABOUTME: Handles Anthropic-style content blocks (text/thinking/tool_use/
// ABOUTME: tool_result) with system-role carriers for tool results.
package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// IsValidQwenPawIDPart accepts workspace names and session file
// stems. QwenPaw emits channel-scoped filenames containing dots,
// at-signs, and double dashes (e.g. "<userId>@im.wechat_wechat--..."),
// so the check is permissive — but it still rejects path traversal
// components (".", "..") since those never appear in a real session
// stem and would let a crafted raw ID escape the QwenPaw root.
//
// It also rejects characters that are structurally significant once a
// part is joined into a session ID:
//
//   - ":" joins ID parts in qwenpawSessionID. A stem "foo:bar" would
//     produce qwenpaw:<workspace>:foo:bar, which source lookup reparses
//     as the sessions/foo/bar.json subdir layout.
//   - "~" is the remote-host separator (see StripHostPrefix). A part
//     containing it would be split off as a bogus host prefix.
//   - "?", "#", and "%" are URL delimiters. Session IDs are
//     interpolated into API path segments that are not all percent-
//     encoded (e.g. the export and watch routes), so these would
//     truncate or corrupt the path.
func IsValidQwenPawIDPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\:~?#%") {
		return false
	}
	if strings.ContainsRune(s, 0) {
		return false
	}
	return true
}

// qwenpawSessionID builds the canonical session ID for a file at
// `path`. Layout determines the namespace:
//
//   - <workspace>/sessions/<stem>.json            -> qwenpaw:<workspace>:<stem>
//   - <workspace>/sessions/<subdir>/<stem>.json   -> qwenpaw:<workspace>:<subdir>:<stem>
//
// The subdir segment prevents collisions when both
// sessions/foo.json and sessions/console/foo.json exist in the same
// workspace (otherwise both would become qwenpaw:<workspace>:foo
// and one would overwrite the other during sync).
//
// Every component is validated with IsValidQwenPawIDPart before
// joining: a ":" (the part separator) or path separator in any of
// workspace/subdir/stem would make the ID ambiguous, so such a file
// is rejected rather than silently colliding with another session.
func qwenpawSessionID(path, project, stem string) (string, error) {
	if !IsValidQwenPawIDPart(project) {
		return "", fmt.Errorf(
			"qwenpaw: invalid workspace %q for %s", project, path,
		)
	}
	if !IsValidQwenPawIDPart(stem) {
		return "", fmt.Errorf(
			"qwenpaw: invalid session stem %q for %s", stem, path,
		)
	}
	parent := filepath.Base(filepath.Dir(path))
	if parent == "sessions" {
		return "qwenpaw:" + project + ":" + stem, nil
	}
	if !IsValidQwenPawIDPart(parent) {
		return "", fmt.Errorf(
			"qwenpaw: invalid subdir %q for %s", parent, path,
		)
	}
	return "qwenpaw:" + project + ":" + parent + ":" + stem, nil
}

// parseSession parses a QwenPaw sessions/<name>.json file.
//
// The on-disk shape is:
//
//	{
//	  "agent": {
//	    "memory": {
//	      "content": [[message, []], [message, []], ...]
//	    }
//	  }
//	}
//
// Each message has fields:
//
//   - id:        message identifier (string)
//   - name:      sender name ("user", "<AgentName>", or "system")
//   - role:      "user" | "assistant" | "system"
//   - content:   array of content blocks (text/thinking/tool_use/tool_result)
//   - metadata:  opaque object
//   - timestamp: "YYYY-MM-DD HH:MM:SS.fff" (local time, milliseconds)
//
// System-role messages carry tool_result blocks (QwenPaw's equivalent
// of Anthropic's user-side tool_result). They map to RoleUser +
// IsSystem so they remain distinguishable from real user turns
// without inflating UserMessageCount.
func parseQwenPawSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// gjson.GetBytes silently returns "not found" on malformed JSON,
	// which would otherwise surface as "agent.memory.content missing"
	// — masking the real cause. Validate up front so syntax errors
	// are reported as such.
	if !gjson.ValidBytes(raw) {
		return nil, nil, fmt.Errorf(
			"qwenpaw: malformed JSON in %s", path,
		)
	}

	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	id, err := qwenpawSessionID(path, project, stem)
	if err != nil {
		return nil, nil, err
	}
	sess := &ParsedSession{
		ID:      id,
		Project: project,
		Machine: machine,
		Agent:   AgentQwenPaw,
	}

	contentArr := gjson.GetBytes(raw, "agent.memory.content")
	if !contentArr.Exists() {
		return nil, nil, fmt.Errorf(
			"qwenpaw: agent.memory.content missing in %s", path,
		)
	}
	if !contentArr.IsArray() {
		return nil, nil, fmt.Errorf(
			"qwenpaw: agent.memory.content not an array in %s", path,
		)
	}

	var messages []ParsedMessage
	var malformed int
	ordinal := 0
	contentArr.ForEach(func(_, entry gjson.Result) bool {
		if !entry.IsArray() {
			malformed++
			return true
		}
		items := entry.Array()
		if len(items) == 0 {
			malformed++
			return true
		}
		msgJSON := items[0]
		if !msgJSON.IsObject() {
			malformed++
			return true
		}
		pm, ok := parseQwenPawMessage(msgJSON, ordinal)
		if !ok {
			malformed++
			return true
		}
		ordinal++
		messages = append(messages, pm)
		return true
	})

	sess.MalformedLines = malformed
	sess.MessageCount = len(messages)
	for _, m := range messages {
		if m.Role == RoleUser && !m.IsSystem {
			sess.UserMessageCount++
		}
	}
	if len(messages) > 0 {
		sess.StartedAt = messages[0].Timestamp
		sess.EndedAt = messages[len(messages)-1].Timestamp
	}
	for _, m := range messages {
		if m.Role == RoleUser && !m.IsSystem && strings.TrimSpace(m.Content) != "" {
			sess.FirstMessage = truncateFirstMessage(m.Content)
			break
		}
	}

	populateQwenPawFileFields(sess, info, path)
	if messages == nil {
		return sess, nil, nil
	}
	return sess, messages, nil
}

// parseQwenPawMessage turns one gjson message object into a
// ParsedMessage. Returns ok=false when the role is missing or
// unrecognized.
func parseQwenPawMessage(
	msg gjson.Result, ordinal int,
) (ParsedMessage, bool) {
	roleStr := msg.Get("role").Str
	role, isSystem, ok := qwenpawRole(roleStr)
	if !ok {
		return ParsedMessage{}, false
	}
	pm := ParsedMessage{
		Ordinal:   ordinal,
		Role:      role,
		IsSystem:  isSystem,
		Timestamp: parseQwenPawTimestamp(msg.Get("timestamp").Str),
	}

	var textParts []string
	content := msg.Get("content")
	if content.IsArray() {
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").Str {
			case "text":
				if t := block.Get("text").Str; t != "" {
					textParts = append(textParts, t)
				}
			case "thinking":
				if th := block.Get("thinking").Str; th != "" {
					pm.HasThinking = true
					if pm.ThinkingText != "" {
						pm.ThinkingText += "\n"
					}
					pm.ThinkingText += th
				}
			case "tool_use":
				tc := ParsedToolCall{
					ToolUseID: block.Get("id").Str,
					ToolName:  block.Get("name").Str,
					Category:  NormalizeToolCategory(block.Get("name").Str),
				}
				tc.InputJSON = qwenpawToolInputJSON(block)
				pm.ToolCalls = append(pm.ToolCalls, tc)
				pm.HasToolUse = true
			case "tool_result":
				output := block.Get("output")
				tr := ParsedToolResult{
					ToolUseID:     block.Get("id").Str,
					ContentLength: toolResultContentLength(output),
					ContentRaw:    output.Raw,
				}
				pm.ToolResults = append(pm.ToolResults, tr)
			}
			return true
		})
	}

	pm.Content = strings.Join(textParts, "\n")
	pm.ContentLength = len(pm.Content)
	return pm, true
}

// qwenpawRole maps a QwenPaw role string to (ParsedMessage role,
// IsSystem, ok). System messages map to RoleUser + IsSystem so tool
// results remain queryable as user-side content while staying
// distinguishable from real user turns.
func qwenpawRole(role string) (RoleType, bool, bool) {
	switch role {
	case "user":
		return RoleUser, false, true
	case "assistant":
		return RoleAssistant, false, true
	case "system":
		return RoleUser, true, true
	}
	return "", false, false
}

// qwenpawToolInputJSON selects the canonical tool input payload.
// QwenPaw echoes the parser input both as a structured "input"
// object and as a JSON-string "raw_input"; prefer "raw_input"
// when present because it is the verbatim original, falling back
// to the raw "input" object as emitted by gjson.
func qwenpawToolInputJSON(block gjson.Result) string {
	if raw := block.Get("raw_input"); raw.Exists() {
		if s := strings.TrimSpace(raw.Str); s != "" {
			return s
		}
	}
	if input := block.Get("input"); input.Exists() {
		return input.Raw
	}
	return "{}"
}

// parseQwenPawTimestamp parses QwenPaw's "YYYY-MM-DD HH:MM:SS.fff"
// format. Timestamps are recorded as local wall-clock time without a
// timezone offset, so naive values are interpreted as time.Local
// (mirroring the Hermes parser). Empty or unparseable inputs return
// the zero time.
func parseQwenPawTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// truncateFirstMessage caps FirstMessage length to keep list views
// readable; the constant matches the truncation length other parsers
// use for the same field.
func truncateFirstMessage(s string) string {
	const max = 300
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// populateQwenPawFileFields fills the File metadata on the session.
func populateQwenPawFileFields(
	sess *ParsedSession, info os.FileInfo, path string,
) {
	sess.File = FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}
}
