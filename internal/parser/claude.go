// ABOUTME: Parses Claude Code JSONL session files into structured session data.
// ABOUTME: Detects DAG forks in uuid/parentUuid trees and splits large-gap forks into separate sessions.
package parser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

var (
	xmlTaskIDRe   = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	xmlToolUseRe  = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	xmlCmdNameRe  = regexp.MustCompile(`<command-name>([^<]+)</command-name>`)
	xmlCmdMsgRe   = regexp.MustCompile(`<command-message>([^<]+)</command-message>`)
	xmlCmdArgsRe  = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
	xmlCmdStripRe = regexp.MustCompile(`<command-(?:name|message|args)>[^<]*</command-(?:name|message|args)>`)
)

const (
	initialScanBufSize = 64 * 1024        // 64KB
	maxLineSize        = 64 * 1024 * 1024 // 64MB
	forkThreshold      = 3
)

// dagEntry holds metadata for a single JSONL entry participating
// in the uuid/parentUuid DAG.
type dagEntry struct {
	uuid       string
	parentUuid string
	entryType  string // "user" or "assistant"
	lineIndex  int
	line       string
	timestamp  time.Time
}

// ParseClaudeSession parses a Claude Code JSONL session file.
// Returns one or more ParseResult structs (multiple when forks
// are detected in the uuid/parentUuid DAG).
func ParseClaudeSession(
	path, project, machine string,
) ([]ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// First pass: collect all valid lines with metadata.
	var (
		entries         = make([]dagEntry, 0)
		hasAnyUUID      bool
		allHaveUUID     bool
		parentSessionID string
		sourceSessionID string
		sourceVersion   string
		cwd             string
		gitBranch       string
		foundParentSID  bool
		lineIndex       int
		malformedLines  int
		lastLine        string
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
		lastLine = line
		if !gjson.Valid(line) {
			malformedLines++
			continue
		}

		entryType := gjson.Get(line, "type").Str

		// Extract source version from first line that has it.
		if sourceVersion == "" {
			if v := gjson.Get(line, "version").Str; v != "" {
				sourceVersion = v
			}
		}

		// Track global timestamps from all lines for session
		// bounds, including non-message events.
		if ts := extractTimestamp(line); !ts.IsZero() {
			if globalStart.IsZero() || ts.Before(globalStart) {
				globalStart = ts
			}
			if ts.After(globalEnd) {
				globalEnd = ts
			}
		}

		// Collect queue-operation enqueue entries for subagent mapping.
		if entryType == "queue-operation" {
			if gjson.Get(line, "operation").Str == "enqueue" {
				contentStr := gjson.Get(line, "content").Str
				if contentStr != "" {
					tuid := gjson.Get(contentStr, "tool_use_id").Str
					taskID := gjson.Get(contentStr, "task_id").Str
					if tuid == "" || taskID == "" {
						// Fallback: extract from XML <task-id> and <tool-use-id> tags.
						if m := xmlTaskIDRe.FindStringSubmatch(contentStr); m != nil {
							taskID = m[1]
						}
						if m := xmlToolUseRe.FindStringSubmatch(contentStr); m != nil {
							tuid = m[1]
						}
					}
					if tuid != "" && taskID != "" {
						subagentMap[tuid] = "agent-" + taskID
					}
				}
			}
			continue
		}

		// Collect agent_progress events for subagent mapping.
		// Claude Code v2.1+ emits these instead of queue-operation for Agent tool calls.
		if entryType == "progress" {
			if gjson.Get(line, "data.type").Str == "agent_progress" {
				tuid := gjson.Get(line, "parentToolUseID").Str
				agentID := gjson.Get(line, "data.agentId").Str
				if tuid != "" && agentID != "" {
					subagentMap[tuid] = "agent-" + agentID
				}
			}
			continue
		}

		if entryType != "user" && entryType != "assistant" {
			continue
		}

		// Extract cwd and gitBranch from user entries.
		if entryType == "user" {
			if cwd == "" {
				cwd = gjson.Get(line, "cwd").Str
			}
			if gitBranch == "" {
				gitBranch = gjson.Get(line, "gitBranch").Str
			}
		}

		// Capture sourceSessionID from first sessionId seen,
		// then check whether it differs from the file-derived
		// ID to detect parent sessions.
		if !foundParentSID {
			if sid := gjson.Get(line, "sessionId").Str; sid != "" {
				foundParentSID = true
				sourceSessionID = sid
				if sid != sessionID {
					parentSessionID = sid
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

		ts := extractTimestamp(line)

		entries = append(entries, dagEntry{
			uuid:       uuid,
			parentUuid: parentUuid,
			entryType:  entryType,
			lineIndex:  lineIndex,
			line:       line,
			timestamp:  ts,
		})
		lineIndex++
	}

	if err := lr.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Detect truncation: last line is non-empty, invalid JSON,
	// AND the file did not end with a newline. A newline-
	// terminated invalid line is just a complete malformed
	// record, not a truncated write.
	isTruncated := lastLine != "" &&
		strings.TrimSpace(lastLine) != "" &&
		!gjson.Valid(lastLine) &&
		!fileEndsWithNewline(f, info.Size())

	// Collapse consecutive assistant entries that share the same
	// message.id — these are streaming progress snapshots of a
	// single response. Keep only the last entry per message.id
	// run (it has the final content and token counts).
	entries = collapseStreamingDuplicates(entries)

	fileInfo := FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	meta := claudeSessionMeta{
		sourceSessionID: sourceSessionID,
		sourceVersion:   sourceVersion,
		cwd:             cwd,
		gitBranch:       gitBranch,
		malformedLines:  malformedLines,
		isTruncated:     isTruncated,
	}

	// If all user/assistant entries have uuids, use DAG-aware processing.
	if hasAnyUUID && allHaveUUID {
		return parseDAG(
			entries, sessionID, project, machine,
			parentSessionID, fileInfo, subagentMap,
			globalStart, globalEnd, meta,
		)
	}

	// Fall back to linear processing.
	return parseLinear(
		entries, sessionID, project, machine,
		parentSessionID, fileInfo, subagentMap,
		globalStart, globalEnd, meta,
	)
}

// ParseClaudeSessionFrom parses only new lines from a Claude
// JSONL file starting at the given byte offset. Returns only
// the newly parsed messages (with ordinals starting at
// startOrdinal) and the latest timestamp. Fork detection is
// skipped — new entries are processed linearly. Used for
// incremental re-parsing of append-only session files.
// ErrDAGDetected is returned by ParseClaudeSessionFrom when
// appended lines contain uuid fields that require DAG-aware
// fork detection, which incremental parsing cannot handle.
var ErrDAGDetected = fmt.Errorf(
	"incremental parse: DAG uuid detected",
)

func ParseClaudeSessionFrom(
	path string,
	offset int64,
	startOrdinal int,
) ([]ParsedMessage, time.Time, int64, error) {
	var (
		entries   []dagEntry
		lineIndex = startOrdinal
		// Track latest timestamp from all lines, including
		// non-message events (progress, queue-operation) so
		// callers can update ended_at even when no new
		// messages are found.
		latestTS time.Time
	)

	consumed, err := readJSONLFrom(
		path, offset, func(line string) {
			if ts := extractTimestamp(line); !ts.IsZero() {
				if ts.After(latestTS) {
					latestTS = ts
				}
			}
			entryType := gjson.Get(line, "type").Str
			if entryType != "user" &&
				entryType != "assistant" {
				return
			}
			ts := extractTimestamp(line)
			entries = append(entries, dagEntry{
				uuid:       gjson.Get(line, "uuid").Str,
				parentUuid: gjson.Get(line, "parentUuid").Str,
				entryType:  entryType,
				lineIndex:  lineIndex,
				line:       line,
				timestamp:  ts,
			})
			lineIndex++
		},
	)
	if err != nil {
		return nil, time.Time{}, 0, fmt.Errorf(
			"reading claude %s from offset %d: %w",
			path, offset, err,
		)
	}

	if len(entries) == 0 {
		return nil, latestTS, consumed, nil
	}

	// Detect forks: if any entry's parentUuid doesn't
	// match the previous entry's uuid, the appended data
	// contains a branch that requires full DAG processing.
	if hasDAGFork(entries) {
		return nil, time.Time{}, 0, ErrDAGDetected
	}

	msgs, _, endedAt := extractMessagesFrom(
		entries, startOrdinal,
	)
	// Use the latest timestamp from all lines (including
	// non-message events) if it's later than what
	// extractMessagesFrom found.
	if latestTS.After(endedAt) {
		endedAt = latestTS
	}
	return msgs, endedAt, consumed, nil
}

// hasDAGFork returns true if the entries contain a fork —
// i.e. any entry whose parentUuid doesn't point to the
// immediately preceding entry's uuid. Linear UUID chains
// (each entry parenting the next) are safe for incremental
// parsing; forks require full DAG processing.
func hasDAGFork(entries []dagEntry) bool {
	var lastUUID string
	for _, e := range entries {
		if e.uuid == "" {
			continue // non-UUID entries are always linear
		}
		if lastUUID != "" &&
			e.parentUuid != lastUUID {
			return true
		}
		lastUUID = e.uuid
	}
	return false
}

// extractMessagesFrom is like extractMessages but uses a
// custom starting ordinal for incremental parsing.
func extractMessagesFrom(
	entries []dagEntry, startOrdinal int,
) ([]ParsedMessage, time.Time, time.Time) {
	var (
		messages  []ParsedMessage
		startedAt time.Time
		endedAt   time.Time
		ordinal   = startOrdinal
	)

	for _, e := range entries {
		if !e.timestamp.IsZero() {
			if startedAt.IsZero() {
				startedAt = e.timestamp
			}
			endedAt = e.timestamp
		}

		// Detect compact summaries before the user/assistant
		// gates: Claude can emit isCompactSummary=true with
		// either top-level type, and the record must always
		// be persisted as a system boundary regardless.
		if gjson.Get(e.line, "isCompactSummary").Bool() {
			summary := extractCompactSummary(e.line)
			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleAssistant,
				Content:           summary,
				Timestamp:         e.timestamp,
				IsSystem:          true,
				ContentLength:     len(summary),
				SourceType:        "system",
				SourceSubtype:     "compact_boundary",
				SourceUUID:        e.uuid,
				SourceParentUUID:  e.parentUuid,
				IsSidechain:       gjson.Get(e.line, "isSidechain").Bool(),
				IsCompactBoundary: true,
			})
			ordinal++
			continue
		}

		if e.entryType == "user" {
			if gjson.Get(e.line, "isMeta").Bool() {
				continue
			}
		}

		content := gjson.Get(e.line, "message.content")
		text, thinkingText, hasThinking, hasToolUse, tcs, trs :=
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

		if strings.TrimSpace(text) == "" && len(trs) == 0 {
			continue
		}

		if e.entryType == "user" {
			if subtype := ClassifyClaudeSystemMessage(text); subtype != "" {
				// Preserve Role=user so analytics that compute
				// turn-cycle/throughput on role alone (see
				// internal/db/analytics.go) don't count these as
				// assistant replies. is_system + source_subtype
				// let the UI and filters route them correctly.
				messages = append(messages, ParsedMessage{
					Ordinal:          ordinal,
					Role:             RoleUser,
					Content:          text,
					Timestamp:        e.timestamp,
					IsSystem:         true,
					ContentLength:    len(text),
					SourceType:       "system",
					SourceSubtype:    subtype,
					SourceUUID:       e.uuid,
					SourceParentUUID: e.parentUuid,
					IsSidechain:      gjson.Get(e.line, "isSidechain").Bool(),
				})
				ordinal++
				continue
			}
			// Skip unclassified noise (e.g. non-caveat
			// <local-command-*> envelopes).
			if isClaudeSystemMessage(text) {
				continue
			}
		}

		msg := ParsedMessage{
			Ordinal:            ordinal,
			Role:               RoleType(e.entryType),
			Content:            text,
			ThinkingText:       thinkingText,
			Timestamp:          e.timestamp,
			HasThinking:        hasThinking,
			HasToolUse:         hasToolUse,
			ContentLength:      len(text),
			ToolCalls:          tcs,
			ToolResults:        trs,
			SourceType:         e.entryType,
			SourceUUID:         e.uuid,
			SourceParentUUID:   e.parentUuid,
			IsSidechain:        gjson.Get(e.line, "isSidechain").Bool(),
			tokenPresenceKnown: e.entryType == "assistant",
		}

		if e.entryType == "assistant" {
			extractClaudeTokenFields(&msg, e.line)
		}

		messages = append(messages, msg)
		ordinal++
	}

	return messages, startedAt, endedAt
}

// claudeSessionMeta holds source metadata extracted during the
// main parse loop and applied to all resulting ParsedSessions.
type claudeSessionMeta struct {
	sourceSessionID string
	sourceVersion   string
	cwd             string
	gitBranch       string
	malformedLines  int
	isTruncated     bool
}

// applyTo sets source metadata fields on a ParsedSession.
func (m claudeSessionMeta) applyTo(sess *ParsedSession) {
	sess.SourceSessionID = m.sourceSessionID
	sess.SourceVersion = m.sourceVersion
	sess.Cwd = m.cwd
	sess.GitBranch = m.gitBranch
	sess.MalformedLines = m.malformedLines
	sess.IsTruncated = m.isTruncated
}

// parseLinear processes entries sequentially without DAG awareness.
func parseLinear(
	entries []dagEntry,
	sessionID, project, machine, parentSessionID string,
	fileInfo FileInfo,
	subagentMap map[string]string,
	globalStart, globalEnd time.Time,
	meta claudeSessionMeta,
) ([]ParseResult, error) {
	messages, startedAt, endedAt := extractMessages(entries)
	startedAt = earlierTime(globalStart, startedAt)
	endedAt = laterTime(globalEnd, endedAt)
	annotateSubagentSessions(messages, subagentMap)

	// Promoted system messages (continuation/resume/interrupted/
	// task_notification/stop_hook) carry Role=user so role-keyed
	// analytics ignore them, but they are not real user turns;
	// firstMessageAndUserCount skips them when computing
	// user_message_count / first_message. It also skips leading
	// /clear and /effort command envelopes so the sidebar shows
	// the next real message instead of the command.
	firstMsg, userCount := firstMessageAndUserCount(messages)

	sess := ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentClaude,
		ParentSessionID:  parentSessionID,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File:             fileInfo,
	}
	meta.applyTo(&sess)
	accumulateMessageTokenUsage(&sess, messages)

	return []ParseResult{{Session: sess, Messages: messages}}, nil
}

