package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// The unified harness session store (store format
// "mistral.vibe.unified-session-store/v1") replaced the legacy
// messages.jsonl + meta.json layout under <root>/unified/<session-id>/.
// A session directory holds:
//
//	CURRENT                      pointer to the newest generation
//	meta.json                    session metadata (absent for subagents)
//	journal/<seq>.jsonl          recovery journal (not parsed here)
//	chunks/<sha256>.json         content-addressed entry chunks
//	generations/<seq>/           snapshots, pruned to the newest two
//	  manifest.json              lists the projection and checkpoint chunks
//	  projection-state.json      session envelope with aggregate token usage
//	  runtime-state.json         runtime metadata (active model, cwd)
//
// The public transcript is the projection chunks listed by the newest
// generation's manifest, in listed order; each chunk is a JSON array of
// harness public-session-state entries (message, reasoning, effect, notice,
// checkpoint).

type vibeUnifiedCurrent struct {
	Generation string `json:"generation"`
}

type vibeUnifiedManifest struct {
	ProjectionState struct {
		Chunks []string `json:"chunks"`
	} `json:"projection_state"`
}

type vibeUnifiedTokenUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
}

type vibeUnifiedProjectionSession struct {
	Title        string                `json:"title"`
	CreatedAt    int64                 `json:"createdAt"`
	UpdatedAt    int64                 `json:"updatedAt"`
	TokenUsage   vibeUnifiedTokenUsage `json:"tokenUsage"`
	ContextUsage vibeUnifiedTokenUsage `json:"contextUsage"`
}

type vibeUnifiedProjectionState struct {
	Snapshot struct {
		Session vibeUnifiedProjectionSession `json:"session"`
		History struct {
			Entries []vibeUnifiedEntry `json:"entries"`
		} `json:"history"`
	} `json:"snapshot"`
}

// vibeUnifiedRuntimeMetadata carries the identity-bearing fields of
// runtime-state.json. The file also embeds plugin and skill manifests, so
// only these fields are decoded.
type vibeUnifiedRuntimeMetadata struct {
	Identity struct {
		Kind            string `json:"kind"`
		ParentSessionID string `json:"parent_session_id"`
	} `json:"identity"`
	SessionMetadata struct {
		ActiveModel string `json:"active_model"`
		Cwd         string `json:"cwd"`
	} `json:"session_metadata"`
	ImportProvenance *struct {
		Source struct {
			SessionID string `json:"session_id"`
		} `json:"source"`
	} `json:"import_provenance"`
}

type vibeUnifiedEntry struct {
	Type               string                  `json:"type"`
	Role               string                  `json:"role"`
	ID                 string                  `json:"id"`
	TurnID             string                  `json:"turnId"`
	Text               string                  `json:"text"`
	Summary            []string                `json:"summary,omitempty"`
	Content            jsontext.Value          `json:"content,omitempty"`
	UserDisplayContent jsontext.Value          `json:"userDisplayContent,omitempty"`
	CreatedAt          int64                   `json:"createdAt"`
	UpdatedAt          int64                   `json:"updatedAt"`
	Detail             *vibeUnifiedEffect      `json:"detail,omitempty"`
	State              *vibeUnifiedEffectState `json:"state,omitempty"`
}

type vibeUnifiedEffect struct {
	Kind           string         `json:"kind,omitempty"`
	ToolName       string         `json:"toolName,omitempty"`
	Input          jsontext.Value `json:"input,omitempty"`
	ChildSessionID string         `json:"childSessionId,omitempty"`
}

type vibeUnifiedEffectState struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Output     *vibeUnifiedEffectOutput `json:"output,omitempty"`
	OutputText string                   `json:"outputText,omitempty"`
}

type vibeUnifiedEffectOutput struct {
	Content jsontext.Value `json:"content,omitempty"`
}

// vibeIsUnifiedAnchor reports whether path is the CURRENT anchor of a unified
// session directory (.../unified/<session-id>/CURRENT).
func vibeIsUnifiedAnchor(path string) bool {
	return filepath.Base(path) == "CURRENT" &&
		filepath.Base(filepath.Dir(filepath.Dir(path))) == "unified"
}

