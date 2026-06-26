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
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

var (
	xmlTaskIDRe               = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	xmlToolUseRe              = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	xmlCmdNameRe              = regexp.MustCompile(`<command-name>([^<]+)</command-name>`)
	xmlCmdMsgRe               = regexp.MustCompile(`<command-message>([^<]+)</command-message>`)
	xmlCmdArgsRe              = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
	xmlCmdStripRe             = regexp.MustCompile(`<command-(?:name|message|args)>[^<]*</command-(?:name|message|args)>`)
	persistedToolResultPathRe = regexp.MustCompile(`(?m)Full output saved to:\s*(.+)$`)
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

// claudeQueuedCommand is a user message Claude Code persisted as
// type=attachment with attachment.type=queued_command — i.e. a
// prompt the user typed while a tool call was still running.
// These records have no uuid/parentUuid, so we collect them out
// of band and splice them into the message stream by timestamp
// after DAG processing completes.
type claudeQueuedCommand struct {
	prompt    string
	timestamp time.Time
}

// claudeParseWithExclusions parses a Claude Code JSONL session file
// and also returns session IDs intentionally excluded from the
// archive, such as content-free /usage probes. Sync uses those IDs
// during full resync so orphan preservation does not restore rows the
// current parser deliberately dropped. This is the provider-owned
// parse body shared by the Claude provider (both its discovered-session
// Parse path and its ParseUploadedTranscript entry) and the Cowork
// parser (which reuses the Claude transcript format); it carries no
// legacy entrypoint naming so the provider can call it without shimming
// a Parse* free function.
func claudeParseWithExclusions(
	path, project, machine string,
) ([]ParseResult, []string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// First pass: collect all valid lines with metadata.
	var (
		entries         = make([]dagEntry, 0)
		queuedCommands  []claudeQueuedCommand
		hasAnyUUID      bool
		allHaveUUID     bool
		parentSessionID string
		sourceSessionID string
		sourceVersion   string
		cwd             string
		gitBranch       string
		displayName     string
		foundParentSID  bool
		lineIndex       int
		malformedLines  int
		lastLine        string
		subagentMap     = map[string]string{}
		globalStart     time.Time
		globalEnd       time.Time
	)
	allHaveUUID = true
	parentSessionID = claudeCompanionParentSessionID(path, sessionID)

	lr := newLineReader(f, maxLineSize)
	lastLineFailed := false
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		lastLine = line
		if !gjson.Valid(line) {
			malformedLines++
			lastLineFailed = true
			continue
		}
		line = resolveClaudePersistedToolResults(path, line)
		lastLineFailed = false

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

		// Collect queued_command attachments — user messages
		// the user typed mid-tool-call. Other attachment types
		// (e.g. task_reminder) are intentionally dropped.
		if entryType == "attachment" {
			if qc, ok := extractQueuedCommand(line); ok {
				queuedCommands = append(queuedCommands, qc)
			}
			continue
		}

		// Handle system records. /rename local commands update the
		// display name; last rename wins (empty arg clears it).
		if entryType == "system" {
			if name, ok := extractRenameName(
				gjson.Get(line, "content").Str,
			); ok {
				displayName = name
			}
			continue
		}

		if entryType != "user" && entryType != "assistant" {
			continue
		}

		// Collect subagent links and cwd/gitBranch from user entries.
		if entryType == "user" {
			collectToolResultAgentID(line, subagentMap)
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
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Detect truncation: last line is non-empty, invalid JSON,
	// AND the file did not end with a newline. A newline-
	// terminated invalid line is just a complete malformed
	// record, not a truncated write.
	isTruncated := lastLine != "" &&
		strings.TrimSpace(lastLine) != "" &&
		!gjson.Valid(lastLine) &&
		!fileEndsWithNewline(f, info.Size())

	// Merge consecutive assistant entries that share the same
	// message.id. Claude Code writes both cumulative streaming
	// snapshots and additive chunks for one response under the same
	// provider message id. Keep final metadata/token usage while
	// preserving distinct content blocks from the whole run.
	entries = mergeClaudeAssistantMessageChunks(entries)

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
		displayName:     displayName,
		malformedLines:  malformedLines,
		isTruncated:     isTruncated,
	}

	var (
		results  []ParseResult
		parseErr error
	)
	// If all user/assistant entries have uuids, use DAG-aware processing.
	if hasAnyUUID && allHaveUUID {
		results, parseErr = parseDAG(
			entries, sessionID, project, machine,
			parentSessionID, fileInfo, subagentMap,
			globalStart, globalEnd, meta,
		)
	} else {
		// Fall back to linear processing.
		results, parseErr = parseLinear(
			entries, sessionID, project, machine,
			parentSessionID, fileInfo, subagentMap,
			globalStart, globalEnd, meta,
		)
	}
	if parseErr != nil {
		return nil, nil, parseErr
	}

	// Splice queued_command attachments into the main session
	// by timestamp. Attachments have no uuid/parentUuid and so
	// can't participate in DAG fork detection; they belong to
	// the original conversation timeline (results[0]).
	if len(queuedCommands) > 0 && len(results) > 0 {
		results[0] = applyQueuedCommands(results[0], queuedCommands)
	}

	// Classify termination status for each result. All forks
	// from a single file share lastLineFailed because a
	// truncated tail affects every branch. The stop_reason is
	// pulled from the last assistant message in each branch so
	// "awaiting_user" can be distinguished from a generic clean
	// termination.
	for i := range results {
		results[i].Session.TerminationStatus = Classify(
			results[i].Messages,
			lastAssistantStopReason(results[i].Messages),
			lastLineFailed,
		)
	}

	// Drop content-free /usage probe sessions (e.g. CodexBar's
	// ClaudeProbe) after the queued-command splice so both inline
	// and queued /usage prompts are visible to the check. They never
	// enter the archive.
	kept := results[:0]
	var excluded []string
	for _, r := range results {
		if isUsageProbeSession(r.Messages) {
			excluded = append(excluded, r.Session.ID)
			continue
		}
		kept = append(kept, r)
	}
	return kept, excluded, nil
}

// lastAssistantStopReason returns the StopReason of the most
// recent assistant message in the slice, or "" when there is
// none. Used by Classify to decide between awaiting_user and
// clean for sessions that ended without an orphan tool_use.
func lastAssistantStopReason(messages []ParsedMessage) string {
	for _, v := range slices.Backward(messages) {
		if v.Role == RoleAssistant {
			return v.StopReason
		}
	}
	return ""
}

// claudeParseSessionFrom parses only new lines from a Claude JSONL
// file starting at the given byte offset. Returns only the newly
// parsed messages (with ordinals starting at startOrdinal) and the
// latest timestamp. Fork detection is skipped — new entries are
// processed linearly. Used by the Claude provider for incremental
// re-parsing of append-only session files. ErrDAGDetected is returned
// when appended lines contain uuid fields that require DAG-aware fork
// detection, which incremental parsing cannot handle. This is the
// provider-owned incremental body; it carries no legacy entrypoint
// naming so the provider can call it without shimming a Parse* free
// function.
var ErrDAGDetected = fmt.Errorf(
	"incremental parse: DAG uuid detected",
)

// ErrClaudeIncrementalNeedsFullParse signals that appended Claude
// lines contain content the incremental path cannot stitch into
// already-stored rows (subagent linkage updates from
// toolUseResult.agentId, or same-message.id chunk merging).
var ErrClaudeIncrementalNeedsFullParse = fmt.Errorf(
	"incremental parse: appended Claude lines require full parse",
)

func claudeParseSessionFrom(
	path string,
	offset int64,
	startOrdinal int,
	lastEntryUUID string,
) ([]ParsedMessage, time.Time, int64, error) {
	var (
		entries        []dagEntry
		queuedCommands []claudeQueuedCommand
		subagentMap    = make(map[string]string)
		lineIndex      = startOrdinal
		// Track latest timestamp from all lines, including
		// non-message events (progress, queue-operation) so
		// callers can update ended_at even when no new
		// messages are found.
		latestTS  time.Time
		sawRename bool
	)

	consumed, err := readJSONLFrom(
		path, offset, func(line string) {
			line = resolveClaudePersistedToolResults(path, line)
			if ts := extractTimestamp(line); !ts.IsZero() {
				if ts.After(latestTS) {
					latestTS = ts
				}
			}
			entryType := gjson.Get(line, "type").Str
			if entryType == "system" {
				if _, ok := extractRenameName(
					gjson.Get(line, "content").Str,
				); ok {
					sawRename = true
				}
				return
			}
			if entryType == "attachment" {
				if qc, ok := extractQueuedCommand(line); ok {
					queuedCommands = append(queuedCommands, qc)
				}
				return
			}
			if entryType == "queue-operation" {
				if gjson.Get(line, "operation").Str == "enqueue" {
					contentStr := gjson.Get(line, "content").Str
					if contentStr != "" {
						tuid := gjson.Get(contentStr, "tool_use_id").Str
						taskID := gjson.Get(contentStr, "task_id").Str
						if tuid == "" || taskID == "" {
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
				return
			}
			if entryType == "progress" {
				if gjson.Get(line, "data.type").Str == "agent_progress" {
					tuid := gjson.Get(line, "parentToolUseID").Str
					agentID := gjson.Get(line, "data.agentId").Str
					if tuid != "" && agentID != "" {
						subagentMap[tuid] = "agent-" + agentID
					}
				}
				return
			}
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

	// A rename-only append produces no entries and no queued commands, so
	// the empty-entries early return below would silently succeed. Check
	// first and force a full parse so the display name is persisted.
	if sawRename {
		return nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}

	// Queue/progress events can repair subagent linkage on an already-stored
	// tool call. If the mapped tool_use_id is not introduced in this append,
	// incremental parsing would advance file_size without updating that row.
	if needsClaudeFullParseForSubagentMap(entries, subagentMap) {
		return nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}

	if len(entries) == 0 && len(queuedCommands) == 0 {
		return nil, latestTS, consumed, nil
	}

	// Detect forks: if any entry's parentUuid doesn't
	// match the previous entry's uuid, the appended data
	// contains a branch that requires full DAG processing.
	if hasDAGFork(entries, lastEntryUUID) {
		return nil, time.Time{}, 0, ErrDAGDetected
	}

	// Subagent linkage updates (toolUseResult.agentId) and
	// same-message.id chunk merging both need state the full
	// parser builds across the whole file. Bail to a full parse
	// when appended lines contain either.
	if needsClaudeFullParse(entries) {
		return nil, time.Time{}, 0,
			ErrClaudeIncrementalNeedsFullParse
	}

	msgs, _, endedAt := extractMessagesFrom(
		entries, startOrdinal,
	)
	annotateSubagentSessions(msgs, subagentMap)
	if len(queuedCommands) > 0 {
		msgs = mergeQueuedCommands(
			msgs, queuedCommands, startOrdinal,
		)
		for _, qc := range queuedCommands {
			if qc.timestamp.After(endedAt) {
				endedAt = qc.timestamp
			}
		}
	}
	// Use the latest timestamp from all lines (including
	// non-message events) if it's later than what
	// extractMessagesFrom found.
	if latestTS.After(endedAt) {
		endedAt = latestTS
	}
	return msgs, endedAt, consumed, nil
}

// needsClaudeFullParse returns true when appended entries contain
// either a tool_result with toolUseResult.agentId (whose linkage
// must update an already-stored tool_call row) or a consecutive
// same-message.id assistant run (whose chunks the full parser
// merges into one message). Both cases require a full re-parse.
func needsClaudeFullParse(entries []dagEntry) bool {
	toolUseIDs := make(map[string]struct{})
	var prevAssistantMID string
	for _, e := range entries {
		if e.entryType == "user" {
			if gjson.Get(e.line, "toolUseResult.agentId").Str != "" {
				return true
			}
			content := gjson.Get(e.line, "message.content")
			if content.IsArray() {
				unmatched := false
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str != "tool_result" {
						return true
					}
					toolUseID := part.Get("tool_use_id").Str
					if toolUseID == "" {
						return true
					}
					if _, ok := toolUseIDs[toolUseID]; !ok {
						unmatched = true
						return false
					}
					return true
				})
				if unmatched {
					return true
				}
			}
		}
		if e.entryType == "assistant" {
			mid := gjson.Get(e.line, "message.id").Str
			if mid != "" && mid == prevAssistantMID {
				return true
			}
			content := gjson.Get(e.line, "message.content")
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str != "tool_use" {
						return true
					}
					if toolUseID := part.Get("id").Str; toolUseID != "" {
						toolUseIDs[toolUseID] = struct{}{}
					}
					return true
				})
			}
			prevAssistantMID = mid
			continue
		}
		prevAssistantMID = ""
	}
	return false
}

func needsClaudeFullParseForSubagentMap(
	entries []dagEntry, subagentMap map[string]string,
) bool {
	if len(subagentMap) == 0 {
		return false
	}

	appendedToolUseIDs := make(map[string]struct{})
	for _, e := range entries {
		if e.entryType != "assistant" {
			continue
		}
		content := gjson.Get(e.line, "message.content")
		if !content.IsArray() {
			continue
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str != "tool_use" {
				return true
			}
			if toolUseID := part.Get("id").Str; toolUseID != "" {
				appendedToolUseIDs[toolUseID] = struct{}{}
			}
			return true
		})
	}

	for toolUseID := range subagentMap {
		if _, ok := appendedToolUseIDs[toolUseID]; !ok {
			return true
		}
	}
	return false
}

