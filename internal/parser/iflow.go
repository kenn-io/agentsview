// ABOUTME: Parses iFlow JSONL session files into structured session data.
// iFlow uses a similar format to Claude Code with uuid/parentUuid structure.
package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
)

// dagEntryIflow holds metadata for a single JSONL entry participating
// in the uuid/parentUuid DAG.
type dagEntryIflow struct {
	uuid       string
	parentUuid string
	entryType  string // "user" or "assistant"
	lineIndex  int
	line       string
	timestamp  time.Time
}

// ParseIflowSession parses an iFlow JSONL session file.
// Returns a single ParseResult. Unlike Claude, iFlow's
// uuid/parentUuid DAG represents streaming incremental updates
// (sliding-window snapshots), not conversation forks, so fork
// splitting is intentionally not applied.
func ParseIflowSession(
	path, project, machine string,
) ([]ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// Extract session ID from filename: session-<uuid>.jsonl
	filename := filepath.Base(path)
	sessionID := strings.TrimSuffix(filename, ".jsonl")
	if trimmed, ok := strings.CutPrefix(sessionID, "session-"); ok {
		sessionID = trimmed
	}
	// Normalize iFlow IDs with namespace prefix
	sessionID = "iflow:" + sessionID

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// First pass: collect all valid lines with metadata.
	var (
		entries         = make([]dagEntryIflow, 0)
		hasAnyUUID      bool
		allHaveUUID     bool
		parentSessionID string
		foundParentSID  bool
		fileLineNum     int // counts every valid JSON line
		subagentMap     = map[string]string{}
		globalStart     time.Time
		globalEnd       time.Time
	)
	allHaveUUID = true

	lr := newLineReader(f, maxLineSize)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		entryType := gjson.Get(line, "type").Str
		fileLineNum++

		// Track global timestamps from all lines for session
		// bounds, including non-message events.
		if ts := extractTimestampIflow(line); !ts.IsZero() {
			if globalStart.IsZero() || ts.Before(globalStart) {
				globalStart = ts
			}
			if ts.After(globalEnd) {
				globalEnd = ts
			}
		}

		if entryType != "user" && entryType != "assistant" {
			continue
		}

		// Track parentSessionID from first user/assistant entry.
		if !foundParentSID {
			if sid := gjson.Get(line, "sessionId").Str; sid != "" {
				foundParentSID = true
				// iFlow sessionId is the full session filename (e.g., "session-uuid")
				// Extract the ID by trimming "session-" prefix and compare with sessionID
				sidID := strings.TrimPrefix(sid, "session-")
				// Compare with the raw UUID (without iflow: prefix)
				rawSessionID := strings.TrimPrefix(sessionID, "iflow:")
				if sidID != rawSessionID {
					parentSessionID = "iflow:" + sidID
				}
			}
		}

		uuid := gjson.Get(line, "uuid").Str
		parentUuid := gjson.Get(line, "parentUuid").Str

		if uuid != "" {
			hasAnyUUID = true
		} else {
			allHaveUUID = false
		}

		ts := extractTimestampIflow(line)

		entries = append(entries, dagEntryIflow{
			uuid:       uuid,
			parentUuid: parentUuid,
			entryType:  entryType,
			lineIndex:  fileLineNum,
			line:       line,
			timestamp:  ts,
		})
	}

	if err := lr.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	fileInfo := FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	// iFlow's DAG represents streaming incremental updates,
	// not conversation forks. Deduplicate assistant siblings
	// (keep the last one per parentUuid group) then parse
	// linearly.
	if hasAnyUUID && allHaveUUID {
		entries = deduplicateIflowEntries(entries)
	}

	return parseLinearIflow(
		entries, sessionID, project, machine,
		parentSessionID, fileInfo, subagentMap,
		globalStart, globalEnd,
	)
}

