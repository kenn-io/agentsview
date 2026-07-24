// ABOUTME: Parses Claude Code JSONL session files into structured session data.
// ABOUTME: Detects DAG forks in uuid/parentUuid trees and splits large-gap forks into separate sessions.
package parser

import (
	"bytes"
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
		agentLabel      string
		entrypoint      string
		foundParentSID  bool
		lineIndex       int
		malformedLines  int
		lastLineHasData bool
		lastLineValid   bool
		subagentMap     = map[string]string{}
		globalStart     time.Time
		globalEnd       time.Time
	)
	allHaveUUID = true
	parentSessionID = claudeCompanionParentSessionID(path, sessionID)

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	lastLineFailed := false
	for {
		lineBytes, ok := lr.nextBytes()
		if !ok {
			break
		}
		lastLineHasData = len(bytes.TrimSpace(lineBytes)) > 0
		lastLineValid = gjson.ValidBytes(lineBytes)
		if !lastLineValid {
			malformedLines++
			lastLineFailed = true
			continue
		}
		lastLineFailed = false

		entryType := gjson.GetBytes(lineBytes, "type").Str
		if agentLabel == "" {
			if value := gjson.GetBytes(lineBytes, "agentSetting").Str; strings.TrimSpace(value) != "" {
				agentLabel = strings.Clone(value)
			}
		}
		if entrypoint == "" {
			if value := gjson.GetBytes(lineBytes, "entrypoint").Str; strings.TrimSpace(value) != "" {
				entrypoint = strings.Clone(value)
			}
		}

		// Extract source version from first line that has it.
		if sourceVersion == "" {
			if v := gjson.GetBytes(lineBytes, "version").Str; v != "" {
				sourceVersion = strings.Clone(v)
			}
		}

		// Track global timestamps from all lines for session
		// bounds, including non-message events.
		if ts := extractTimestampBytes(lineBytes); !ts.IsZero() {
			if globalStart.IsZero() || ts.Before(globalStart) {
				globalStart = ts
			}
			if ts.After(globalEnd) {
				globalEnd = ts
			}
		}

		// Collect queue-operation enqueue entries for subagent mapping.
		if entryType == "queue-operation" {
			if gjson.GetBytes(lineBytes, "operation").Str == "enqueue" {
				contentStr := gjson.GetBytes(lineBytes, "content").Str
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
						subagentMap[strings.Clone(tuid)] = "agent-" + strings.Clone(taskID)
					}
				}
			}
			continue
		}

		// Collect agent_progress events for subagent mapping.
		// Claude Code v2.1+ emits these instead of queue-operation for Agent tool calls.
		if entryType == "progress" {
			if gjson.GetBytes(lineBytes, "data.type").Str == "agent_progress" {
				tuid := gjson.GetBytes(lineBytes, "parentToolUseID").Str
				agentID := gjson.GetBytes(lineBytes, "data.agentId").Str
				if tuid != "" && agentID != "" {
					subagentMap[strings.Clone(tuid)] = "agent-" + strings.Clone(agentID)
				}
			}
			continue
		}

		// Collect queued_command attachments — user messages
		// the user typed mid-tool-call. Other attachment types
		// (e.g. task_reminder) are intentionally dropped.
		if entryType == "attachment" {
			if qc, ok := extractQueuedCommand(string(lineBytes)); ok {
				qc.prompt = strings.Clone(qc.prompt)
				queuedCommands = append(queuedCommands, qc)
			}
			continue
		}

		// Handle system records. /rename local commands update the
		// display name; last rename wins (empty arg clears it).
		if entryType == "system" {
			if name, ok := extractRenameName(
				gjson.GetBytes(lineBytes, "content").Str,
			); ok {
				displayName = strings.Clone(name)
			}
			continue
		}

		if entryType != "user" && entryType != "assistant" {
			continue
		}
		line := resolveClaudePersistedToolResults(
			path, compactClaudeEntry(lineBytes),
		)

		// Collect subagent links and cwd/gitBranch from user entries.
		if entryType == "user" {
			collectToolResultAgentID(line, subagentMap)
			if cwd == "" {
				cwd = strings.Clone(gjson.GetBytes(lineBytes, "cwd").Str)
			}
			if gitBranch == "" {
				gitBranch = strings.Clone(gjson.GetBytes(lineBytes, "gitBranch").Str)
			}
		}

		// Capture sourceSessionID from first sessionId seen,
		// then check whether it differs from the file-derived
		// ID to detect parent sessions.
		if !foundParentSID {
			if sid := gjson.GetBytes(lineBytes, "sessionId").Str; sid != "" {
				foundParentSID = true
				sourceSessionID = strings.Clone(sid)
				if sid != sessionID {
					parentSessionID = strings.Clone(sid)
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
			entryType:  strings.Clone(entryType),
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
	isTruncated := lastLineHasData &&
		!lastLineValid &&
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
		agentLabel:      agentLabel,
		entrypoint:      entrypoint,
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

type claudeCompactField struct {
	name   string
	result gjson.Result
}

func compactClaudeEntry(line []byte) string {
	topFields := []claudeCompactField{
		{name: "uuid"}, {name: "parentUuid"}, {name: "timestamp"},
		{name: "isCompactSummary"}, {name: "isSidechain"},
		{name: "isMeta"}, {name: "requestId"},
	}
	messageFields := []claudeCompactField{
		{name: "content"}, {name: "id"}, {name: "stop_reason"},
		{name: "model"}, {name: "usage"},
	}
	snapshotFields := []claudeCompactField{
		{name: "timestamp"},
	}
	toolResultFields := []claudeCompactField{
		{name: "agentId"}, {name: "persistedOutputPath"},
	}

	// Fill all field groups in a single pass over the entry. This runs
	// for every retained message during a full parse, and one gjson
	// scan per field made it the dominant per-line parse cost. Only the
	// first occurrence of a key is kept, matching gjson.Get's
	// duplicate-key behavior.
	var seenMessage, seenSnapshot, seenToolResult bool
	gjson.Parse(string(line)).ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "message":
			if !seenMessage {
				seenMessage = true
				setClaudeCompactFields(messageFields, value)
			}
		case "snapshot":
			if !seenSnapshot {
				seenSnapshot = true
				setClaudeCompactFields(snapshotFields, value)
			}
		case "toolUseResult":
			if !seenToolResult {
				seenToolResult = true
				setClaudeCompactFields(toolResultFields, value)
			}
		default:
			setClaudeCompactField(topFields, key.Str, value)
		}
		return true
	})

	var b strings.Builder
	b.Grow(compactClaudeEntrySize(
		topFields, snapshotFields, messageFields, toolResultFields,
	))
	b.WriteByte('{')
	first := true
	writeClaudeCompactFields(&b, &first, topFields)
	writeClaudeCompactObject(&b, &first, "snapshot", snapshotFields)
	writeClaudeCompactObject(&b, &first, "message", messageFields)
	writeClaudeCompactObject(&b, &first, "toolUseResult", toolResultFields)
	b.WriteByte('}')
	return b.String()
}