// hasDAGFork returns true if the entries contain a fork —
// i.e. any entry whose parentUuid doesn't point to the
// immediately preceding entry's uuid. Linear UUID chains
// (each entry parenting the next) are safe for incremental
// parsing; forks require full DAG processing.
func hasDAGFork(entries []dagEntry, lastEntryUUID string) bool {
	lastUUID := lastEntryUUID
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
			if subtype := classifyClaudeSystemMessage(text); subtype != "" {
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
			msg.StopReason = gjson.Get(e.line, "message.stop_reason").Str
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
	displayName     string
	malformedLines  int
	isTruncated     bool
}

// applyTo sets source metadata fields on a ParsedSession.
func (m claudeSessionMeta) applyTo(sess *ParsedSession) {
	sess.SourceSessionID = m.sourceSessionID
	sess.SourceVersion = m.sourceVersion
	sess.Cwd = m.cwd
	sess.GitBranch = m.gitBranch
	sess.SessionName = m.displayName
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

func collectToolResultAgentID(line string, subagentMap map[string]string) {
	agentID := gjson.Get(line, "toolUseResult.agentId").Str
	if agentID == "" {
		return
	}
	sessionID := agentID
	if !strings.HasPrefix(sessionID, "agent-") {
		sessionID = "agent-" + sessionID
	}

	content := gjson.Get(line, "message.content")
	if !content.IsArray() {
		return
	}
	var toolUseID string
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").Str != "tool_result" {
			return true
		}
		tuid := block.Get("tool_use_id").Str
		if tuid == "" {
			return true
		}
		if toolUseID != "" {
			toolUseID = ""
			return false
		}
		toolUseID = tuid
		return true
	})
	if toolUseID == "" {
		return
	}
	if _, exists := subagentMap[toolUseID]; !exists {
		subagentMap[toolUseID] = sessionID
	}
}