// parseDAG builds a parent->children adjacency map and walks the
// tree to detect fork points. Large-gap forks produce separate
// ParseResults; small-gap retries follow the latest branch.
func parseDAG(
	entries []dagEntry,
	sessionID, project, machine, parentSessionID string,
	fileInfo FileInfo,
	subagentMap map[string]string,
	globalStart, globalEnd time.Time,
	meta claudeSessionMeta,
) ([]ParseResult, error) {
	// Build parent -> children ordered by line position and
	// collect the set of all uuids for connectivity checks.
	children := make(map[string][]int, len(entries))
	uuidSet := make(map[string]struct{}, len(entries))
	var roots []int
	for i, e := range entries {
		if e.uuid != "" {
			uuidSet[e.uuid] = struct{}{}
		}
		if e.parentUuid == "" {
			roots = append(roots, i)
		} else {
			children[e.parentUuid] = append(children[e.parentUuid], i)
		}
	}

	// A well-formed DAG has exactly one root and all parentUuid
	// references resolve to an existing entry's uuid. If not,
	// fall back to linear parsing to avoid dropping messages.
	if len(roots) != 1 {
		return parseLinear(
			entries, sessionID, project, machine,
			parentSessionID, fileInfo, subagentMap,
			globalStart, globalEnd, meta,
		)
	}
	for _, e := range entries {
		if e.parentUuid != "" {
			if _, ok := uuidSet[e.parentUuid]; !ok {
				return parseLinear(
					entries, sessionID, project, machine,
					parentSessionID, fileInfo, subagentMap,
					globalStart, globalEnd, meta,
				)
			}
		}
	}

	// Walk from the root, collecting branches.
	// branches[0] is the main branch; subsequent entries are forks.
	type branch struct {
		indices  []int
		parentID string // immediate parent session ID
	}

	var branches []branch

	// walkBranch follows the DAG from a starting index, collecting
	// all entries on the chosen path. At fork points, it either
	// follows the latest child (small gap) or splits (large gap).
	// ownerID is the session ID of the branch that owns this walk.
	var walkBranch func(startIdx int, ownerID string) []int
	var forkBranches []branch

	walkBranch = func(startIdx int, ownerID string) []int {
		var path []int
		current := startIdx

		for current >= 0 {
			path = append(path, current)
			uuid := entries[current].uuid
			kids := children[uuid]
			if len(kids) == 0 {
				break
			}
			if len(kids) == 1 {
				current = kids[0]
				continue
			}

			// Fork point: count user turns on first child's branch.
			firstChildTurns := countUserTurns(entries, children, kids[0])
			if firstChildTurns <= forkThreshold {
				// Small-gap retry: follow the last child.
				current = kids[len(kids)-1]
			} else {
				// Large-gap fork: follow first child on main,
				// collect other children as fork branches.
				for _, kid := range kids[1:] {
					forkSID := sessionID + "-" +
						entries[kid].uuid
					forkPath := walkBranch(kid, forkSID)
					forkBranches = append(
						forkBranches,
						branch{
							indices:  forkPath,
							parentID: ownerID,
						},
					)
				}
				current = kids[0]
			}
		}

		return path
	}

	mainPath := walkBranch(roots[0], sessionID)
	branches = append(
		branches,
		branch{indices: mainPath, parentID: parentSessionID},
	)
	branches = append(branches, forkBranches...)

	// Build results for each branch.
	var results []ParseResult

	for i, b := range branches {
		branchEntries := make([]dagEntry, len(b.indices))
		for j, idx := range b.indices {
			branchEntries[j] = entries[idx]
		}

		messages, startedAt, endedAt := extractMessages(branchEntries)
		// Main session uses global bounds to capture timestamps
		// from non-message events (e.g. queue-operation).
		if i == 0 {
			startedAt = earlierTime(globalStart, startedAt)
			endedAt = laterTime(globalEnd, endedAt)
		}
		annotateSubagentSessions(messages, subagentMap)

		firstMsg, userCount := firstMessageAndUserCount(messages)

		sid := sessionID
		pSID := b.parentID
		relType := RelationshipType("")

		if i > 0 {
			// Fork session: ID derived from first entry's uuid,
			// parent is the branch that forked.
			firstEntry := entries[b.indices[0]]
			sid = sessionID + "-" + firstEntry.uuid
			relType = RelFork
		}

		sess := ParsedSession{
			ID:               sid,
			Project:          project,
			Machine:          machine,
			Agent:            AgentClaude,
			ParentSessionID:  pSID,
			RelationshipType: relType,
			FirstMessage:     firstMsg,
			StartedAt:        startedAt,
			EndedAt:          endedAt,
			MessageCount:     len(messages),
			UserMessageCount: userCount,
			File:             fileInfo,
		}
		meta.applyTo(&sess)
		accumulateMessageTokenUsage(&sess, messages)

		results = append(results, ParseResult{
			Session:  sess,
			Messages: messages,
		})
	}

	return results, nil
}