// setClaudeCompactFields fills fields from the keys of an object
// value, keeping the first occurrence of each key.
func setClaudeCompactFields(fields []claudeCompactField, obj gjson.Result) {
	if !obj.IsObject() {
		return
	}
	obj.ForEach(func(key, value gjson.Result) bool {
		setClaudeCompactField(fields, key.Str, value)
		return true
	})
}

func setClaudeCompactField(
	fields []claudeCompactField, name string, value gjson.Result,
) {
	for i := range fields {
		if fields[i].name == name && !fields[i].result.Exists() {
			fields[i].result = value
			return
		}
	}
}

func compactClaudeEntrySize(groups ...[]claudeCompactField) int {
	size := 2
	for _, fields := range groups {
		for _, field := range fields {
			if field.result.Exists() {
				size += len(field.name) + len(field.result.Raw) + 4
			}
		}
	}
	return size
}

func writeClaudeCompactObject(
	b *strings.Builder, first *bool, name string, fields []claudeCompactField,
) {
	hasFields := false
	for _, field := range fields {
		if field.result.Exists() {
			hasFields = true
			break
		}
	}
	if !hasFields {
		return
	}
	writeClaudeCompactSeparator(b, first)
	b.WriteByte('"')
	b.WriteString(name)
	b.WriteString("\":{")
	nestedFirst := true
	writeClaudeCompactFields(b, &nestedFirst, fields)
	b.WriteByte('}')
}

