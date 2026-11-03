package parser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func parsePiLikeSession(
	ctx context.Context,
	path, project, machine string,
	agent AgentType,
	idPrefix string,
) (*ParsedSession, []ParsedMessage, error) {
	return parsePiLikeSessionContext(ctx, path, project, machine, agent, idPrefix, piHeaderLookup(piSessionHeader), nil)
}

func parsePiLikeSessionContext(ctx context.Context, path, project, machine string, agent AgentType, idPrefix string, read piHeaderLookup, resolve func(string) (string, bool)) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)

	// --- Parse session header (first non-whitespace line) ---
	// Skip whitespace-only lines to stay consistent with
	// IsPiSessionFile in discovery.go which uses TrimSpace. OMP (Oh My Pi)
	// v16.3+ prefixes the header with a fixed-width rewritable
	// {"type":"title",...} slot line holding the current session title;
	// skip it too (matching IsPiSessionFile) and keep its title.
	var headerLine, slotTitle string
	for {
		line, ok := lr.next()
		if !ok {
			return nil, nil, fmt.Errorf(
				"not a pi session: missing session header in %s", path,
			)
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if gjson.Get(line, "type").Str == "title" {
			slotTitle = gjson.Get(line, "title").Str
			continue
		}
		headerLine = line
		break
	}

	if !gjson.Valid(headerLine) {
		return nil, nil, fmt.Errorf(
			"not a pi session: invalid JSON header in %s", path,
		)
	}

	if gjson.Get(headerLine, "type").Str != "session" {
		return nil, nil, fmt.Errorf(
			"not a pi session: missing session header in %s", path,
		)
	}

	sessionID := gjson.Get(headerLine, "id").Str
	cwd := gjson.Get(headerLine, "cwd").Str
	headerTitle := gjson.Get(headerLine, "title").Str
	headerTimestamp := parseTimestamp(gjson.Get(headerLine, "timestamp").Str)

	// If project was not passed in, derive from cwd.
	if project == "" && cwd != "" {
		project = ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
	}

	parentSessionID, isOMPSubagent, isPiSubagent := resolvePiLineage(
		path, headerLine, agent, idPrefix, read, resolve,
	)

	// V1 detection: if header has no id, we may need to derive from filename.
	isV1 := sessionID == ""

	// --- Main message loop ---
	var (
		messages      []ParsedMessage
		firstMessage  string
		sessionName   string
		ordinal       int
		userCount     int
		currentModel  string
		currentEffort string
		assistantByID map[string]int
	)
	assistantByID = make(map[string]int)
	// Pi emits metadata rows that stay in the tree, so bridge them to the
	// nearest visible ancestor before assigning SourceParentUUID.
	visibleAncestorByID := map[string]string{}
	resolveVisibleAncestor := func(id string) string {
		if id == "" {
			return ""
		}
		return visibleAncestorByID[id]
	}

	for {
		line, ok := lr.next()
		if !ok {
			break
		}

		if !gjson.Valid(line) {
			continue
		}

		entryType := gjson.Get(line, "type").Str
		if entryType == "" {
			continue
		}
		entryID := gjson.Get(line, "id").Str
		parentID := gjson.Get(line, "parentId").Str

		// If any message entry has an id field, this is a V2 session.
		if isV1 && entryID != "" {
			isV1 = false
		}

		switch entryType {
		case "message":
			role := gjson.Get(line, "message.role").Str
			switch role {
			case "user":
				sourceParentUUID := resolveVisibleAncestor(parentID)
				msg := parsePiUserMessage(
					line, ordinal, entryID, sourceParentUUID,
				)
				if msg == nil {
					continue
				}
				if firstMessage == "" && msg.Content != "" {
					firstMessage = truncate(
						strings.ReplaceAll(msg.Content, "\n", " "),
						300,
					)
				}
				messages = append(messages, *msg)
				if entryID != "" {
					visibleAncestorByID[entryID] = entryID
				}
				ordinal++
				userCount++

			case "assistant":
				sourceParentUUID := resolveVisibleAncestor(parentID)
				msg := parsePiAssistantMessage(
					ctx, line, ordinal, currentModel, currentEffort,
					entryID, sourceParentUUID, cwd,
				)
				if msg == nil {
					continue
				}
				if msg.Model != "" {
					currentModel = msg.Model
				}
				messages = append(messages, *msg)
				if entryID != "" {
					assistantByID[entryID] = len(messages) - 1
					visibleAncestorByID[entryID] = entryID
				}
				ordinal++

			case "toolResult":
				sourceParentUUID := resolveVisibleAncestor(parentID)
				msg := parsePiToolResultMessage(
					line, ordinal, entryID, sourceParentUUID,
				)
				if msg == nil {
					continue
				}
				messages = append(messages, *msg)
				if entryID != "" {
					visibleAncestorByID[entryID] = sourceParentUUID
				}
				ordinal++

			default:
				if entryID != "" {
					visibleAncestorByID[entryID] = resolveVisibleAncestor(
						parentID,
					)
				}
				// skip silently
			}

		case "model_change":
			if entryID != "" {
				visibleAncestorByID[entryID] = resolveVisibleAncestor(
					parentID,
				)
			}
			if id := gjson.Get(line, "modelId").Str; id != "" {
				currentModel = id
			}

		case "thinking_level_change":
			if entryID != "" {
				visibleAncestorByID[entryID] = resolveVisibleAncestor(
					parentID,
				)
			}
			if level := gjson.Get(line, "thinkingLevel").Str; level != "" {
				currentEffort = level
			}

		case "compaction":
			sourceParentUUID := resolveVisibleAncestor(parentID)
			msg := parsePiCompactionMessage(
				line, ordinal, entryID, sourceParentUUID,
			)
			if msg == nil {
				continue
			}
			messages = append(messages, *msg)
			if entryID != "" {
				visibleAncestorByID[entryID] = entryID
			}
			ordinal++

		case "session_info":
			if entryID != "" {
				visibleAncestorByID[entryID] = resolveVisibleAncestor(
					parentID,
				)
			}
			if name := gjson.Get(line, "name"); name.Exists() {
				sessionName = name.Str
			}

		case "child_usage_attributed":
			if entryID != "" {
				visibleAncestorByID[entryID] = resolveVisibleAncestor(
					parentID,
				)
			}
			if agent != AgentPrimeAgent {
				continue
			}
			targetID := gjson.Get(line, "targetId").Str
			if index, ok := assistantByID[targetID]; ok &&
				index >= 0 && index < len(messages) {
				applyPiUsage(
					&messages[index], gjson.Get(line, "aggregateUsage"),
				)
			}

		default:
			if entryID != "" {
				visibleAncestorByID[entryID] = resolveVisibleAncestor(
					parentID,
				)
			}
			// skip silently (entry types with no visible content)
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading pi %s: %w", path, err)
	}

	// Session name precedence: the OMP title slot is rewritten in place
	// and always holds the current title, so it outranks session_info
	// renames; the header title is the initial auto-generated fallback.
	if slotTitle != "" {
		sessionName = slotTitle
	} else if sessionName == "" {
		sessionName = headerTitle
	}

	// OMP subagent transcripts have an empty title slot and header title; the
	// meaningful label is the agent name, which is the transcript's filename.
	if isOMPSubagent && sessionName == "" {
		sessionName = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}

	// V1 fallback: derive session ID from filename.
	if isV1 || sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}

	// Compute StartedAt and EndedAt from message timestamps.
	startedAt := headerTimestamp
	var endedAt time.Time
	for _, m := range messages {
		if m.Timestamp.IsZero() {
			continue
		}
		if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
			startedAt = m.Timestamp
		}
		if endedAt.IsZero() || m.Timestamp.After(endedAt) {
			endedAt = m.Timestamp
		}
	}

	sess := &ParsedSession{
		ID:               idPrefix + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            agent,
		ParentSessionID:  parentSessionID,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		SessionName:      sessionName,
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
	// A persisted parent means the transcript was branched from another
	// session (/fork, /clone), not spawned as a subagent. A subagent child
	// found in a pi-subagents run directory still overrides this below.
	if (agent == AgentPrimeAgent || agent == AgentPi || agent == AgentOMO ||
		agent == AgentStepCode) && parentSessionID != "" {
		sess.RelationshipType = RelFork
	}
	if isOMPSubagent || isPiSubagent || (agent == AgentStepCode && isStepCodeChildSessionID(sessionID)) {
		sess.RelationshipType = RelSubagent
	}

	accumulateMessageTokenUsage(sess, messages)

	return sess, messages, nil
}