// vibeUnifiedSessionDirFromRel maps CURRENT and meta.json to their session directory.
func vibeUnifiedSessionDirFromRel(rel, root string) (string, bool) {
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] != "unified" || (parts[2] != "CURRENT" && parts[2] != "meta.json") {
		return "", false
	}
	return filepath.Join(filepath.Clean(root), "unified", parts[1]), true
}

// vibeUnifiedGenerationDir resolves the generation named by CURRENT.
func vibeUnifiedGenerationDir(sessionDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(sessionDir, "CURRENT"))
	if err != nil {
		return "", fmt.Errorf("reading Vibe unified CURRENT: %w", err)
	}
	var current vibeUnifiedCurrent
	if err := json.Unmarshal(raw, &current); err != nil {
		return "", fmt.Errorf("parsing Vibe unified CURRENT: %w", err)
	}
	if !isSafeSinglePathComponent(current.Generation) {
		return "", errors.New("invalid generation in Vibe unified CURRENT")
	}
	return filepath.Join(sessionDir, "generations", current.Generation), nil
}

// parseVibeUnifiedResultFile parses a unified harness session directory,
// anchored on its CURRENT pointer, into a ParseResult.
func parseVibeUnifiedResultFile(anchorPath string, fileInfo FileInfo) (ParseResult, error) {
	sessionDir := filepath.Dir(anchorPath)
	dirName := filepath.Base(sessionDir)

	result := ParseResult{
		Session: ParsedSession{
			Agent:           AgentVibe,
			File:            fileInfo,
			ID:              "vibe:" + dirName,
			SourceSessionID: dirName,
			Project:         "vibe",
			EndedAt:         time.Unix(0, fileInfo.Mtime),
		},
	}

	genDir, err := vibeUnifiedGenerationDir(sessionDir)
	if err != nil {
		return result, err
	}

	var manifest vibeUnifiedManifest
	if err := readVibeUnifiedDoc(genDir, "manifest.json", &manifest); err != nil {
		return result, err
	}

	var projection vibeUnifiedProjectionState
	if err := readVibeUnifiedDoc(genDir, "projection-state.json", &projection); err != nil {
		return result, err
	}
	projSession := projection.Snapshot.Session

	_, _, _, err = applyVibeMetadata(&result, sessionDir)
	if err != nil {
		return result, err
	}
	result.Session.ID = "vibe:" + dirName
	result.Session.SourceSessionID = dirName
	var parentID string

	var runtimeMeta vibeUnifiedRuntimeMetadata
	if err := readVibeUnifiedDoc(genDir, "runtime-state.json", &runtimeMeta); err != nil {
		return result, err
	}
	if result.Session.Cwd == "" {
		result.Session.Cwd = runtimeMeta.SessionMetadata.Cwd
		if project := ExtractProjectFromCwdWithBranch(result.Session.Cwd, result.Session.GitBranch); project != "" {
			result.Session.Project = project
		}
	}
	runtimeKind := runtimeMeta.Identity.Kind
	if runtimeKind == "subagent" || runtimeKind == "fork" {
		parentID = runtimeMeta.Identity.ParentSessionID
	}

	if runtimeMeta.ImportProvenance != nil {
		parentID = runtimeMeta.ImportProvenance.Source.SessionID
	}

	if result.Session.SessionName == "" {
		result.Session.SessionName = projSession.Title
	}
	if result.Session.StartedAt.IsZero() {
		result.Session.StartedAt = time.Unix(0, fileInfo.Mtime)
		if projSession.CreatedAt > 0 {
			result.Session.StartedAt = time.UnixMilli(projSession.CreatedAt)
		}
	}
	if projSession.UpdatedAt > 0 {
		result.Session.EndedAt = time.UnixMilli(projSession.UpdatedAt)
	}

	if parentID != "" {
		result.Session.ParentSessionID = "vibe:" + parentID
		switch {
		case runtimeKind == "subagent":
			result.Session.RelationshipType = RelSubagent
		case runtimeKind == "fork":
			result.Session.RelationshipType = RelFork
		case runtimeMeta.ImportProvenance != nil:
			result.Session.RelationshipType = RelContinuation
		}
	}

	if projSession.TokenUsage.OutputTokens > 0 {
		result.Session.HasTotalOutputTokens = true
		result.Session.TotalOutputTokens = projSession.TokenUsage.OutputTokens
	}
	if contextTokens := projSession.ContextUsage.InputTokens + projSession.ContextUsage.OutputTokens; contextTokens > 0 {
		result.Session.HasPeakContextTokens = true
		result.Session.PeakContextTokens = contextTokens
	}
	sessionModel := runtimeMeta.SessionMetadata.ActiveModel
	if sessionModel == "" {
		sessionModel, _ = vibeUnifiedParentModel(sessionDir, runtimeMeta.Identity.ParentSessionID)
	}
	messages, err := parseVibeUnifiedChunks(sessionDir, manifest.ProjectionState.Chunks, projection.Snapshot.History.Entries, sessionModel)
	if err != nil {
		return result, err
	}
	result.Messages = messages
	setVibeMessageMetadata(&result)

	stats := VibeStats{
		SessionPromptTokens:     projSession.TokenUsage.InputTokens,
		SessionCompletionTokens: projSession.TokenUsage.OutputTokens,
		SessionCachedTokens:     projSession.TokenUsage.CachedInputTokens,
	}
	result.UsageEvents = vibeUsageEvents(
		stats, sessionModel, result.Session.ID,
		result.Session.StartedAt, result.Session.EndedAt,
	)

	return result, nil
}