// parseLinearIflow processes entries sequentially without DAG awareness.
func parseLinearIflow(
	entries []dagEntryIflow,
	sessionID, project, machine, parentSessionID string,
	fileInfo FileInfo,
	subagentMap map[string]string,
	globalStart, globalEnd time.Time,
) ([]ParseResult, error) {
	messages, startedAt, endedAt := extractMessagesIflow(entries)
	startedAt = earlierTime(globalStart, startedAt)
	endedAt = laterTime(globalEnd, endedAt)

	userCount := 0
	firstMsg := ""
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
			if firstMsg == "" {
				firstMsg = truncate(
					strings.ReplaceAll(m.Content, "\n", " "), 300,
				)
			}
		}
	}

	sess := ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentIflow,
		ParentSessionID:  parentSessionID,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File:             fileInfo,
	}

	return []ParseResult{{Session: sess, Messages: messages}}, nil
}

// iflowStreamingGap is the maximum time gap between two
// assistant entries to consider them part of the same
// streaming burst. iFlow emits incremental updates within
// milliseconds of each other; a gap >1s signals a new turn.
const iflowStreamingGap = time.Second

// deduplicateIflowEntries merges redundant assistant streaming
// updates. iFlow emits multiple assistant entries under the same
// parentUuid as sliding-window snapshots within a streaming burst
// (<1s apart). Each snapshot shows currently-active tool calls,
// so adjacent entries share overlapping tool_use IDs while text
// blocks appear only in the first snapshot. This function merges
// each burst into a single entry with all unique content blocks.
// User entries are always kept because they carry distinct data.
func deduplicateIflowEntries(
	entries []dagEntryIflow,
) []dagEntryIflow {
	// Group strictly adjacent assistant entries by parentUuid.
	// Adjacency is based on lineIndex (the original JSONL file
	// position), so non-user/assistant events that were filtered
	// out before this function still break runs.
	type assistantRun struct {
		indices []int
	}
	groups := map[string][]assistantRun{} // parentUuid -> runs
	for i, e := range entries {
		if e.entryType != "assistant" || e.parentUuid == "" {
			continue
		}
		runs, ok := groups[e.parentUuid]
		if !ok {
			runs = []assistantRun{}
		}
		canExtend := false
		if len(runs) > 0 {
			last := &runs[len(runs)-1]
			prevIdx := last.indices[len(last.indices)-1]
			prev := entries[prevIdx]
			adjacent := prev.lineIndex == e.lineIndex-1
			sameGap := !e.timestamp.IsZero() &&
				!prev.timestamp.IsZero() &&
				e.timestamp.Sub(prev.timestamp) < iflowStreamingGap
			canExtend = adjacent && sameGap
		}
		if canExtend {
			last := &runs[len(runs)-1]
			last.indices = append(last.indices, i)
			groups[e.parentUuid] = runs
			continue
		}
		groups[e.parentUuid] = append(runs, assistantRun{
			indices: []int{i},
		})
	}

	// For multi-entry runs, merge content blocks into the
	// last entry and mark earlier entries for removal.
	merged := map[int]string{} // last index -> merged line
	drop := map[int]bool{}
	for _, runs := range groups {
		for _, run := range runs {
			if len(run.indices) <= 1 {
				continue
			}
			for _, idx := range run.indices[:len(run.indices)-1] {
				drop[idx] = true
			}
			lastIdx := run.indices[len(run.indices)-1]
			merged[lastIdx] = mergeIflowBurst(
				entries, run.indices,
			)
		}
	}

	result := make([]dagEntryIflow, 0, len(entries)-len(drop))
	for i, e := range entries {
		if drop[i] {
			continue
		}
		if line, ok := merged[i]; ok {
			e.line = line
		}
		result = append(result, e)
	}
	return result
}

// mergeIflowBurst combines content blocks from all entries in a
// streaming burst into a single JSON line. Tool-use blocks are
// deduplicated by their "id" field (keeping first occurrence);
// text and thinking blocks are always retained.
func mergeIflowBurst(
	entries []dagEntryIflow, indices []int,
) string {
	if len(indices) == 0 {
		return ""
	}

	var blocks []string
	seenToolUse := map[string]bool{}

	for _, idx := range indices {
		content := gjson.Get(entries[idx].line, "message.content")
		if !content.IsArray() {
			continue
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").Str == "tool_use" {
				id := block.Get("id").Str
				if id != "" && seenToolUse[id] {
					return true
				}
				if id != "" {
					seenToolUse[id] = true
				}
			}
			blocks = append(blocks, block.Raw)
			return true
		})
	}

	merged := "[" + strings.Join(blocks, ",") + "]"

	// Splice merged content into the last entry's JSON line.
	lastLine := entries[indices[len(indices)-1]].line
	cr := gjson.Get(lastLine, "message.content")
	if cr.Index > 0 {
		end := cr.Index + len(cr.Raw)
		return lastLine[:cr.Index] + merged + lastLine[end:]
	}
	return lastLine
}