// extractQueuedCommand parses a Claude Code attachment entry and
// returns the queued_command prompt if present. Other attachment
// types (e.g. task_reminder) return ok=false. Whitespace-only or
// empty prompts also return false to match the parser's general
// "skip empty user content" behavior.
func extractQueuedCommand(line string) (claudeQueuedCommand, bool) {
	if gjson.Get(line, "attachment.type").Str != "queued_command" {
		return claudeQueuedCommand{}, false
	}
	prompt := gjson.Get(line, "attachment.prompt").Str
	if strings.TrimSpace(prompt) == "" {
		return claudeQueuedCommand{}, false
	}
	return claudeQueuedCommand{
		prompt:    prompt,
		timestamp: extractTimestamp(line),
	}, true
}

// applyQueuedCommands splices queued_command attachments into a
// ParseResult by timestamp, renumbers ordinals, and refreshes
// derived session counts. Token aggregates are unchanged because
// queued_command entries have no usage data. Callers must ensure
// queued is non-empty.
func applyQueuedCommands(
	r ParseResult, queued []claudeQueuedCommand,
) ParseResult {
	merged := mergeQueuedCommands(r.Messages, queued, 0)
	firstMsg, userCount := firstMessageAndUserCount(merged)
	r.Session.FirstMessage = firstMsg
	r.Session.UserMessageCount = userCount
	r.Session.MessageCount = len(merged)
	for _, qc := range queued {
		if qc.timestamp.After(r.Session.EndedAt) {
			r.Session.EndedAt = qc.timestamp
		}
		if !qc.timestamp.IsZero() &&
			(r.Session.StartedAt.IsZero() ||
				qc.timestamp.Before(r.Session.StartedAt)) {
			r.Session.StartedAt = qc.timestamp
		}
	}
	r.Messages = merged
	return r
}