// collapseStreamingDuplicates removes consecutive assistant entries
// that share the same message.id. Claude Code writes multiple JSONL
// lines as a response streams — each has the same message.id but
// progressively more output tokens. Only the last entry in each
// same-message.id run has the final content and token counts.
func collapseStreamingDuplicates(entries []dagEntry) []dagEntry {
	if len(entries) <= 1 {
		return entries
	}

	result := make([]dagEntry, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		mid := ""
		if entries[i].entryType == "assistant" {
			mid = gjson.Get(entries[i].line, "message.id").Str
		}

		// Look ahead: if next entries are assistant with same
		// message.id, skip to the last one in the run.
		if mid != "" {
			j := i + 1
			for j < len(entries) &&
				entries[j].entryType == "assistant" &&
				gjson.Get(entries[j].line, "message.id").Str == mid {
				j++
			}
			// Keep only the last entry (j-1) in the run.
			result = append(result, entries[j-1])
			i = j - 1
		} else {
			result = append(result, entries[i])
		}
	}
	return result
}

// countUserTurns counts all user entries reachable from a
// starting index by traversing the entire subtree. Earlier
// versions followed only the first child at each node, which
// undercounted in sessions with many nested forks and caused
// the fork heuristic to discard the main conversation branch.
func countUserTurns(
	entries []dagEntry,
	children map[string][]int,
	startIdx int,
) int {
	count := 0
	stack := []int{startIdx}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if entries[current].entryType == "user" {
			count++
		}
		stack = append(stack, children[entries[current].uuid]...)
	}
	return count
}

