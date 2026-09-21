package parser

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

// grokSubagentParentFromDisk returns the spawning parent session id and the
// on-disk meta.json that recorded the spawn. Grok stores child sessions as
// siblings (or, for worktree isolation, under another encoded cwd group) and
// writes the parent link at
// <parent-session>/subagents/<child-id>/meta.json.
func grokSubagentParentFromDisk(
	sessionDir, producerSessionKind string,
) (parentID, metaPath string, ok bool) {
	childID := filepath.Base(sessionDir)
	if !IsValidSessionID(childID) {
		return "", "", false
	}
	cwdDir := filepath.Dir(sessionDir)
	if parentID, metaPath, ok = grokFindSubagentParentInCWD(cwdDir, childID); ok {
		return parentID, metaPath, true
	}
	if !strings.HasPrefix(producerSessionKind, "subagent") {
		return "", "", false
	}
	return grokFindSubagentParentUnderRoot(filepath.Dir(cwdDir), cwdDir, childID)
}

func grokFindSubagentParentInCWD(
	cwdDir, childID string,
) (parentID, metaPath string, ok bool) {
	entries, err := os.ReadDir(cwdDir)
	if err != nil {
		return "", "", false
	}
	for _, entry := range entries {
		if entry.Name() == childID || !IsValidSessionID(entry.Name()) {
			continue
		}
		path := filepath.Join(
			cwdDir, entry.Name(), "subagents", childID, "meta.json",
		)
		parent, child, readable := readGrokSubagentMeta(path)
		if !readable {
			continue
		}
		if child != "" && child != childID {
			continue
		}
		if parent == "" || parent == childID {
			continue
		}
		return parent, path, true
	}
	return "", "", false
}

func grokFindSubagentParentUnderRoot(
	root, skipCWD, childID string,
) (parentID, metaPath string, ok bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", "", false
	}
	skipCWD = filepath.Clean(skipCWD)
	for _, entry := range entries {
		cwdPath := filepath.Join(root, entry.Name())
		if filepath.Clean(cwdPath) == skipCWD {
			continue
		}
		if parentID, metaPath, ok = grokFindSubagentParentInCWD(
			cwdPath, childID,
		); ok {
			return parentID, metaPath, true
		}
	}
	return "", "", false
}

func readGrokSubagentMeta(path string) (parentID, childID string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || !gjson.ValidBytes(data) {
		return "", "", false
	}
	root := gjson.ParseBytes(data)
	parentID = strings.TrimSpace(root.Get("parent_session_id").String())
	childID = strings.TrimSpace(root.Get("child_session_id").String())
	if parentID == "" || !IsValidSessionID(parentID) {
		return "", "", false
	}
	if childID != "" && !IsValidSessionID(childID) {
		return "", "", false
	}
	return parentID, childID, true
}

func grokSummarySessionKind(summaryPath string) string {
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(data, "session_kind").String())
}

func grokParentSubagentMetaPath(summaryPath string) string {
	sessionDir := filepath.Dir(summaryPath)
	_, metaPath, ok := grokSubagentParentFromDisk(
		sessionDir, grokSummarySessionKind(summaryPath),
	)
	if !ok {
		return ""
	}
	return metaPath
}

func grokAttachSpawnedSubagents(messages []ParsedMessage) {
	results := make(map[string]string)
	for _, msg := range messages {
		for _, result := range msg.ToolResults {
			id := grokSubagentIDFromResult(result.ContentRaw)
			if id == "" || result.ToolUseID == "" {
				continue
			}
			results[result.ToolUseID] = "grok:" + id
		}
	}
	if len(results) == 0 {
		return
	}
	for i := range messages {
		for j := range messages[i].ToolCalls {
			tc := &messages[i].ToolCalls[j]
			if !grokSpawnSubagentTool(tc.ToolName) {
				continue
			}
			if sid, ok := results[tc.ToolUseID]; ok {
				tc.SubagentSessionID = sid
			}
		}
	}
}

func grokSpawnSubagentTool(name string) bool {
	switch name {
	case "spawn_subagent", "task", "Task":
		return true
	default:
		return false
	}
}

func grokSubagentIDFromResult(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	if gjson.Valid(text) {
		parsed := gjson.Parse(text)
		switch parsed.Type {
		case gjson.String:
			text = parsed.Str
		case gjson.Null, gjson.False, gjson.Number, gjson.True:
			return ""
		case gjson.JSON:
			if id := strings.TrimSpace(parsed.Get("subagent_id").String()); id != "" {
				if IsValidSessionID(id) {
					return id
				}
			}
		}
	}
	const marker = "subagent_id:"
	idx := strings.Index(strings.ToLower(text), marker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(text[idx+len(marker):])
	if rest == "" {
		return ""
	}
	if end := strings.IndexAny(rest, " \t\r\n,;"); end >= 0 {
		rest = rest[:end]
	}
	if IsValidSessionID(rest) {
		return rest
	}
	return ""
}