// extractMessagesIflow converts dagEntryIflow into ParsedMessages, applying
// the same filtering and content extraction as the original linear
// parser.
func extractMessagesIflow(entries []dagEntryIflow) (
	[]ParsedMessage, time.Time, time.Time,
) {
	var (
		messages  []ParsedMessage
		startedAt time.Time
		endedAt   time.Time
		ordinal   int
	)

	for _, e := range entries {
		if !e.timestamp.IsZero() {
			if startedAt.IsZero() {
				startedAt = e.timestamp
			}
			endedAt = e.timestamp
		}

		// Tier 1: skip system-injected user entries.
		if e.entryType == "user" {
			if gjson.Get(e.line, "isMeta").Bool() ||
				gjson.Get(e.line, "isCompactSummary").Bool() {
				continue
			}
		}

		content := gjson.Get(e.line, "message.content")
		text, _, hasThinking, hasToolUse, tcs, trs :=
			ExtractTextContent(content)

		// Convert command/skill invocation XML into readable
		// text (e.g. "/roborev-fix 450"). If the content
		// looks like a command envelope but can't be
		// normalized, skip it to avoid raw XML in transcripts.
		if e.entryType == "user" {
			if cmdText, ok := extractCommandText(text); ok {
				text = cmdText
			} else if isCommandEnvelope(text) {
				continue
			}
		}

		if strings.TrimSpace(text) == "" && len(trs) == 0 && len(tcs) == 0 {
			continue
		}

		// Tier 2: skip known system-injected patterns.
		if e.entryType == "user" && isIflowSystemMessage(text) {
			continue
		}

		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleType(e.entryType),
			Content:       text,
			Timestamp:     e.timestamp,
			HasThinking:   hasThinking,
			HasToolUse:    hasToolUse,
			ContentLength: len(text),
			ToolCalls:     tcs,
			ToolResults:   trs,
		})
		ordinal++
	}

	return messages, startedAt, endedAt
}

// extractTimestampIflow parses the timestamp from a JSONL line.
func extractTimestampIflow(line string) time.Time {
	tsStr := gjson.Get(line, "timestamp").Str
	return parseTimestamp(tsStr)
}

// ExtractIflowProjectHints reads project-identifying metadata
// from an iFlow JSONL session file.
func ExtractIflowProjectHints(
	path string,
) (cwd, gitBranch string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		if gjson.Get(line, "type").Str == "user" {
			// Skip meta/system-injected user entries
			if gjson.Get(line, "isMeta").Bool() ||
				gjson.Get(line, "isCompactSummary").Bool() {
				continue
			}

			if cwd == "" {
				cwd = gjson.Get(line, "cwd").Str
			}
			if gitBranch == "" {
				gitBranch = gjson.Get(line, "gitBranch").Str
			}

			if cwd != "" && gitBranch != "" {
				return cwd, gitBranch
			}
		}
	}
	if err := lr.Err(); err != nil {
		logParseError(fmt.Sprintf("reading hints from %s: %v", path, err))
	}
	return cwd, gitBranch
}

// isIflowSystemMessage returns true if the content matches
// a known system-injected user message pattern.
func isIflowSystemMessage(content string) bool {
	trimmed := strings.TrimLeftFunc(content, func(r rune) bool {
		return r == '\uFEFF' || unicode.IsSpace(r)
	})
	prefixes := [...]string{
		"This session is being continued",
		"[Request interrupted",
		"<task-notification>",
		"<local-command-",
		"Stop hook feedback:",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}
