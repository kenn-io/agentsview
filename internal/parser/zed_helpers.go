package parser

import (
	"encoding/json"
	"sort"
	"strings"
)

type zedDocMeta struct {
	model             string
	totalInputTokens  int
	totalOutputTokens int
	peakContextTokens int
	hasTokenUsage     bool
}

func extractZedDocMeta(doc map[string]any) zedDocMeta {
	var meta zedDocMeta
	if modelObj, ok := doc["model"].(map[string]any); ok {
		meta.model, _ = modelObj["model"].(string)
	}
	if requestUsage, ok := doc["request_token_usage"].(map[string]any); ok {
		for _, v := range requestUsage {
			entry, ok := v.(map[string]any)
			if !ok {
				continue
			}
			meta.hasTokenUsage = true
			if in, ok := entry["input_tokens"].(float64); ok {
				meta.totalInputTokens += int(in)
				if int(in) > meta.peakContextTokens {
					meta.peakContextTokens = int(in)
				}
			}
			if out, ok := entry["output_tokens"].(float64); ok {
				meta.totalOutputTokens += int(out)
			}
		}
	}
	return meta
}

func zedExtractText(v any) string {
	var parts []string
	zedWalk(v, func(obj map[string]any) {
		if text, ok := obj["Text"].(string); ok {
			parts = append(parts, text)
			return
		}
		if text, ok := obj["Text"].(map[string]any); ok {
			if value, ok := text["text"].(string); ok {
				parts = append(parts, value)
			}
		}
	})
	return strings.Join(parts, "\n")
}

func zedExtractThinking(v any) string {
	var parts []string
	zedWalk(v, func(obj map[string]any) {
		thinking, ok := obj["Thinking"].(map[string]any)
		if !ok {
			return
		}
		if text, ok := thinking["text"].(string); ok {
			parts = append(parts, text)
		}
	})
	return strings.Join(parts, "\n")
}

func zedExtractToolCalls(v any) []ParsedToolCall {
	var calls []ParsedToolCall
	zedWalk(v, func(obj map[string]any) {
		tool, ok := obj["ToolUse"].(map[string]any)
		if !ok {
			return
		}
		input := tool["input"]
		if input == nil {
			input = tool["raw_input"]
		}
		inputJSON := zedMarshalRaw(input)
		name, _ := tool["name"].(string)
		id, _ := tool["id"].(string)
		calls = append(calls, ParsedToolCall{
			ToolUseID: id,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: inputJSON,
		})
	})
	return calls
}

func zedExtractToolResults(v any) []ParsedToolResult {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	agent, ok := obj["tool_results"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(agent))
	for key := range agent {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	results := make([]ParsedToolResult, 0, len(keys))
	for _, key := range keys {
		text := zedToolResultText(agent[key])
		quoted, _ := json.Marshal(text)
		results = append(results, ParsedToolResult{
			ToolUseID:     key,
			ContentRaw:    string(quoted),
			ContentLength: len(text),
		})
	}
	return results
}

// zedToolResultText extracts plain text from a Zed tool result entry.
// Zed stores results as objects with an "output" string or a "content" array
// of {"Text":"..."} blocks; either is normalised to a string that
// DecodeContent can decode via its gjson.String branch.
func zedToolResultText(v any) string {
	entry, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if out, ok := entry["output"].(string); ok && out != "" {
		return out
	}
	if content, ok := entry["content"]; ok {
		if text := strings.TrimSpace(zedExtractText(content)); text != "" {
			return text
		}
	}
	return ""
}

func zedWalk(v any, visit func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		visit(x)
		for _, child := range x {
			zedWalk(child, visit)
		}
	case []any:
		for _, child := range x {
			zedWalk(child, visit)
		}
	}
}

func zedMarshalRaw(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func zedProjectFromFolderPaths(paths string) string {
	if cwd := zedFirstFolderPath(paths); cwd != "" {
		if project := ExtractProjectFromCwd(cwd); project != "" {
			return project
		}
	}
	return "unknown"
}

func zedFirstFolderPath(paths string) string {
	paths = strings.TrimSpace(paths)
	if paths == "" {
		return ""
	}
	for _, sep := range []string{"\n", "\x00"} {
		if before, _, ok := strings.Cut(paths, sep); ok {
			return strings.TrimSpace(before)
		}
	}
	return paths
}

func zedParentSessionID(parentID string) string {
	if parentID == "" {
		return ""
	}
	return zedIDPrefix + parentID
}

func zedRelationshipType(parentID string) RelationshipType {
	if parentID == "" {
		return RelNone
	}
	return RelContinuation
}

// ZedSQLiteVirtualPath identifies one thread row inside a shared
// Zed threads.db file.
func ZedSQLiteVirtualPath(dbPath, sessionID string) string {
	return dbPath + "#" + sessionID
}