// extractMessages converts dagEntries into ParsedMessages, applying
// the same filtering and content extraction as the original linear
// parser.
func extractMessages(entries []dagEntry) (
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

		// Detect compact summaries before the user/assistant
		// gates: Claude can emit isCompactSummary=true with
		// either top-level type, and the record must always
		// be persisted as a system boundary regardless.
		if gjson.Get(e.line, "isCompactSummary").Bool() {
			summary := extractCompactSummary(e.line)
			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleAssistant,
				Content:           summary,
				Timestamp:         e.timestamp,
				IsSystem:          true,
				ContentLength:     len(summary),
				SourceType:        "system",
				SourceSubtype:     "compact_boundary",
				SourceUUID:        e.uuid,
				SourceParentUUID:  e.parentUuid,
				IsSidechain:       gjson.Get(e.line, "isSidechain").Bool(),
				IsCompactBoundary: true,
			})
			ordinal++
			continue
		}

		// Tier 1: skip system-injected user entries.
		if e.entryType == "user" {
			if gjson.Get(e.line, "isMeta").Bool() {
				continue
			}
		}

		content := gjson.Get(e.line, "message.content")
		text, thinkingText, hasThinking, hasToolUse, tcs, trs :=
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

		if strings.TrimSpace(text) == "" && len(trs) == 0 {
			continue
		}

		// Tier 2: promote classifiable system-injected patterns
		// to source_subtype messages; skip unclassified noise
		// (e.g. non-caveat <local-command-*> envelopes). Role
		// stays "user" so role-keyed analytics continue to treat
		// these as inputs, not assistant replies.
		if e.entryType == "user" {
			if subtype := ClassifyClaudeSystemMessage(text); subtype != "" {
				messages = append(messages, ParsedMessage{
					Ordinal:          ordinal,
					Role:             RoleUser,
					Content:          text,
					Timestamp:        e.timestamp,
					IsSystem:         true,
					ContentLength:    len(text),
					SourceType:       "system",
					SourceSubtype:    subtype,
					SourceUUID:       e.uuid,
					SourceParentUUID: e.parentUuid,
					IsSidechain:      gjson.Get(e.line, "isSidechain").Bool(),
				})
				ordinal++
				continue
			}
			if isClaudeSystemMessage(text) {
				continue
			}
		}

		msg := ParsedMessage{
			Ordinal:            ordinal,
			Role:               RoleType(e.entryType),
			Content:            text,
			ThinkingText:       thinkingText,
			Timestamp:          e.timestamp,
			HasThinking:        hasThinking,
			HasToolUse:         hasToolUse,
			ContentLength:      len(text),
			ToolCalls:          tcs,
			ToolResults:        trs,
			SourceType:         e.entryType,
			SourceUUID:         e.uuid,
			SourceParentUUID:   e.parentUuid,
			IsSidechain:        gjson.Get(e.line, "isSidechain").Bool(),
			tokenPresenceKnown: e.entryType == "assistant",
		}

		if e.entryType == "assistant" {
			extractClaudeTokenFields(&msg, e.line)
		}

		messages = append(messages, msg)
		ordinal++
	}

	return messages, startedAt, endedAt
}