// mergeQueuedCommands merges queued_command entries into messages
// in timestamp order and renumbers ordinals starting at the given
// offset. Both inputs are assumed to already be in chronological
// order. Equal timestamps preserve the original message before the
// queued command (queued commands always follow the entry that
// triggered the tool call).
func mergeQueuedCommands(
	messages []ParsedMessage,
	queued []claudeQueuedCommand,
	startOrdinal int,
) []ParsedMessage {
	out := make([]ParsedMessage, 0, len(messages)+len(queued))
	i, j := 0, 0
	for i < len(messages) && j < len(queued) {
		if queuedBefore(queued[j], messages[i]) {
			out = append(out, queuedCommandMessage(queued[j]))
			j++
		} else {
			out = append(out, messages[i])
			i++
		}
	}
	for ; i < len(messages); i++ {
		out = append(out, messages[i])
	}
	for ; j < len(queued); j++ {
		out = append(out, queuedCommandMessage(queued[j]))
	}
	for k := range out {
		out[k].Ordinal = startOrdinal + k
	}
	return out
}

// queuedBefore reports whether a queued_command should sort before
// a regular message. Zero timestamps on either side are treated
// conservatively: a zero-timestamp message keeps its original
// position relative to queued items.
func queuedBefore(
	q claudeQueuedCommand, m ParsedMessage,
) bool {
	if q.timestamp.IsZero() {
		return false
	}
	if m.Timestamp.IsZero() {
		return false
	}
	return q.timestamp.Before(m.Timestamp)
}