func writeClaudeCompactFields(
	b *strings.Builder, first *bool, fields []claudeCompactField,
) {
	for _, field := range fields {
		if !field.result.Exists() {
			continue
		}
		writeClaudeCompactSeparator(b, first)
		b.WriteByte('"')
		b.WriteString(field.name)
		b.WriteString("\":")
		b.WriteString(field.result.Raw)
	}
}

func writeClaudeCompactSeparator(b *strings.Builder, first *bool) {
	if *first {
		*first = false
		return
	}
	b.WriteByte(',')
}

// lastAssistantStopReason returns the StopReason of the most
// recent assistant message in the slice, or "" when there is
// none. Used by Classify to decide between awaiting_user and
// clean for sessions that ended without an orphan tool_use.
func lastAssistantStopReason(messages []ParsedMessage) string {
	for _, v := range slices.Backward(messages) {
		if v.IsSystem {
			continue
		}
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
// already-stored rows (renames, late identity fields, and subagent-map
// repairs for tool calls outside the append).
var ErrClaudeIncrementalNeedsFullParse = fmt.Errorf(
	"incremental parse: appended Claude lines require full parse",
)

type ClaudeSubagentLink struct {
	ToolUseID         string
	SubagentSessionID string
	ResultContentRaw  string
	ResultContentLen  int
	HasResult         bool
}

// claudeStoredIdentity carries the session identity values already
// persisted for the session being incrementally parsed. Identity is
// first-non-empty-wins across the file, so an appended identity field can
// only change the stored session when the corresponding stored value is
// still empty.
type claudeStoredIdentity struct {
	agentLabel string
	entrypoint string
}

// claudeIncrementalScan carries the per-session stored state an
// incremental parse needs beyond the file path and byte offset.
type claudeIncrementalScan struct {
	startOrdinal  int
	lastEntryUUID string
	stored        claudeStoredIdentity
	// storedLinearParse mirrors the session's persisted
	// claude_linear_parse flag: whether the last full parse fell back
	// to linear processing (multi-root or unresolvable-parent DAG).
	// Linearity is monotonic — appends can only add roots or
	// unresolvable references, never repair them — so true lets the
	// incremental path skip fork detection: the full parser processes
	// such files in line order regardless of parent uuids. nil
	// (unknown, legacy rows) or false keeps strict fork detection.
	storedLinearParse *bool
	// storedTailClaudeMessageID is the provider message id of the last
	// stored assistant message for this session (empty when none), or
	// nil when the call site cannot supply it. The queued-command
	// masking fallback only matters when the appended assistant head
	// continues exactly this message id; a fresh id cannot be a hidden
	// continuation, so such appends stay incremental. nil keeps the
	// conservative fallback.
	storedTailClaudeMessageID *string
}

func claudeParseSessionFrom(
	path string,
	offset int64,
	scan claudeIncrementalScan,
) ([]ParsedMessage, []ClaudeSubagentLink, time.Time, int64, error) {
	startOrdinal := scan.startOrdinal
	stored := scan.stored
	var (
		entries        []dagEntry
		queuedCommands []claudeQueuedCommand
		subagentMap    = make(map[string]string)
		lineIndex      = startOrdinal
		// Track latest timestamp from all lines, including
		// non-message events (progress, queue-operation) so
		// callers can update ended_at even when no new
		// messages are found.
		latestTS               time.Time
		sawRename              bool
		sawSessionIdentityEdit bool
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
			if claudeSessionIdentityUpdate(line, stored) {
				sawSessionIdentityEdit = true
			}
			if entryType == "system" {
				if _, ok := extractRenameName(
					gjson.Get(line, "content").Str,
				); ok {
					sawRename = true
				}
				return
			}
			if entryType == "agent-setting" {
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
		return nil, nil, time.Time{}, 0, fmt.Errorf(
			"reading claude %s from offset %d: %w",
			path, offset, err,
		)
	}

	// Merge same-message.id streaming runs exactly as the full parser
	// does before any DAG work. Merging swallows chunk uuids, which
	// makes the merged entry's parent unresolvable — the same thing
	// happens in the full parser's post-merge uuid set, driving such
	// files to linear parsing. Runs that straddle a sync boundary are
	// detected by the engine's LastClaudeMessageID check on the first
	// appended assistant message; when a queued command would sort
	// ahead of that head and mask the check, the parser itself falls
	// back to a full parse (claudeQueuedCommandMasksSplitDetection).
	if len(entries) > 1 {
		entries = mergeClaudeAssistantMessageChunks(entries)
	}

	// A rename-only append produces no entries and no queued commands, so
	// the empty-entries early return below would silently succeed. Check
	// first and force a full parse so the display name is persisted.
	if sawRename {
		return nil, nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}
	if sawSessionIdentityEdit {
		return nil, nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}

	// Queue/progress events can repair subagent linkage on an already-stored
	// tool call. If the mapped tool_use_id is not introduced in this append,
	// incremental parsing would advance file_size without updating that row.
	if needsClaudeFullParseForSubagentMap(entries, subagentMap) {
		return nil, nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}

	if len(entries) == 0 && len(queuedCommands) == 0 {
		return nil, nil, latestTS, consumed, nil
	}

	// Fork detection only matters when the full parser would actually
	// walk the DAG. parseLinear-bound files — multi-root or with
	// unresolvable parents, which is every real CLI transcript whose
	// chain routes through attachment/system lines — are processed in
	// line order by the full parser too, so a linear append is exactly
	// equivalent and chain breaks are irrelevant there. For
	// DAG-resolvable or unknown (legacy) sessions, any break falls
	// back to the full parser, which re-decides and persists the flag.
	//
	// Linearity is monotonic under the transcript's causal write
	// order: a line's parentUuid always references an already-written
	// line, and appended lines carry fresh uuids, so an append can
	// never supply a previously-unresolved parent. The one
	// append-visible way a file can move toward resolvability is a
	// parentless entry adding a DAG root; guard that explicitly and
	// let the full parser re-derive the verdict. The reverse drift —
	// a DAG verdict flipping to linear — happens when an appended
	// entry lacks a uuid, since the full parser only walks the DAG
	// when every entry carries one; guard that in the DAG branch.
	linearBound := scan.storedLinearParse != nil && *scan.storedLinearParse
	dagBound := scan.storedLinearParse != nil && !*scan.storedLinearParse
	if linearBound {
		if appendAddsDAGRoot(entries) {
			return nil, nil, time.Time{}, 0, ErrDAGDetected
		}
	} else if (dagBound || scan.lastEntryUUID != "") &&
		appendMissingEntryUUID(entries) {
		return nil, nil, time.Time{}, 0, ErrDAGDetected
	} else if hasDAGFork(entries, scan.lastEntryUUID) {
		return nil, nil, time.Time{}, 0, ErrDAGDetected
	}

	links := collectClaudeSubagentLinks(entries)
	links = append(
		links, collectClaudeUnmatchedToolResults(entries, links)...,
	)

	msgs, _, endedAt := extractMessagesFrom(
		entries, startOrdinal,
	)
	annotateSubagentSessions(msgs, subagentMap)
	if len(queuedCommands) > 0 {
		// The engine's cross-sync split detection compares only the
		// FIRST returned message's ClaudeMessageID against the stored
		// tail. A queued command sorting ahead of an assistant head
		// would mask a same-message.id continuation and let the stored
		// partial response be appended a second time instead of
		// replaced — fall back to a full parse instead.
		if claudeQueuedCommandMasksSplitDetection(
			msgs, queuedCommands, scan.storedTailClaudeMessageID,
		) {
			return nil, nil, time.Time{}, 0,
				ErrClaudeIncrementalNeedsFullParse
		}
		msgs = mergeQueuedCommands(
			msgs, queuedCommands, startOrdinal, queuedCommandMessage,
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
	return msgs, links, endedAt, consumed, nil
}

// claudeSessionIdentityUpdate reports whether an appended line carries an
// identity value that could change the stored session. Identity is
// first-non-empty-wins, so a field whose stored value is already set can
// never be changed by an append; gating on the stored values keeps routine
// appends incremental even though real CLI transcripts carry a top-level
// entrypoint on most message lines.
func claudeSessionIdentityUpdate(line string, stored claudeStoredIdentity) bool {
	if stored.agentLabel == "" &&
		strings.TrimSpace(gjson.Get(line, "agentSetting").Str) != "" {
		return true
	}
	return stored.entrypoint == "" &&
		strings.TrimSpace(gjson.Get(line, "entrypoint").Str) != ""
}

// collectClaudeUnmatchedToolResults returns result links for appended
// tool_result blocks whose tool_use lives outside the appended window.
// In-append results pair at write time and agentId-linked results are
// already carried by collectClaudeSubagentLinks. isMeta carriers are
// skipped: the full parser drops those lines entirely, so their result
// content never reaches the stored tool call there either. Results
// whose tool_use id is unknown to the store no-op at apply time,
// matching the full parser's unpaired-result behavior.
func collectClaudeUnmatchedToolResults(
	entries []dagEntry, agentLinks []ClaudeSubagentLink,
) []ClaudeSubagentLink {
	linked := make(map[string]struct{}, len(agentLinks))
	for _, l := range agentLinks {
		linked[l.ToolUseID] = struct{}{}
	}
	appendedToolUse := make(map[string]struct{})
	var out []ClaudeSubagentLink
	for _, e := range entries {
		if e.entryType == "assistant" {
			content := gjson.Get(e.line, "message.content")
			if !content.IsArray() {
				continue
			}
			content.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").Str != "tool_use" {
					return true
				}
				if id := part.Get("id").Str; id != "" {
					appendedToolUse[id] = struct{}{}
				}
				return true
			})
			continue
		}
		if e.entryType != "user" || gjson.Get(e.line, "isMeta").Bool() {
			continue
		}
		content := gjson.Get(e.line, "message.content")
		if !content.IsArray() {
			continue
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str != "tool_result" {
				return true
			}
			result, ok := parseToolResult(part)
			if !ok {
				return true
			}
			if _, matched := appendedToolUse[result.ToolUseID]; matched {
				return true
			}
			if _, hasAgentLink := linked[result.ToolUseID]; hasAgentLink {
				return true
			}
			out = append(out, ClaudeSubagentLink{
				ToolUseID:        result.ToolUseID,
				ResultContentRaw: result.ContentRaw,
				ResultContentLen: result.ContentLength,
				HasResult:        true,
			})
			return true
		})
	}
	return out
}

func collectClaudeSubagentLinks(entries []dagEntry) []ClaudeSubagentLink {
	links := make([]ClaudeSubagentLink, 0, len(entries))
	for _, entry := range entries {
		if entry.entryType != "user" {
			continue
		}
		link, ok := extractToolResultAgentIDLink(entry.line)
		if !ok {
			continue
		}
		if gjson.Get(entry.line, "isMeta").Bool() {
			link.ResultContentRaw = ""
			link.ResultContentLen = 0
			link.HasResult = false
		}
		links = append(links, link)
	}
	return links
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

// appendAddsDAGRoot reports whether an appended message entry has a
// uuid but no parentUuid, i.e. it would add a root to the session's
// DAG. Root count is the only property of the stored linearity verdict
// an append can move toward resolvability, so linear-bound sessions
// fall back to a full parse (which re-derives the verdict) when one
// appears.
func appendAddsDAGRoot(entries []dagEntry) bool {
	for _, e := range entries {
		if e.uuid != "" && e.parentUuid == "" {
			return true
		}
	}
	return false
}

// appendMissingEntryUUID reports whether an appended message entry
// lacks a uuid. The full parser only walks the DAG when every entry
// carries a uuid (hasAnyUUID && allHaveUUID), so a uuid-less append
// flips a DAG-resolvable transcript to linear processing — restoring
// branches DAG processing had omitted. Sessions with a DAG verdict or
// a uuid-carrying stored tip force a full parse so the stored
// messages and the verdict are re-derived together. Linear-bound
// sessions and unknown sessions with a uuid-less stored tip skip
// this check: the former are already processed in line order, and
// the latter already fail allHaveUUID, so a uuid-less append cannot
// change the full parser's mode.
func appendMissingEntryUUID(entries []dagEntry) bool {
	for _, e := range entries {
		if e.uuid == "" {
			return true
		}
	}
	return false
}

// hasDAGFork returns true if the entries contain a fork —
// i.e. any entry whose parentUuid doesn't point to the
// immediately preceding entry's uuid. Linear UUID chains
// (each entry parenting the next) are safe for incremental
// parsing; forks require full DAG processing. Callers skip this
// check entirely for parseLinear-bound sessions (see
// claudeIncrementalScan.storedLinearParse).
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
			var skip bool
			text, skip = preprocessClaudeUserText(text)
			if skip {
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
					ToolResults:      trs,
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
	agentLabel      string
	entrypoint      string
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
	sess.AgentLabel = m.agentLabel
	sess.Entrypoint = m.entrypoint
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

	// Promoted system messages carry Role=user so role-keyed analytics
	// ignore them, but they are not real user turns;
	// firstMessageAndUserCount skips them when computing
	// user_message_count / first_message. It also skips leading
	// /clear and /effort command envelopes so the sidebar shows
	// the next real message instead of the command.
	firstMsg, userCount := firstMessageAndUserCount(messages)

	linear := true
	sess := ParsedSession{
		ID:                sessionID,
		Project:           project,
		Machine:           machine,
		Agent:             AgentClaude,
		ParentSessionID:   parentSessionID,
		FirstMessage:      firstMsg,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		MessageCount:      len(messages),
		UserMessageCount:  userCount,
		File:              fileInfo,
		ClaudeLinearParse: &linear,
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

		linear := false
		sess := ParsedSession{
			ID:                sid,
			Project:           project,
			Machine:           machine,
			Agent:             AgentClaude,
			ParentSessionID:   pSID,
			RelationshipType:  relType,
			FirstMessage:      firstMsg,
			StartedAt:         startedAt,
			EndedAt:           endedAt,
			MessageCount:      len(messages),
			UserMessageCount:  userCount,
			File:              fileInfo,
			ClaudeLinearParse: &linear,
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
	link, ok := extractToolResultAgentIDLink(line)
	if !ok {
		return
	}
	if _, exists := subagentMap[link.ToolUseID]; !exists {
		subagentMap[link.ToolUseID] = link.SubagentSessionID
	}
}

func extractToolResultAgentIDLink(line string) (ClaudeSubagentLink, bool) {
	agentID := gjson.Get(line, "toolUseResult.agentId").Str
	if agentID == "" {
		return ClaudeSubagentLink{}, false
	}
	sessionID := agentID
	if !strings.HasPrefix(sessionID, "agent-") {
		sessionID = "agent-" + sessionID
	}

	content := gjson.Get(line, "message.content")
	if !content.IsArray() {
		return ClaudeSubagentLink{}, false
	}
	var toolResult ParsedToolResult
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").Str != "tool_result" {
			return true
		}
		result, ok := parseToolResult(block)
		if !ok {
			return true
		}
		if toolResult.ToolUseID != "" {
			toolResult = ParsedToolResult{}
			return false
		}
		toolResult = result
		return true
	})
	if toolResult.ToolUseID == "" {
		return ClaudeSubagentLink{}, false
	}
	return ClaudeSubagentLink{
		ToolUseID:         toolResult.ToolUseID,
		SubagentSessionID: sessionID,
		ResultContentRaw:  toolResult.ContentRaw,
		ResultContentLen:  toolResult.ContentLength,
		HasResult:         true,
	}, true
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
	var skip bool
	prompt, skip = preprocessClaudeUserText(prompt)
	if skip || strings.TrimSpace(prompt) == "" {
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
	merged := mergeQueuedCommands(r.Messages, queued, 0, queuedCommandMessage)
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
	buildMessage func(claudeQueuedCommand) ParsedMessage,
) []ParsedMessage {
	out := make([]ParsedMessage, 0, len(messages)+len(queued))
	i, j := 0, 0
	for i < len(messages) && j < len(queued) {
		if queuedBefore(queued[j], messages[i]) {
			out = append(out, buildMessage(queued[j]))
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
		out = append(out, buildMessage(queued[j]))
	}
	for k := range out {
		out[k].Ordinal = startOrdinal + k
	}
	return out
}

// claudeQueuedCommandMasksSplitDetection reports whether merging
// queued commands would sort one ahead of a leading assistant message
// that carries a provider message id. The engine's cross-sync split
// detection (LastClaudeMessageID) inspects only the first appended
// message, so a displaced assistant head would hide a same-message.id
// continuation of the stored tail and duplicate the partial response.
// Real CLI transcripts write queued_command attachments mid-stream,
// between chunks of one response, so this masking is reachable
// whenever the sync boundary falls inside such a run.
//
// Masking only matters when the head actually continues the stored
// tail's message id: a fresh id is a new response, appending is correct
// regardless of the queued command's sort position, and forcing a full
// parse would make every routine queued-command turn scale with
// transcript size. When the stored tail id is unavailable (nil) the
// check stays conservative and treats any displaced head as masked.
func claudeQueuedCommandMasksSplitDetection(
	msgs []ParsedMessage,
	queued []claudeQueuedCommand,
	storedTailID *string,
) bool {
	if len(msgs) == 0 {
		return false
	}
	head := msgs[0]
	if head.Role != RoleAssistant || head.ClaudeMessageID == "" {
		return false
	}
	if storedTailID != nil && head.ClaudeMessageID != *storedTailID {
		return false
	}
	for _, qc := range queued {
		if queuedBefore(qc, head) {
			return true
		}
	}
	return false
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
// queued_command attachment.
func queuedCommandMessage(
	q claudeQueuedCommand,
) ParsedMessage {
	q.prompt = stripLeadingClaudeSystemReminderContent(q.prompt)
	if subtype := classifyClaudeSystemMessage(q.prompt); subtype != "" {
		return ParsedMessage{
			Role:          RoleUser,
			Content:       q.prompt,
			Timestamp:     q.timestamp,
			IsSystem:      true,
			ContentLength: len(q.prompt),
			SourceType:    "system",
			SourceSubtype: subtype,
		}
	}
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
			var skip bool
			text, skip = preprocessClaudeUserText(text)
			if skip {
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
					ToolResults:      trs,
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

func extractTimestampBytes(line []byte) time.Time {
	tsStr := gjson.GetBytes(line, "timestamp").Str
	ts := parseTimestamp(tsStr)
	if ts.IsZero() {
		snapTsStr := gjson.GetBytes(line, "snapshot.timestamp").Str
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
	defer releaseLineReader(lr)

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

func preprocessClaudeUserText(content string) (string, bool) {
	trimmed := trimClaudeSystemMessagePrefix(content)
	remainder, stripped := stripLeadingClaudeSystemReminderBlocks(trimmed)
	if stripped && remainder != "" {
		trimmed = remainder
	}
	if cmdText, ok := extractCommandText(trimmed); ok {
		return cmdText, false
	}
	if isCommandEnvelope(trimmed) {
		return "", true
	}
	if stripped && remainder == "" {
		return content, false
	}
	if !stripped {
		return content, false
	}
	return trimmed, false
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
	trimmed := trimClaudeSystemMessagePrefix(content)
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
	case strings.HasPrefix(trimmed, "<system-reminder>"):
		remainder, stripped := stripLeadingClaudeSystemReminderBlocks(trimmed)
		if !stripped {
			return ""
		}
		if remainder != "" {
			return ""
		}
		return "system_reminder"
	case isStandaloneClaudeTaggedMessage(trimmed, "ide_opened_file"):
		return "ide_opened_file"
	case isStandaloneClaudeTaggedMessage(trimmed, "ide_selection"):
		return "ide_selection"
	}
	return ""
}

func isStandaloneClaudeTaggedMessage(content, tag string) bool {
	trimmed := strings.TrimSpace(content)
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	if !strings.HasPrefix(trimmed, openTag) ||
		!strings.HasSuffix(trimmed, closeTag) {
		return false
	}

	afterOpen := trimmed[len(openTag):]
	return strings.Index(afterOpen, closeTag) ==
		len(afterOpen)-len(closeTag)
}

func stripLeadingClaudeSystemReminderContent(content string) string {
	trimmed := trimClaudeSystemMessagePrefix(content)
	remainder, stripped := stripLeadingClaudeSystemReminderBlocks(trimmed)
	if stripped && remainder != "" {
		return remainder
	}
	return content
}

func stripLeadingClaudeSystemReminderBlocks(content string) (string, bool) {
	rest := trimClaudeSystemMessagePrefix(content)
	stripped := false
	for strings.HasPrefix(rest, "<system-reminder>") {
		closeIdx := strings.Index(rest, "</system-reminder>")
		if closeIdx < 0 {
			return "", false
		}
		rest = trimClaudeSystemMessagePrefix(
			rest[closeIdx+len("</system-reminder>"):],
		)
		stripped = true
	}
	return rest, stripped
}

func trimClaudeSystemMessagePrefix(content string) string {
	return strings.TrimLeftFunc(content, func(r rune) bool {
		return r == '\uFEFF' || unicode.IsSpace(r)
	})
}

// isClaudeSystemMessage returns true if the content matches
// a known system-injected user message pattern.
func isClaudeSystemMessage(content string) bool {
	trimmed := trimClaudeSystemMessagePrefix(content)
	if classifyClaudeSystemMessage(trimmed) != "" {
		return true
	}
	return strings.HasPrefix(trimmed, "<local-command-")
}