// isStepCodeChildSessionID reports whether a StepCode header ID belongs to a
// spawned subagent or workflow agent. StepCode writes those children beside
// the parent with no parentSession, so the ID prefix is the only marker.
func isStepCodeChildSessionID(id string) bool {
	return strings.HasPrefix(id, "subagent-") || strings.HasPrefix(id, "workflow-")
}

func piPersistedPathSessionID(value string) string {
	base := piPersistedPathBase(value)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func piPersistedPathBase(value string) string {
	return filepath.Base(strings.ReplaceAll(value, `\`, "/"))
}

func (read piHeaderLookup) primeParentIDWithStatus(childPath, persistedPath string) (string, bool) {
	base := piPersistedPathBase(persistedPath)
	localSibling := filepath.Join(filepath.Dir(childPath), base)
	// pi-subagents names every child session.jsonl, so a nested child's parent
	// basename can match the child's own file. A session is never its own parent.
	if filepath.Clean(localSibling) != filepath.Clean(childPath) {
		if headerID, ok := read.id(localSibling); ok && headerID != "" {
			return headerID, true
		}
	}
	if filepath.IsAbs(persistedPath) &&
		filepath.Clean(persistedPath) != filepath.Clean(localSibling) {
		if headerID, ok := read.id(persistedPath); ok && headerID != "" {
			return headerID, true
		}
	}
	return strings.TrimSuffix(base, filepath.Ext(base)), false
}

// parsePiUserMessage parses a message entry with role="user".
// Returns nil if the entry is malformed.
func parsePiUserMessage(
	line string, ordinal int, sourceUUID, sourceParentUUID string,
) *ParsedMessage {
	content := gjson.Get(line, "message.content")

	var text string
	if content.Type == gjson.String {
		text = content.Str
	} else if content.IsArray() {
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").Str == "text" {
				if t := block.Get("text").Str; t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		text = strings.Join(parts, "\n")
	}

	ts := piTimestamp(line)

	return &ParsedMessage{
		Ordinal:          ordinal,
		Role:             RoleUser,
		SourceType:       "user",
		SourceUUID:       sourceUUID,
		SourceParentUUID: sourceParentUUID,
		Content:          text,
		Timestamp:        ts,
		ContentLength:    len(text),
	}
}

// parsePiAssistantMessage parses a message entry with role="assistant".
// Returns nil if the entry is malformed. fallbackModel is the most
// recent model id seen (from a prior assistant message or
// model_change entry), used when this message has no inline model.
// fallbackEffort is the most recent thinking_level_change value, used
// when this message has no inline thinkingLevel.
func parsePiAssistantMessage(
	ctx context.Context,
	line string, ordinal int, fallbackModel, fallbackEffort, sourceUUID,
	sourceParentUUID, sessionCwd string,
) *ParsedMessage {
	var (
		parts         []string
		dialogue      []string
		thinkingParts []string
		toolCalls     []ParsedToolCall
		hasThinking   bool
		hasToolUse    bool
	)

	msgContent := gjson.Get(line, "message.content")
	if msgContent.Type == gjson.String {
		// Plain string content (back-compat format variation).
		parts = append(parts, msgContent.Str)
		dialogue = append(dialogue, msgContent.Str)
	} else {
		msgContent.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").Str {
			case "text":
				if t := block.Get("text").Str; t != "" {
					parts = append(parts, t)
					dialogue = append(dialogue, t)
				}
			case "thinking":
				// Set hasThinking regardless of whether the thinking
				// field is empty -- redacted thinking blocks have an
				// empty field but the block type presence is sufficient
				// to mark the message.
				hasThinking = true
				if thinking := block.Get("thinking").Str; thinking != "" {
					thinkingParts = append(thinkingParts, thinking)
					parts = append(parts, "[Thinking]\n"+thinking+"\n[/Thinking]")
				}
			case "toolCall":
				hasToolUse = true
				id := block.Get("id").Str
				name := block.Get("name").Str
				argsRaw := block.Get("arguments").Raw
				// Normalize Pi's agent__intent / _i field to
				// "description" so the frontend can use a single
				// params.description check across all agents.
				argsRaw = normalizePiIntent(argsRaw)
				rendering := formatPiToolUse(name, argsRaw)
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: id,
					ToolName:  name,
					Category:  NormalizeToolCategory(name),
					InputJSON: argsRaw,
					SkillName: inferPiSkillName(
						ctx, name, argsRaw, sessionCwd,
					),
					Rendering: rendering,
				})
				parts = append(parts, rendering)
			}
			return true
		})
	}

	content := strings.Join(parts, "\n")
	ts := piTimestamp(line)

	pm := &ParsedMessage{
		Ordinal:          ordinal,
		Role:             RoleAssistant,
		SourceType:       "assistant",
		SourceUUID:       sourceUUID,
		SourceParentUUID: sourceParentUUID,
		Content:          content,
		DialogueText:     new(strings.Join(dialogue, "\n")),
		ThinkingText:     strings.Join(thinkingParts, "\n\n"),
		Timestamp:        ts,
		HasThinking:      hasThinking,
		HasToolUse:       hasToolUse,
		ContentLength:    len(content),
		ToolCalls:        toolCalls,
	}
	applyPiTokenUsage(pm, line, fallbackModel)
	applyPiReasoningEffort(pm, line, fallbackEffort)
	return pm
}

// inferPiSkillName attributes a Pi tool call to a skill when the call
// is a skill load. Pi has no dedicated skill tool: a native skill load
// is emitted as an ordinary `read` tool call whose path argument points
// at the skill's SKILL.md (Pi resolves skills from many roots:
// ~/.pi/agent/skills, project .pi/skills, .agents/skills, package
// skills/, --skill <path>), or, in newer OMP builds, at a
// skill://<name> URI in that read path. Without attribution these calls
// inflate the Read tool count and leave the Skills dimension empty.
// Relative SKILL.md paths resolve against the session working directory
// (sessionCwd).
func inferPiSkillName(ctx context.Context, toolName, inputJSON, sessionCwd string) string {
	if isCursorSkillReadTool(toolName) {
		// Pi's read input carries no cwd/workdir key, so
		// inferSkillNameFromJSONPaths can't resolve relative SKILL.md
		// paths; try the path keys directly against the session working
		// directory first, mirroring the OpenCode parser.
		for _, key := range []string{"path", "file_path"} {
			fp := gjson.Get(inputJSON, key).Str
			if skill, ok := piSkillURISkillName(fp); ok {
				return skill
			}
			if fp != "" && sessionCwd != "" {
				if name := skillNameFromPath(ctx, fp, sessionCwd); name != "" {
					return name
				}
			}
		}
		return inferSkillNameFromJSONPaths(ctx, inputJSON)
	}
	return inferCodexSkillNameWithBase(ctx, toolName, inputJSON, sessionCwd)
}

// piSkillURISkillName extracts the decoded skill name from the
// skill://<name> URI used as a Pi-family read path. ok is false when
// path is not a valid skill URI, leaving attribution to the SKILL.md
// path heuristic.
func piSkillURISkillName(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "skill://") {
		return "", false
	}
	rest := path[len("skill://"):]
	if end := strings.IndexAny(rest, "/?# \t\n\r"); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		return "", false
	}
	name, err := url.PathUnescape(rest)
	if err != nil || name == "" {
		return "", false
	}
	return name, true
}

// applyPiTokenUsage extracts the assistant message's model and
// per-message token counts from a Pi JSONL line. Pi records
// usage as a flat object under message.usage with provider-
// agnostic input/output keys plus optional cache breakdowns.
// Cache fields are read from both the nested cache.{read,write}
// shape (OpenCode-style) and the flat cacheRead/cacheCreation/cacheWrite
// shapes used across Pi-family producers.
//
// Coverage semantics match the claude parser contract: a field
// present at zero is preserved as "known zero" and sets its
// coverage flag, while a usage object with no recognized
// fields (empty `{}` or a foreign schema) leaves TokenUsage
// empty so the usage query filter skips the row.
func applyPiTokenUsage(
	pm *ParsedMessage, line, fallbackModel string,
) {
	if model := gjson.Get(line, "message.model").Str; model != "" {
		pm.Model = model
	} else if fallbackModel != "" {
		pm.Model = fallbackModel
	}

	usage := gjson.Get(line, "message.usage")
	applyPiUsage(pm, usage)
}

// applyPiReasoningEffort records the thinking level Pi requested for an
// assistant response. Newer Pi versions write it on the message as
// thinkingLevel; older ones only append thinking_level_change entries, so
// the most recent entry value fills in. The value is stored as Pi wrote it.
func applyPiReasoningEffort(pm *ParsedMessage, line, fallback string) {
	if level := gjson.Get(line, "message.thinkingLevel").Str; level != "" {
		pm.ReasoningEffort = level
	} else {
		pm.ReasoningEffort = fallback
	}
}

func applyPiUsage(pm *ParsedMessage, usage gjson.Result) {
	if !usage.Exists() {
		return
	}

	inputField := usage.Get("input")
	outputField := usage.Get("output")
	cacheReadField := usage.Get("cache.read")
	if !cacheReadField.Exists() {
		cacheReadField = usage.Get("cacheRead")
	}
	cacheWriteField := usage.Get("cache.write")
	if !cacheWriteField.Exists() {
		cacheWriteField = usage.Get("cacheCreation")
	}
	if !cacheWriteField.Exists() {
		cacheWriteField = usage.Get("cacheWrite")
	}

	if !inputField.Exists() && !outputField.Exists() &&
		!cacheReadField.Exists() && !cacheWriteField.Exists() {
		return
	}

	input := int(inputField.Int())
	output := int(outputField.Int())
	cacheRead := int(cacheReadField.Int())
	cacheCreate := int(cacheWriteField.Int())

	normalized := map[string]int{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheCreate,
	}
	j, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	pm.TokenUsage = j
	pm.OutputTokens = output
	pm.HasOutputTokens = outputField.Exists()
	pm.ContextTokens = input + cacheRead + cacheCreate
	pm.HasContextTokens = inputField.Exists() ||
		cacheReadField.Exists() || cacheWriteField.Exists()
}

// parsePiToolResultMessage parses a message entry with role="toolResult".
// Returns nil if the entry is malformed.
func parsePiToolResultMessage(
	line string, ordinal int, sourceUUID, sourceParentUUID string,
) *ParsedMessage {
	toolUseID := gjson.Get(line, "message.toolCallId").Str
	content := gjson.Get(line, "message.content")
	contentLen := toolResultContentLength(content)

	ts := piTimestamp(line)

	return &ParsedMessage{
		Ordinal:          ordinal,
		Role:             RoleUser,
		SourceType:       "toolResult",
		SourceUUID:       sourceUUID,
		SourceParentUUID: sourceParentUUID,
		Timestamp:        ts,
		ToolResults: []ParsedToolResult{
			{
				ToolUseID:     toolUseID,
				ContentLength: contentLen,
				ContentRaw:    content.Raw,
			},
		},
	}
}

func parsePiCompactionMessage(
	line string, ordinal int, sourceUUID, sourceParentUUID string,
) *ParsedMessage {
	summary := gjson.Get(line, "summary").Str
	ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
	return &ParsedMessage{
		Ordinal:           ordinal,
		Role:              RoleAssistant,
		Content:           summary,
		Timestamp:         ts,
		IsSystem:          true,
		ContentLength:     len(summary),
		SourceType:        "system",
		SourceSubtype:     "compact_boundary",
		SourceUUID:        sourceUUID,
		SourceParentUUID:  sourceParentUUID,
		IsCompactBoundary: true,
	}
}

// formatPiToolUse constructs a synthetic block with "input" mapped from
// Pi's "arguments" field and delegates to formatToolUse. This avoids
// duplicating the tool-name switch logic.
func formatPiToolUse(name, argsRaw string) string {
	// Build {"name":"<name>","input":<args>} so formatToolUse can
	// read input.* paths as usual.
	var sb strings.Builder
	sb.WriteString(`{"name":`)
	nameJSON, _ := json.Marshal(name)
	sb.Write(nameJSON)
	sb.WriteString(`,"input":`)
	if argsRaw == "" {
		sb.WriteString("{}")
	} else {
		sb.WriteString(argsRaw)
	}
	sb.WriteByte('}')
	return formatToolUse(gjson.Parse(sb.String()))
}

// normalizePiIntent rewrites Pi's agent__intent or _i argument field to
// "description" so the frontend can use a uniform params.description check
// across all agents. Returns the original JSON unchanged if neither field
// is present or if "description" already exists.
func normalizePiIntent(argsRaw string) string {
	if argsRaw == "" {
		return argsRaw
	}
	// Don't overwrite an existing description field.
	if gjson.Get(argsRaw, "description").Exists() {
		return argsRaw
	}
	intent := gjson.Get(argsRaw, "agent__intent")
	if !intent.Exists() {
		intent = gjson.Get(argsRaw, "_i")
	}
	if !intent.Exists() {
		return argsRaw
	}
	// Unmarshal into a map, rename the intent key to "description",
	// and re-marshal to produce valid JSON with proper escaping.
	var m map[string]jsontext.Value
	if err := json.Unmarshal([]byte(argsRaw), &m); err != nil {
		return argsRaw
	}
	if v, ok := m["agent__intent"]; ok {
		m["description"] = v
	} else if v, ok := m["_i"]; ok {
		m["description"] = v
	} else {
		return argsRaw
	}
	delete(m, "agent__intent")
	delete(m, "_i")
	out, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return argsRaw
	}
	return string(out)
}

// piTimestamp extracts the timestamp for a pi JSONL entry.
// Tries the top-level "timestamp" field first (ISO 8601), then
// falls back to "message.timestamp" as Unix milliseconds.
func piTimestamp(line string) time.Time {
	if ts := parseTimestamp(gjson.Get(line, "timestamp").Str); !ts.IsZero() {
		return ts
	}
	if ms := gjson.Get(line, "message.timestamp").Int(); ms != 0 {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

// ompParentID preserves raw parent IDs and resolves path references
// through transcript headers. A stale path can use a matching local structural
// parent or sibling, but must not borrow an unrelated directory parent.
func (read piHeaderLookup) ompParentID(childPath, parentReference, childID, structuralParentID string) string {
	if !strings.ContainsAny(parentReference, `/\`) &&
		!strings.HasSuffix(parentReference, ".jsonl") {
		return parentReference
	}
	if childID == "" {
		childID = strings.TrimSuffix(filepath.Base(childPath), ".jsonl")
	}

	parentPath := filepath.FromSlash(strings.ReplaceAll(parentReference, `\`, "/"))
	if !filepath.IsAbs(parentPath) {
		parentPath = filepath.Join(filepath.Dir(childPath), parentPath)
	}
	parentPath = filepath.Clean(parentPath)
	structuralParentPath := filepath.Dir(childPath) + ".jsonl"
	structuralParentPath = filepath.Clean(structuralParentPath)
	if parentPath != structuralParentPath {
		if parentID := read.transcriptID(parentPath, true); parentID != "" && parentID != childID {
			return parentID
		}
	} else if structuralParentID != "" && structuralParentID != childID {
		// An exact structural path remains authoritative over a similarly
		// named sibling in the child's directory.
		return structuralParentID
	}

	base := piPersistedPathBase(parentReference)
	localSibling := filepath.Join(filepath.Dir(childPath), base)
	if filepath.Clean(localSibling) != filepath.Clean(parentPath) &&
		filepath.Clean(localSibling) != filepath.Clean(structuralParentPath) {
		if parentID := read.transcriptID(localSibling, true); parentID != "" && parentID != childID {
			return parentID
		}
	}
	// OMP's readable structural parent can recover a stale path with the same
	// basename, but a readable exact sibling above takes precedence.
	if structuralParentID != "" && structuralParentID != childID &&
		strings.EqualFold(base, filepath.Base(structuralParentPath)) {
		return structuralParentID
	}
	return ""
}

// subagentParentID returns the parent Pi session's stored raw ID for a
// pi-subagents child at <parent>/<runId>/run-N/session.jsonl, or "" when
// childPath does not have that shape or no Pi transcript sits at
// <parent>.jsonl. A delegated child's own children resolve to that child,
// because the extension nests them below its session.jsonl stem.
func (read piHeaderLookup) subagentParentID(childPath string) string {
	if !isPiSubagentPath(childPath) {
		return ""
	}
	return read.transcriptID(piSubagentParentPath(childPath), false)
}

func isPiSubagentPath(childPath string) bool {
	return filepath.Base(childPath) == "session.jsonl" &&
		isPiSubagentRunDir(filepath.Base(filepath.Dir(childPath)))
}

func piSubagentParentPath(childPath string) string {
	runDir := filepath.Dir(childPath)
	return filepath.Dir(filepath.Dir(runDir)) + ".jsonl"
}

// isPiSubagentRunDir reports whether name is a pi-subagents attempt directory
// (run-0, run-1, ...).
func isPiSubagentRunDir(name string) bool {
	n, ok := strings.CutPrefix(name, "run-")
	if !ok || n == "" {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// piSessionHeaderID reads path's session header id, skipping a leading OMP
// title slot line. The boolean reports whether the file has a valid pi session
// header. os.Open intentionally follows symlinks to supported transcripts.
func piSessionHeaderID(path string) (string, bool) {
	header, ok := piSessionHeader(path)
	return gjson.Get(header, "id").Str, ok
}

func piSessionHeader(path string) (string, bool) {
	header, ok, _ := readPiSessionHeader(path)
	return header, ok
}

func readPiSessionHeader(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	return scanPiSessionHeader(f)
}

func scanPiSessionHeader(reader io.Reader) (string, bool, error) {
	lr := newLineReader(reader, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			return "", false, lr.Err()
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if gjson.Get(line, "type").Str == "title" {
			continue
		}
		if gjson.Get(line, "type").Str != "session" {
			return "", false, nil
		}
		return line, true, nil
	}
}

// resolvePiLineage shares the exact parent lookup order between parsing and capture.
func resolvePiLineage(path, headerLine string, agent AgentType, idPrefix string, read piHeaderLookup, resolve func(string) (string, bool)) (string, bool, bool) {
	sessionID := gjson.Get(headerLine, "id").Str
	// OMP's directory layout identifies subagents independently of whether
	// their headers also carry lineage.
	var ompParentID string
	if agent == AgentOMP {
		ompParentID = read.transcriptID(filepath.Dir(path)+".jsonl", true)
	}
	isOMPSubagent := ompParentID != ""

	// branchedFrom keeps its existing filename identity and precedence.
	// OMP parentSession may be an opaque raw ID or a transcript path;
	// other Pi-family providers retain their existing path resolution.
	var parentSessionID string
	var parentSessionResolved bool
	var parentSessionPath bool
	var parentSessionBase string
	if branchedFrom := gjson.Get(headerLine, "branchedFrom").Str; branchedFrom != "" {
		parentSessionID = idPrefix + piPersistedPathSessionID(branchedFrom)
	} else if parentSession := gjson.Get(headerLine, "parentSession").Str; parentSession != "" &&
		(agent == AgentPi || agent == AgentOMP || agent == AgentPrimeAgent ||
			agent == AgentOMO || agent == AgentStepCode) {
		// Client aliases must resolve before host-specific absolute-path checks.
		if resolve != nil {
			if local, ok := resolvePiStoredPath(resolve, parentSession); ok {
				parentSession = local
			}
		}
		if agent == AgentOMP {
			if parentID := read.ompParentID(path, parentSession, sessionID, ompParentID); parentID != "" {
				parentSessionID = idPrefix + parentID
			}
		} else {
			parentSessionBase = piPersistedPathBase(parentSession)
			parentSessionPath = strings.ContainsAny(parentSession, `/\\`) ||
				strings.HasSuffix(parentSession, ".jsonl")
			parentID, resolved := read.primeParentIDWithStatus(path, parentSession)
			parentSessionID = idPrefix + parentID
			parentSessionResolved = resolved
		}
	} else if ompParentID != "" {
		// Older OMP children omit parentSession entirely.
		parentSessionID = idPrefix + ompParentID
	}

	// The pi-subagents extension writes a fresh child to
	// <project>/<parent>/<runId>/run-N/session.jsonl. Older releases leave
	// parentSession out of the header, so lineage comes from the parent
	// transcript three levels up. Newer releases record parentSession, the same
	// field /fork uses, so the run directory is what marks the child as a
	// subagent. The header's parent wins when both are present.
	var isPiSubagent bool
	if agent == AgentPi || agent == AgentOMO {
		if parentID := read.subagentParentID(path); parentID != "" {
			isPiSubagent = true
			structuralParentBase := filepath.Base(piSubagentParentPath(path))
			if parentSessionID == "" ||
				(parentSessionPath && !parentSessionResolved &&
					strings.EqualFold(parentSessionBase, structuralParentBase)) {
				parentSessionID = idPrefix + parentID
			}
		}
	}

	return parentSessionID, isOMPSubagent, isPiSubagent
}

// piHeaderLookup can confine reads or record physical capture dependencies.
type piHeaderLookup func(string) (string, bool)

func (read piHeaderLookup) id(path string) (string, bool) {
	header, ok := read(path)
	return gjson.Get(header, "id").Str, ok
}

func (read piHeaderLookup) transcriptID(path string, strict bool) string {
	header, ok := read(path)
	if !ok || strict && !gjson.Valid(header) {
		return ""
	}
	if id := gjson.Get(header, "id").Str; id != "" {
		return id
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