// queuedCommandMessage builds a ParsedMessage from a collected
// queued_command attachment. Role stays user (the user typed
// this) and IsSystem is false so it counts as a real user turn;
// SourceSubtype lets the UI distinguish it from inline prompts.
func queuedCommandMessage(
	q claudeQueuedCommand,
) ParsedMessage {
	return ParsedMessage{
		Role:          RoleUser,
		Content:       q.prompt,
		Timestamp:     q.timestamp,
		ContentLength: len(q.prompt),
		SourceType:    "user",
		SourceSubtype: "queued_command",
	}
}

// mergeClaudeAssistantMessageChunks merges consecutive assistant
// entries that share the same message.id. Claude Code uses this shape
// both for cumulative streaming snapshots and for additive chunks of a
// single response. The last entry owns metadata and token usage; the
// merged message content keeps each distinct block in first-seen order.
func mergeClaudeAssistantMessageChunks(entries []dagEntry) []dagEntry {
	if len(entries) <= 1 {
		return entries
	}

	result := make([]dagEntry, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		mid := ""
		if entries[i].entryType == "assistant" {
			mid = gjson.Get(entries[i].line, "message.id").Str
		}
		if mid == "" {
			result = append(result, entries[i])
			continue
		}

		j := i + 1
		for j < len(entries) &&
			entries[j].entryType == "assistant" &&
			gjson.Get(entries[j].line, "message.id").Str == mid {
			j++
		}
		if j == i+1 {
			result = append(result, entries[i])
		} else {
			result = append(result, mergeClaudeAssistantRun(entries[i:j]))
		}
		i = j - 1
	}
	return result
}