// extractClaudeTokenFields populates Model, TokenUsage,
// ContextTokens, OutputTokens, ClaudeMessageID, and
// ClaudeRequestID on a ParsedMessage from a Claude JSONL line.
// Used by both full and incremental parsing paths.
func extractClaudeTokenFields(msg *ParsedMessage, line string) {
	msg.Model = gjson.Get(line, "message.model").String()
	msg.ClaudeMessageID = gjson.Get(line, "message.id").String()
	msg.ClaudeRequestID = gjson.Get(line, "requestId").String()

	usageResult := gjson.Get(line, "message.usage")
	if usageResult.Exists() {
		msg.TokenUsage = json.RawMessage(usageResult.Raw)
		msg.HasOutputTokens = usageResult.Get("output_tokens").Exists()
		msg.HasContextTokens = usageResult.Get("input_tokens").Exists() ||
			usageResult.Get("cache_creation_input_tokens").Exists() ||
			usageResult.Get("cache_read_input_tokens").Exists()

		input := int(usageResult.Get("input_tokens").Int())
		cacheCreation := int(usageResult.Get(
			"cache_creation_input_tokens",
		).Int())
		cacheRead := int(usageResult.Get(
			"cache_read_input_tokens",
		).Int())
		msg.OutputTokens = int(usageResult.Get(
			"output_tokens",
		).Int())
		msg.ContextTokens = input + cacheCreation + cacheRead
	}
}