// parseVibeUnifiedChunks uses inline history unless the manifest names a chunk list.
func parseVibeUnifiedChunks(sessionDir string, chunkHashes []string, entries []vibeUnifiedEntry, model string) ([]ParsedMessage, error) {
	if chunkHashes != nil {
		entries = nil
		for _, chunkHash := range chunkHashes {
			if !isSafeSinglePathComponent(chunkHash) {
				return nil, fmt.Errorf("invalid Vibe unified chunk hash %q", chunkHash)
			}
			raw, err := os.ReadFile(filepath.Join(sessionDir, "chunks", chunkHash+".json"))
			if err != nil {
				return nil, fmt.Errorf("reading Vibe unified chunk %s: %w", chunkHash, err)
			}
			var chunkEntries []vibeUnifiedEntry
			if err := json.Unmarshal(raw, &chunkEntries); err != nil {
				return nil, fmt.Errorf("parsing Vibe unified chunk %s: %w", chunkHash, err)
			}
			entries = append(entries, chunkEntries...)
		}
	}
	var messages []ParsedMessage
	var thinking, thinkingTurn string
	var thinkingCreatedAt int64
	flushThinking := func() {
		if thinking != "" {
			msg := ParsedMessage{
				Ordinal: len(messages), Role: RoleAssistant, Model: model,
				ThinkingText: thinking, HasThinking: true,
			}
			if thinkingCreatedAt > 0 {
				msg.Timestamp = time.UnixMilli(thinkingCreatedAt)
			}
			messages = append(messages, msg)
			thinking = ""
		}
	}
	for _, entry := range entries {
		if entry.Type == "notice" || entry.Type == "checkpoint" {
			continue
		}
		if thinking != "" && entry.TurnID != thinkingTurn {
			flushThinking()
		}
		switch entry.Type {
		case "message":
			msg := vibeUnifiedEntryMessage(entry)
			msg.Ordinal = len(messages)
			if msg.Role == RoleAssistant {
				msg.Model = model
				msg.ThinkingText = thinking
				msg.HasThinking = thinking != ""
				thinking = ""
			}
			messages = append(messages, msg)
		case "reasoning":
			thinkingTurn = entry.TurnID
			thinkingCreatedAt = entry.CreatedAt
			if entry.Text != "" {
				thinking += entry.Text
			} else {
				thinking += strings.Join(entry.Summary, "")
			}
		case "effect":
			call := vibeUnifiedEffectMessages(entry, len(messages))
			if call != nil {
				call.Model = model
				call.ThinkingText = thinking
				call.HasThinking = call.ThinkingText != ""
				thinking = ""
				messages = append(messages, *call)
			}
		}
	}
	flushThinking()
	return messages, nil
}