// mergeClaudeAssistantRun collapses one same-message.id assistant
// run into a single dagEntry. For each snapshot in the run we decide
// whether it is a cumulative continuation of the merged-so-far (its
// leading blocks align by type, tool_use id, and text equal-or-prefix)
// or an additive chunk (distinct new content). Cumulative snapshots
// update overlapping positions in place and append any trailing blocks;
// additive snapshots append blocks not already present.
//
// Once any entry in the run has stop_reason="end_turn", the message
// has terminated; subsequent same-message.id entries are treated as
// additive distinct chunks rather than streaming snapshots, even if
// their text would otherwise prefix-match.
func mergeClaudeAssistantRun(run []dagEntry) dagEntry {
	base := run[len(run)-1]
	var merged []gjson.Result
	// Once a snapshot in the run has stop_reason="end_turn" the
	// message has terminated. Any further same-message.id entries
	// are additive distinct chunks rather than streaming snapshots,
	// so cumulative prefix-matching must be skipped.
	runEnded := false

	for _, e := range run {
		content := gjson.Get(e.line, "message.content")
		if !content.IsArray() {
			continue
		}
		merged = mergeClaudeSnapshot(
			merged, claudeContentBlocks(content), runEnded,
		)
		if gjson.Get(e.line, "message.stop_reason").Str == "end_turn" {
			runEnded = true
		}
	}
	if len(merged) == 0 {
		return base
	}
	base.line = replaceClaudeMessageContent(base.line, merged)
	return base
}

func claudeContentBlocks(content gjson.Result) []gjson.Result {
	var blocks []gjson.Result
	content.ForEach(func(_, b gjson.Result) bool {
		if b.Raw != "" {
			blocks = append(blocks, b)
		}
		return true
	})
	return blocks
}

func mergeClaudeSnapshot(
	merged, snapshot []gjson.Result, runEnded bool,
) []gjson.Result {
	if !runEnded && claudeSnapshotIsCumulative(merged, snapshot) {
		for i, block := range snapshot {
			if i < len(merged) {
				merged[i] = pickClaudeLatestBlock(merged[i], block)
				continue
			}
			merged = append(merged, block)
		}
		return merged
	}
	for _, block := range snapshot {
		if !claudeBlockExistsIn(block, merged) {
			merged = append(merged, block)
		}
	}
	return merged
}

func claudeSnapshotIsCumulative(
	merged, snapshot []gjson.Result,
) bool {
	if len(merged) == 0 || len(snapshot) == 0 {
		return true
	}
	n := min(len(snapshot), len(merged))
	for i := range n {
		if !claudeBlocksAlign(merged[i], snapshot[i]) {
			return false
		}
	}
	return true
}

func claudeBlocksAlign(a, b gjson.Result) bool {
	if a.Get("type").Str != b.Get("type").Str {
		return false
	}
	switch a.Get("type").Str {
	case "text":
		ta := a.Get("text").Str
		tb := b.Get("text").Str
		return ta == tb ||
			strings.HasPrefix(tb, ta) ||
			strings.HasPrefix(ta, tb)
	case "tool_use":
		ida := a.Get("id").Str
		idb := b.Get("id").Str
		if ida != "" && idb != "" {
			return ida == idb
		}
		return a.Raw == b.Raw
	default:
		return a.Raw == b.Raw
	}
}