// annotateSubagentSessions sets SubagentSessionID on tool calls
// whose ToolUseID appears in the subagentMap. Only tool calls that
// represent subagent invocations (category "Task" or name containing
// "subagent") are annotated.
func annotateSubagentSessions(
	messages []ParsedMessage, subagentMap map[string]string,
) {
	if len(subagentMap) == 0 {
		return
	}
	for i := range messages {
		for j := range messages[i].ToolCalls {
			tc := &messages[i].ToolCalls[j]
			if tc.ToolUseID == "" {
				continue
			}
			if sid, ok := subagentMap[tc.ToolUseID]; ok {
				if tc.Category == "Task" ||
					strings.Contains(tc.ToolName, "subagent") {
					tc.SubagentSessionID = sid
				}
			}
		}
	}
}

// extractTimestamp parses the timestamp from a JSONL line,
// checking both top-level and snapshot timestamps.
func extractTimestamp(line string) time.Time {
	tsStr := gjson.Get(line, "timestamp").Str
	ts := parseTimestamp(tsStr)
	if ts.IsZero() {
		snapTsStr := gjson.Get(line, "snapshot.timestamp").Str
		ts = parseTimestamp(snapTsStr)
		if ts.IsZero() {
			if tsStr != "" {
				logParseError(tsStr)
			} else if snapTsStr != "" {
				logParseError(snapTsStr)
			}
		}
	}
	return ts
}