// vibeUnifiedEntryMessage converts a public message entry.
func vibeUnifiedEntryMessage(entry vibeUnifiedEntry) ParsedMessage {
	content := gjson.Parse(string(entry.Content))
	if entry.Role == "user" {
		display := gjson.Parse(string(entry.UserDisplayContent))
		if !display.IsObject() {
			for _, block := range content.Array() {
				if meta := block.Get(`_meta.vibe\.userDisplayContent`); meta.IsObject() {
					display = meta
					break
				}
			}
		}
		if literal := display.Get("content"); literal.IsArray() {
			content = literal
		}
	}
	text := vibeUnifiedContentText(content, "")
	msg := ParsedMessage{
		Role:          RoleType(entry.Role),
		Content:       text,
		ContentLength: len(text),
		SourceUUID:    entry.ID,
	}
	if entry.CreatedAt > 0 {
		msg.Timestamp = time.UnixMilli(entry.CreatedAt)
	}
	if entry.Role != "user" && entry.Role != "assistant" {
		msg.IsSystem = true
	}
	return msg
}

func vibeUnifiedContentText(content gjson.Result, separator string) string {
	if content.IsArray() {
		var parts []string
		for _, block := range content.Array() {
			switch block.Get("type").Str {
			case "image":
				parts = append(parts, "[image]")
			case "audio":
				parts = append(parts, "[audio]")
			case "resource", "resource_link":
				parts = append(parts, firstNonEmptyJSONLString(block.Get("resource.text").Str, "[resource]"))
			default:
				parts = append(parts, firstNonEmptyJSONLString(block.Get("text").Str, block.Get("result").Str))
			}
		}
		return strings.Join(parts, separator)
	}
	return decodeContent(content)
}

// Execution events carry output for running and terminal effects.
func vibeUnifiedEffectMessages(entry vibeUnifiedEntry, ordinal int) *ParsedMessage {
	if entry.ID == "" || entry.Detail == nil {
		return nil
	}
	toolName := entry.Detail.ToolName
	if toolName == "" {
		toolName = entry.Detail.Kind
	}
	if toolName == "" {
		return nil
	}
	inputJSON := string(entry.Detail.Input)
	if inputJSON == "" || strings.TrimSpace(inputJSON) == "null" {
		inputJSON = "{}"
	}

	call := &ParsedMessage{
		Ordinal:    ordinal,
		Role:       RoleAssistant,
		HasToolUse: true,
		SourceUUID: entry.ID,
		ToolCalls: []ParsedToolCall{{
			ToolUseID: entry.ID,
			ToolName:  toolName,
			Category:  NormalizeToolCategory(strings.TrimPrefix(toolName, "file_system.")),
			InputJSON: inputJSON,
		}},
	}
	if entry.CreatedAt > 0 {
		call.Timestamp = time.UnixMilli(entry.CreatedAt)
	}
	if toolName == "subagent.spawn" {
		if entry.Detail.ChildSessionID != "" {
			call.ToolCalls[0].SubagentSessionID = "vibe:" + entry.Detail.ChildSessionID
		}
	}

	resultText := ""
	if entry.State != nil {
		if entry.State.Output != nil {
			resultText = vibeUnifiedToolResultContent(gjson.Parse(string(entry.State.Output.Content)))
		}
		resultText = firstNonEmptyJSONLString(resultText, entry.State.OutputText, entry.State.Reason, entry.State.Error.Message)
		status := entry.State.Status
		switch status {
		case "failed", "skipped":
			status = "errored"
		}
		if status == "running" || status == "completed" || status == "errored" || status == "cancelled" {
			event := ParsedToolResultEvent{ToolUseID: entry.ID, Source: "tool_execution", Status: status, Content: resultText}
			if entry.UpdatedAt > 0 {
				event.Timestamp = time.UnixMilli(entry.UpdatedAt)
			}
			call.ToolCalls[0].ResultEvents = []ParsedToolResultEvent{
				{ToolUseID: entry.ID, Source: "tool_execution", Status: "started", Timestamp: call.Timestamp},
				event,
			}
		}
	}
	return call
}