func pickClaudeLatestBlock(existing, candidate gjson.Result) gjson.Result {
	if existing.Get("type").Str != candidate.Get("type").Str {
		return candidate
	}
	switch existing.Get("type").Str {
	case "text":
		if len(candidate.Get("text").Str) >=
			len(existing.Get("text").Str) {
			return candidate
		}
		return existing
	case "tool_use":
		return candidate
	default:
		return existing
	}
}

func claudeBlockExistsIn(
	target gjson.Result, blocks []gjson.Result,
) bool {
	targetType := target.Get("type").Str
	targetID := target.Get("id").Str
	for _, b := range blocks {
		if b.Get("type").Str != targetType {
			continue
		}
		if targetType == "tool_use" && targetID != "" {
			if b.Get("id").Str == targetID {
				return true
			}
			continue
		}
		if b.Raw == target.Raw {
			return true
		}
	}
	return false
}

func replaceClaudeMessageContent(line string, blocks []gjson.Result) string {
	// UseNumber preserves the raw textual form of JSON numbers so
	// re-marshaling doesn't truncate large integers (e.g. usage
	// token counts) or change scientific notation.
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return line
	}
	msg, ok := top["message"].(map[string]any)
	if !ok {
		return line
	}
	content := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		if block.Raw == "" {
			continue
		}
		content = append(content, json.RawMessage(block.Raw))
	}
	msg["content"] = content
	encoded, err := json.Marshal(top)
	if err != nil {
		return line
	}
	return string(encoded)
}

func claudeCompanionParentSessionID(path, sessionID string) string {
	if !strings.HasPrefix(sessionID, "agent-") {
		return ""
	}
	parts := splitCleanPath(path)
	for i, part := range parts {
		if part != "subagents" || i == 0 {
			continue
		}
		parent := parts[i-1]
		if parent != "" {
			return parent
		}
	}
	return ""
}

func splitCleanPath(path string) []string {
	clean := filepath.Clean(path)
	var parts []string
	for {
		dir, file := filepath.Split(clean)
		if file != "" {
			parts = append(parts, file)
		}
		next := filepath.Clean(strings.TrimSuffix(dir, string(filepath.Separator)))
		if next == clean || next == "." || next == string(filepath.Separator) || next == "" {
			break
		}
		clean = next
	}
	slices.Reverse(parts)
	return parts
}

func resolveClaudePersistedToolResults(sessionPath, line string) string {
	if !strings.Contains(line, "persisted-output") &&
		!strings.Contains(line, "persistedOutputPath") {
		return line
	}

	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return line
	}

	msg, ok := top["message"].(map[string]any)
	if !ok {
		return line
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return line
	}

	persistedPath := ""
	if tur, ok := top["toolUseResult"].(map[string]any); ok {
		if p, ok := tur["persistedOutputPath"].(string); ok {
			persistedPath = p
		}
	}
	toolResultCount := countClaudeToolResultBlocks(blocks)

	changed := false
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		content, ok := block["content"].(string)
		if !ok {
			continue
		}
		path := persistedOutputPathFromContent(content)
		if path == "" && (toolResultCount == 1 ||
			isPersistedToolResultPlaceholder(content)) {
			path = persistedPath
		}
		if path == "" {
			continue
		}
		output, ok := readClaudePersistedToolResult(sessionPath, path)
		if !ok {
			continue
		}
		block["content"] = output
		changed = true
	}
	if !changed {
		return line
	}

	encoded, err := json.Marshal(top)
	if err != nil {
		return line
	}
	return string(encoded)
}

func countClaudeToolResultBlocks(blocks []any) int {
	count := 0
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if ok && block["type"] == "tool_result" {
			count++
		}
	}
	return count
}

func isPersistedToolResultPlaceholder(content string) bool {
	return strings.Contains(content, "<persisted-output>")
}