// earlierTime returns the earlier of two times, ignoring zero values.
func earlierTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.Before(b) {
		return a
	}
	return b
}

// laterTime returns the later of two times, ignoring zero values.
func laterTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.After(b) {
		return a
	}
	return b
}

// ExtractClaudeProjectHints reads project-identifying metadata
// from a Claude Code JSONL session file.
func ExtractClaudeProjectHints(
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
		log.Printf("reading hints from %s: %v", path, err)
	}
	return cwd, gitBranch
}

// ExtractCwdFromSession reads the first cwd field from a Claude
// Code JSONL session file.
func ExtractCwdFromSession(path string) string {
	cwd, _ := ExtractClaudeProjectHints(path)
	return cwd
}

func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	// Truncate at a valid rune boundary to avoid producing
	// invalid UTF-8.
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}

// extractCommandText detects Claude Code command/skill invocation
// messages and returns a human-readable representation like
// "/skill-name args". Only matches messages whose trimmed content
// starts with <command-message> or <command-name> (the standard
// envelope format), so user messages that merely mention these
// tags in prose are not affected.
// Returns ("", false) if the content is not a command message.
func extractCommandText(content string) (string, bool) {
	trimmed := strings.TrimLeftFunc(content, func(r rune) bool {
		return r == '\uFEFF' || unicode.IsSpace(r)
	})
	if !strings.HasPrefix(trimmed, "<command-message>") &&
		!strings.HasPrefix(trimmed, "<command-name>") {
		return "", false
	}
	// Verify the content is purely command XML tags with no
	// trailing prose — strip all known tags and check the
	// remainder is whitespace-only.
	stripped := xmlCmdStripRe.ReplaceAllString(trimmed, "")
	if strings.TrimSpace(stripped) != "" {
		return "", false
	}
	m := xmlCmdNameRe.FindStringSubmatch(content)
	if m == nil {
		// Bare <command-message> without <command-name>: extract
		// the command-message value as a fallback.
		if cm := xmlCmdMsgRe.FindStringSubmatch(content); cm != nil {
			return "/" + cm[1], true
		}
		return "", false
	}
	name := m[1]
	// Ensure the name starts with "/" for display.
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	args := ""
	if am := xmlCmdArgsRe.FindStringSubmatch(content); am != nil {
		args = strings.TrimSpace(am[1])
	}
	if args != "" {
		return name + " " + args, true
	}
	return name, true
}