func readVibeUnifiedDoc(genDir, name string, v any) error {
	raw, err := os.ReadFile(filepath.Join(genDir, name))
	if err != nil {
		return fmt.Errorf("reading Vibe unified %s: %w", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parsing Vibe unified %s: %w", name, err)
	}
	return nil
}

func vibeUnifiedToolResultContent(content gjson.Result) string {
	hasImages := false
	for _, block := range content.Array() {
		if block.Get("type").Str == "image" {
			hasImages = true
			break
		}
	}
	if !hasImages {
		return vibeUnifiedContentText(content, "\n")
	}
	var blocks []map[string]string
	for _, block := range content.Array() {
		if block.Get("type").Str == "image" {
			blocks = append(blocks, map[string]string{
				"type": "input_image", "image_url": "data:" + block.Get("mimeType").Str + ";base64," + block.Get("data").Str,
			})
		} else {
			text := vibeUnifiedContentText(gjson.Parse("["+block.Raw+"]"), "")
			blocks = append(blocks, map[string]string{"type": "text", "text": text})
		}
	}
	encoded, _ := json.Marshal(blocks, json.Deterministic(true))
	return string(encoded)
}

func vibeUnifiedParentModel(sessionDir, parentID string) (string, int64) {
	if !isSafeSinglePathComponent(parentID) {
		return "", 0
	}
	parentDir := filepath.Join(filepath.Dir(sessionDir), parentID)
	parentGenDir, err := vibeUnifiedGenerationDir(parentDir)
	if err != nil {
		return "", 0
	}
	var parentMeta vibeUnifiedRuntimeMetadata
	if err := readVibeUnifiedDoc(parentGenDir, "runtime-state.json", &parentMeta); err != nil {
		return "", 0
	}
	info, err := os.Stat(filepath.Join(parentDir, "CURRENT"))
	if err != nil {
		return parentMeta.SessionMetadata.ActiveModel, 0
	}
	return parentMeta.SessionMetadata.ActiveModel, info.ModTime().UnixNano()
}

// CURRENT pins the manifest and its documents by digest; inherited models can change independently.
func vibeUnifiedFingerprint(sessionDir string) (SourceFingerprint, error) {
	var size, mtime int64
	hash := sha256.New()
	for _, path := range []string{
		filepath.Join(sessionDir, "CURRENT"),
		filepath.Join(sessionDir, "meta.json"),
	} {
		info, err := os.Stat(path)
		if filepath.Base(path) == "meta.json" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return SourceFingerprint{}, err
		}
		size += info.Size()
		mtime = max(mtime, info.ModTime().UnixNano())
		if err := addSiblingMetadataFingerprintPart(hash, filepath.Base(path), path, info); err != nil {
			return SourceFingerprint{}, err
		}
	}
	genDir, err := vibeUnifiedGenerationDir(sessionDir)
	if err != nil {
		return SourceFingerprint{}, err
	}
	var runtimeMeta vibeUnifiedRuntimeMetadata
	if err := readVibeUnifiedDoc(genDir, "runtime-state.json", &runtimeMeta); err != nil {
		return SourceFingerprint{}, err
	}
	if runtimeMeta.SessionMetadata.ActiveModel == "" {
		parentModel, parentMtime := vibeUnifiedParentModel(sessionDir, runtimeMeta.Identity.ParentSessionID)
		_, _ = fmt.Fprintf(hash, "parent-model:%s\n", parentModel)
		mtime = max(mtime, parentMtime)
	}
	return SourceFingerprint{Size: size, MTimeNS: mtime, Hash: hex.EncodeToString(hash.Sum(nil))}, nil
}