func persistedOutputPathFromContent(content string) string {
	match := persistedToolResultPathRe.FindStringSubmatch(content)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func readClaudePersistedToolResult(
	sessionPath, resultPath string,
) (string, bool) {
	if resultPath == "" {
		return "", false
	}
	if !filepath.IsAbs(resultPath) {
		return "", false
	}
	cleanResult := filepath.Clean(resultPath)
	for _, dir := range claudeToolResultDirs(sessionPath) {
		if !pathWithinDir(cleanResult, dir) {
			continue
		}
		b, err := os.ReadFile(cleanResult)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return "", false
}

func claudeToolResultDirs(sessionPath string) []string {
	var dirs []string
	sessionDir := filepath.Join(
		filepath.Dir(sessionPath),
		strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl"),
		"tool-results",
	)
	dirs = append(dirs, filepath.Clean(sessionDir))

	clean := filepath.Clean(sessionPath)
	needle := string(filepath.Separator) + "subagents" + string(filepath.Separator)
	if idx := strings.Index(clean, needle); idx > 0 {
		dirs = append(dirs, filepath.Join(clean[:idx], "tool-results"))
	}
	return dirs
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." &&
		rel != "" &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		rel != ".."
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
			if subtype := classifyClaudeSystemMessage(text); subtype != "" {
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
			msg.StopReason = gjson.Get(e.line, "message.stop_reason").Str
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

// extractRenameName returns the argument of a Claude Code /rename
// command envelope. The bool is true when content is a /rename
// invocation (including an empty argument, which clears the name) and
// false for any other command or non-command content.
func extractRenameName(content string) (string, bool) {
	m := xmlCmdNameRe.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	name := strings.TrimPrefix(strings.TrimSpace(m[1]), "/")
	if name != "rename" {
		return "", false
	}
	args := ""
	if am := xmlCmdArgsRe.FindStringSubmatch(content); am != nil {
		args = strings.TrimSpace(am[1])
	}
	return args, true
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

// isSkippablePreviewCommand returns true when content is a Claude
// Code slash command (e.g. /login, /plan, /roborev-fix). Detection
// is generic: the trimmed content must start with "/" followed by one
// or more letters, digits, hyphens, or underscores, then either end
// or be followed by whitespace. Hyphens and underscores are included
// because command envelopes normalise to names like /skill-name.
// File-path references like "/usr/local/bin gives an error" are not
// skipped because the embedded "/" terminates the match.
func isSkippablePreviewCommand(content string) bool {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "/") {
		return false
	}
	rest := trimmed[1:]
	i := 0
	for i < len(rest) {
		r, size := utf8.DecodeRuneInString(rest[i:])
		if unicode.IsSpace(r) {
			return i > 0
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			// Any other character (e.g. another "/") means this is not
			// a plain slash command.
			return false
		}
		i += size
	}
	return i > 0
}

// firstMessageAndUserCount returns the preview string and the
// total number of real (non-system) user turns. The preview skips
// Claude Code slash commands (e.g. /login, /plan, /clear) so
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

// isUsageProbeSession reports whether a parsed session's only real
// user turn(s) are the /usage command — a content-free usage probe
// (for example CodexBar's ClaudeProbe, which runs `claude /usage` to
// read usage stats) with no actual prompt. Such sessions carry no
// conversational content and are skipped during parsing. The notion
// of a real user turn mirrors firstMessageAndUserCount: non-system,
// role=user, non-empty content.
func isUsageProbeSession(messages []ParsedMessage) bool {
	sawUsage := false
	for _, m := range messages {
		if m.IsSystem || m.Role != RoleUser || m.Content == "" {
			continue
		}
		if strings.TrimSpace(m.Content) != "/usage" {
			return false
		}
		sawUsage = true
	}
	return sawUsage
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

// classifyClaudeSystemMessage inspects a user-entry content string and
// returns the matched system subtype (e.g. "continuation", "resume"),
// or "" if the content is an ordinary user message.
//
// Non-caveat <local-command-*> envelopes (stdout/stderr surrounds for
// local command output) are treated as regular noise and return "";
// only the caveat variant is a semantic "resume" marker.
func classifyClaudeSystemMessage(content string) string {
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