// isCommandEnvelope returns true if the content is a pure
// command XML envelope (starts with a command tag and contains
// nothing but command tags and whitespace). Used as a fallback
// to skip messages that look like command envelopes but couldn't
// be normalized by extractCommandText.
func isCommandEnvelope(content string) bool {
	trimmed := strings.TrimLeftFunc(content, func(r rune) bool {
		return r == '\uFEFF' || unicode.IsSpace(r)
	})
	if !strings.HasPrefix(trimmed, "<command-message>") &&
		!strings.HasPrefix(trimmed, "<command-name>") {
		return false
	}
	stripped := xmlCmdStripRe.ReplaceAllString(trimmed, "")
	return strings.TrimSpace(stripped) == ""
}

// previewSkippedCommands lists Claude Code commands that should
// not be used as a session's first_message preview. When a
// session opens with one of these, the parser skips past it and
// picks the next real user message so the sidebar shows
// something descriptive.
var previewSkippedCommands = []string{"/clear", "/effort"}

// isSkippablePreviewCommand returns true when content is a known
// Claude Code command (optionally followed by arguments), for the
// purpose of skipping it when computing first_message. Match is
// word-boundary: the trimmed content must equal the command
// exactly or be followed by a whitespace rune, so "/clearcache"
// does not match "/clear".
func isSkippablePreviewCommand(content string) bool {
	trimmed := strings.TrimSpace(content)
	for _, cmd := range previewSkippedCommands {
		if !strings.HasPrefix(trimmed, cmd) {
			continue
		}
		if len(trimmed) == len(cmd) {
			return true
		}
		r, _ := utf8.DecodeRuneInString(trimmed[len(cmd):])
		if unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// firstMessageAndUserCount returns the preview string and the
// total number of real (non-system) user turns. The preview skips
// known Claude Code command envelopes like /clear and /effort so
// sessions that begin with a command still show a meaningful
// preview; the user count always reflects every non-system user
// turn, including skipped commands.
func firstMessageAndUserCount(
	messages []ParsedMessage,
) (string, int) {
	firstMsg := ""
	userCount := 0
	for _, m := range messages {
		if m.IsSystem {
			continue
		}
		if m.Role != RoleUser || m.Content == "" {
			continue
		}
		userCount++
		if firstMsg == "" &&
			!isSkippablePreviewCommand(m.Content) {
			firstMsg = truncate(
				strings.ReplaceAll(m.Content, "\n", " "), 300,
			)
		}
	}
	return firstMsg, userCount
}

// fileEndsWithNewline returns true when the byte at size-1
// is '\n'. Used to distinguish a fully-flushed final line
// from a truncated write. Empty files return true (no
// dangling content).
func fileEndsWithNewline(f *os.File, size int64) bool {
	if size <= 0 {
		return true
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

// extractCompactSummary extracts text from a Claude compact
// summary JSONL entry. Content is usually an array of content
// blocks in message.content, but Claude also emits compact
// summaries with content as a plain string — handle both.
func extractCompactSummary(line string) string {
	content := gjson.Get(line, "message.content")
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, v gjson.Result) bool {
			if v.Get("type").Str == "text" {
				parts = append(parts, v.Get("text").Str)
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	return content.Str
}

// ClassifyClaudeSystemMessage inspects a user-entry content string and
// returns the matched system subtype (e.g. "continuation", "resume"),
// or "" if the content is an ordinary user message.
//
// Non-caveat <local-command-*> envelopes (stdout/stderr surrounds for
// local command output) are treated as regular noise and return "";
// only the caveat variant is a semantic "resume" marker.
func ClassifyClaudeSystemMessage(content string) string {
	trimmed := strings.TrimLeftFunc(content, func(r rune) bool {
		return r == '\uFEFF' || unicode.IsSpace(r)
	})
	switch {
	case strings.HasPrefix(trimmed, "This session is being continued"):
		return "continuation"
	case strings.HasPrefix(trimmed, "<local-command-caveat>"):
		return "resume"
	case strings.HasPrefix(trimmed, "[Request interrupted"):
		return "interrupted"
	case strings.HasPrefix(trimmed, "<task-notification>"):
		return "task_notification"
	case strings.HasPrefix(trimmed, "Stop hook feedback:"):
		return "stop_hook"
	}
	return ""
}

// isClaudeSystemMessage returns true if the content matches
// a known system-injected user message pattern.
func isClaudeSystemMessage(content string) bool {
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
