package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VisualStudioCopilotVirtualPath pairs a trace file with one conversation ID.
// A single physical trace file can hold spans for multiple conversations, so
// each conversation is tracked as its own work item under this virtual path.
func VisualStudioCopilotVirtualPath(tracePath, conversationID string) string {
	conversationID = canonicalVisualStudioCopilotConversationID(conversationID)
	return VirtualSourcePath(tracePath, conversationID)
}

func canonicalVisualStudioCopilotConversationID(id string) string {
	if isVisualStudioCopilotVS2026SessionID(id) {
		return strings.ToLower(id)
	}
	return id
}

func sameVisualStudioCopilotConversationID(a, b string) bool {
	return canonicalVisualStudioCopilotConversationID(a) ==
		canonicalVisualStudioCopilotConversationID(b)
}

// SplitVisualStudioCopilotVirtualPath splits a <traceFile>#<conversationID>
// virtual source path into its physical trace file and conversation ID. It
// builds on the provider-neutral ParseVirtualSourcePath splitter and adds the
// Visual Studio Copilot validation that the container names a trace file and
// the source ID is a valid conversation ID. It returns ok=false for a plain
// trace-file path. Callers outside the parser package use it to detect and
// resolve the virtual paths Visual Studio Copilot stores for its sessions.
func SplitVisualStudioCopilotVirtualPath(
	sourcePath string,
) (tracePath, conversationID string, ok bool) {
	return splitVisualStudioCopilotVirtualPath(sourcePath)
}

// IsVisualStudioCopilotTraceFile reports whether path names a Visual Studio
// Copilot OpenTelemetry trace file. Callers outside the parser use it to detect
// a physical trace path whose synced sessions are stored under
// <traceFile>#<conversationID> virtual paths.
func IsVisualStudioCopilotTraceFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".jsonl") &&
		strings.Contains(base, "_VSGitHubCopilot_traces")
}

func isVisualStudioCopilotConversationPath(path string) bool {
	return IsVisualStudioCopilotTraceFile(path) ||
		isVisualStudioCopilotVS2026SessionPath(path)
}

// IsVisualStudioCopilotVS2026SessionPath reports whether path names a Visual
// Studio 2026 Copilot one-file conversation source.
func IsVisualStudioCopilotVS2026SessionPath(path string) bool {
	return isVisualStudioCopilotVS2026SessionPath(path)
}

func isVisualStudioCopilotVS2026SessionPath(path string) bool {
	if !isVisualStudioCopilotVS2026SessionFileName(filepath.Base(path)) {
		return false
	}

	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	if len(parts) < 4 {
		return false
	}
	return strings.EqualFold(parts[len(parts)-1], "sessions") &&
		strings.EqualFold(parts[len(parts)-3], "copilot-chat")
}

func isVisualStudioCopilotVS2026SessionFileName(name string) bool {
	return isVisualStudioCopilotVS2026SessionID(name)
}

func isVisualStudioCopilotVS2026SessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isVisualStudioCopilotVS2026Hex(c) {
				return false
			}
		}
	}
	return true
}

func isVisualStudioCopilotVS2026Hex(c rune) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F')
}

// ResolveSourceFilePath maps a stored session source path to a path that can
// be opened on disk. Visual Studio Copilot stores a
// <traceFile>#<conversationID> virtual path whose conversations share one
// physical trace file, and aider stores a <historyFile>#<runIdx> virtual
// path whose runs share one physical history file; both resolve to the
// physical file. Every other agent stores a real path, returned unchanged.
func ResolveSourceFilePath(storedPath string) string {
	if tracePath, _, ok := splitVisualStudioCopilotVirtualPath(storedPath); ok {
		return tracePath
	}
	if historyPath, _, ok := ParseAiderVirtualPath(storedPath); ok {
		return historyPath
	}
	return storedPath
}

type vsCopilotTraceLine struct {
	ResourceSpans []vsCopilotResourceSpan `json:"resourceSpans"`
}

type vsCopilotResourceSpan struct {
	ScopeSpans []vsCopilotScopeSpan `json:"scopeSpans"`
}

type vsCopilotScopeSpan struct {
	Spans []vsCopilotSpan `json:"spans"`
}

type vsCopilotSpan struct {
	TraceID           string               `json:"traceId"`
	SpanID            string               `json:"spanId"`
	Name              string               `json:"name"`
	StartTimeUnixNano string               `json:"startTimeUnixNano"`
	EndTimeUnixNano   string               `json:"endTimeUnixNano"`
	Attributes        []vsCopilotTraceAttr `json:"attributes"`
	attrMap           map[string]string    `json:"-"`
	start             time.Time            `json:"-"`
	end               time.Time            `json:"-"`
}

type vsCopilotTraceAttr struct {
	Key   string              `json:"key"`
	Value vsCopilotTraceValue `json:"value"`
}

type vsCopilotTraceValue struct {
	StringValue string `json:"stringValue"`
	IntValue    string `json:"intValue"`
	BoolValue   bool   `json:"boolValue"`
}

// parseConversation parses one conversation, gathering its spans from the given
// trace file and every sibling trace file in the same directory. File metadata
// is recorded against the conversation's virtual path so that each conversation
// in a shared trace file is tracked independently.
func parseVisualStudioCopilotConversation(
	tracePath, conversationID, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	conversationID = canonicalVisualStudioCopilotConversationID(conversationID)
	if conversationID == "" {
		return nil, nil, nil
	}
	if _, err := os.Stat(tracePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", tracePath, err)
	}

	// Fingerprint every sibling trace file before reading spans. A
	// conversation's transcript is rebuilt from all siblings, so the stored
	// size/mtime must span them; computing it first means a sibling appended
	// during the read shows up as a change on the next sync rather than being
	// hidden behind a fingerprint that already counts it.
	compositeSize, compositeMtime := VisualStudioCopilotTraceFingerprint(
		tracePath,
	)

	spans, err := visualStudioCopilotConversationSpans(tracePath, conversationID)
	if err != nil {
		return nil, nil, err
	}
	if len(spans) == 0 {
		return nil, nil, nil
	}

	messages := visualStudioCopilotTraceMessages(spans)
	if len(messages) == 0 {
		return nil, nil, nil
	}
	userMessageCount := 0
	for _, message := range messages {
		if message.Role == RoleUser {
			userMessageCount++
		}
	}

	startedAt, endedAt := visualStudioCopilotTraceBounds(spans)
	firstMessage := visualStudioCopilotTraceFirstMessage(
		spans, conversationID,
	)

	sess := &ParsedSession{
		ID:               "visualstudio-copilot:" + conversationID,
		Agent:            AgentVSCopilot,
		Project:          project,
		Machine:          machine,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userMessageCount,
		File: FileInfo{
			Path:  VisualStudioCopilotVirtualPath(tracePath, conversationID),
			Size:  compositeSize,
			Mtime: compositeMtime,
		},
	}
	accumulateMessageTokenUsage(sess, messages)

	return sess, messages, nil
}

// visualStudioCopilotConversationSpans gathers every span for one conversation
// from the given trace file plus its sibling trace files. A read error on the
// primary trace file or on any sibling is returned so the caller can surface it
// as a sync failure. Because a conversation's spans can live in any sibling and
// sessions are written with full message replacement, reconstructing from only
// the readable subset would overwrite an indexed conversation with a partial
// transcript, so a transient unreadable sibling must fail the parse instead.
func visualStudioCopilotConversationSpans(
	tracePath, conversationID string,
) ([]vsCopilotSpan, error) {
	own, err := readVisualStudioCopilotTraceSpans(tracePath)
	if err != nil {
		return nil, err
	}
	var spans []vsCopilotSpan
	for _, span := range own {
		if sameVisualStudioCopilotConversationID(
			span.attrMap["gen_ai.conversation.id"], conversationID,
		) {
			spans = append(spans, span)
		}
	}
	siblingSpans, err := visualStudioCopilotSiblingTraceSpans(
		tracePath, conversationID,
	)
	if err != nil {
		return nil, err
	}
	spans = append(spans, siblingSpans...)
	return spans, nil
}

// VisualStudioCopilotFileConversationIDs returns the distinct conversation IDs
// that appear in a single trace file, in first-seen order. A read or scan error
// is returned rather than reported as an empty file, so callers do not mistake
// an unreadable file for one with no conversations.
func VisualStudioCopilotFileConversationIDs(path string) ([]string, error) {
	spans, err := readVisualStudioCopilotTraceSpans(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, span := range spans {
		id := canonicalVisualStudioCopilotConversationID(
			span.attrMap["gen_ai.conversation.id"],
		)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// WriteVisualStudioCopilotConversationJSONL streams the trace data for one
// conversation across every sibling trace file in the representative trace's
// directory, since a conversation's spans can be split across rotated trace
// files. VS 2026 session-file paths already point at one conversation file, so
// they are filtered and exported directly. From each file it emits only the
// spans whose gen_ai.conversation.id matches the requested conversation: a line
// is written verbatim when all of its spans already belong to that
// conversation, otherwise it is re-encoded with only the matching spans so a
// batched OTLP line cannot disclose another conversation's or an id-less span's
// prompts, tool arguments, command output, or secrets. A sibling that vanished
// between listing and open is skipped; any other read error is returned. When
// no trace file in the directory contains the conversation (e.g. the
// representative trace was rotated away and no sibling holds it), it returns an
// os.ErrNotExist-wrapped error rather than succeeding with empty output, so
// callers can report a clear not-found error.
func WriteVisualStudioCopilotConversationJSONL(
	w io.Writer, tracePath, conversationID string,
) error {
	if isVisualStudioCopilotVS2026SessionPath(tracePath) {
		written, err := writeVisualStudioCopilotConversationFile(
			w, tracePath, conversationID,
		)
		if err != nil {
			return err
		}
		if written == 0 {
			return fmt.Errorf(
				"conversation %s not found in %s: %w",
				conversationID, tracePath, os.ErrNotExist,
			)
		}
		return nil
	}
	files, err := visualStudioCopilotSiblingTraceFiles(tracePath)
	if err != nil {
		return err
	}
	written := 0
	for _, file := range files {
		n, err := writeVisualStudioCopilotConversationFile(
			w, file, conversationID,
		)
		written += n
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	if written == 0 {
		return fmt.Errorf(
			"conversation %s not found in %s: %w",
			conversationID, filepath.Dir(tracePath), os.ErrNotExist,
		)
	}
	return nil
}

func writeVisualStudioCopilotConversationFile(
	w io.Writer, path, conversationID string,
) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	written := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	for scanner.Scan() {
		out, emit := visualStudioCopilotConversationLine(
			scanner.Bytes(), conversationID,
		)
		if !emit {
			continue
		}
		if _, err := w.Write(out); err != nil {
			return written, err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return written, err
		}
		written++
	}
	if err := scanner.Err(); err != nil {
		return written, fmt.Errorf("scan %s: %w", path, err)
	}
	return written, nil
}

// visualStudioCopilotConversationLine returns the bytes to export for one trace
// line, keeping only spans for conversationID. It returns (line, true) verbatim
// when every span already belongs to the conversation, a re-encoded line when
// some spans were dropped, or (nil, false) when no span matches or the line
// cannot be parsed. Container objects are decoded into raw messages so that
// re-encoding preserves every field of the spans that are kept.
func visualStudioCopilotConversationLine(
	line []byte, conversationID string,
) ([]byte, bool) {
	conversationID = canonicalVisualStudioCopilotConversationID(conversationID)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		return nil, false
	}
	rsRaw, ok := top["resourceSpans"]
	if !ok {
		return nil, false
	}
	var resourceSpans []json.RawMessage
	if err := json.Unmarshal(rsRaw, &resourceSpans); err != nil {
		return nil, false
	}
	kept, matched, modified := visualStudioCopilotFilterArray(
		resourceSpans, conversationID, visualStudioCopilotFilterResourceSpan,
	)
	if !matched {
		return nil, false
	}
	if !modified {
		return line, true
	}
	rsBytes, err := json.Marshal(kept)
	if err != nil {
		return nil, false
	}
	top["resourceSpans"] = rsBytes
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

// visualStudioCopilotFilterArray applies a per-element filter to a decoded array
// of OTLP container objects. It reports whether any element matched the
// conversation and whether the array changed (an element was dropped or
// rewritten). A nil element returned by filter is treated as dropped.
func visualStudioCopilotFilterArray(
	items []json.RawMessage,
	conversationID string,
	filter func(json.RawMessage, string) (json.RawMessage, bool, bool),
) ([]json.RawMessage, bool, bool) {
	kept := make([]json.RawMessage, 0, len(items))
	matched, modified := false, false
	for _, item := range items {
		out, m, mod := filter(item, conversationID)
		matched = matched || m
		modified = modified || mod || out == nil
		if out == nil {
			continue
		}
		kept = append(kept, out)
	}
	return kept, matched, modified
}

func visualStudioCopilotFilterResourceSpan(
	rs json.RawMessage, conversationID string,
) (json.RawMessage, bool, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rs, &m); err != nil {
		return nil, false, true
	}
	ssRaw, ok := m["scopeSpans"]
	if !ok {
		return nil, false, true
	}
	var scopeSpans []json.RawMessage
	if err := json.Unmarshal(ssRaw, &scopeSpans); err != nil {
		return nil, false, true
	}
	kept, matched, modified := visualStudioCopilotFilterArray(
		scopeSpans, conversationID, visualStudioCopilotFilterScopeSpan,
	)
	if len(kept) == 0 {
		return nil, matched, true
	}
	if !modified {
		return rs, matched, false
	}
	ssBytes, err := json.Marshal(kept)
	if err != nil {
		return nil, false, true
	}
	m["scopeSpans"] = ssBytes
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false, true
	}
	return out, matched, true
}

func visualStudioCopilotFilterScopeSpan(
	ss json.RawMessage, conversationID string,
) (json.RawMessage, bool, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ss, &m); err != nil {
		return nil, false, true
	}
	spansRaw, ok := m["spans"]
	if !ok {
		return nil, false, true
	}
	var spans []json.RawMessage
	if err := json.Unmarshal(spansRaw, &spans); err != nil {
		return nil, false, true
	}
	kept := make([]json.RawMessage, 0, len(spans))
	modified := false
	for _, sp := range spans {
		if sameVisualStudioCopilotConversationID(
			visualStudioCopilotSpanConversationID(sp), conversationID,
		) {
			kept = append(kept, sp)
		} else {
			modified = true
		}
	}
	if len(kept) == 0 {
		return nil, false, true
	}
	if !modified {
		return ss, true, false
	}
	spansBytes, err := json.Marshal(kept)
	if err != nil {
		return nil, false, true
	}
	m["spans"] = spansBytes
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false, true
	}
	return out, true, true
}

// visualStudioCopilotSpanConversationID extracts a span's gen_ai.conversation.id
// attribute, returning "" when the span carries no conversation id. An id-less
// span never matches a requested conversation, so it is dropped from exports.
func visualStudioCopilotSpanConversationID(span json.RawMessage) string {
	var s struct {
		Attributes []vsCopilotTraceAttr `json:"attributes"`
	}
	if err := json.Unmarshal(span, &s); err != nil {
		return ""
	}
	for _, attr := range s.Attributes {
		if attr.Key == "gen_ai.conversation.id" {
			return canonicalVisualStudioCopilotConversationID(
				attr.Value.StringValue,
			)
		}
	}
	return ""
}

func readVisualStudioCopilotTraceSpans(
	path string,
) ([]vsCopilotSpan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	var spans []vsCopilotSpan
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var trace vsCopilotTraceLine
		if err := json.Unmarshal([]byte(line), &trace); err != nil {
			return nil, fmt.Errorf(
				"decode %s line %d: %w", path, lineNo, err,
			)
		}
		for _, resourceSpan := range trace.ResourceSpans {
			for _, scopeSpan := range resourceSpan.ScopeSpans {
				for _, span := range scopeSpan.Spans {
					span.attrMap = vsCopilotTraceAttrs(span.Attributes)
					span.start = parseUnixNano(span.StartTimeUnixNano)
					span.end = parseUnixNano(span.EndTimeUnixNano)
					if span.attrMap["gen_ai.conversation.id"] == "" {
						continue
					}
					spans = append(spans, span)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return spans, nil
}

// visualStudioCopilotSiblingTraceSpans collects spans for one conversation from
// every sibling trace file in the directory. Any sibling read error is returned,
// including a sibling that vanished between directory listing and open: because
// sessions are written with full message replacement, reconstructing from the
// readable subset would overwrite an indexed conversation with a partial
// transcript and drop archived messages, so an incomplete read must fail the
// parse and be retried instead. Once the file is permanently gone it no longer
// appears in the listing, so the next parse succeeds and archive preservation in
// the sync engine guards the stored transcript.
func visualStudioCopilotSiblingTraceSpans(
	path, conversationID string,
) ([]vsCopilotSpan, error) {
	siblings, err := visualStudioCopilotSiblingTraceFiles(path)
	if err != nil {
		return nil, err
	}
	var spans []vsCopilotSpan
	for _, sibling := range siblings {
		if sibling == path {
			continue
		}
		candidateSpans, err := readVisualStudioCopilotTraceSpans(sibling)
		if err != nil {
			return nil, err
		}
		for _, span := range candidateSpans {
			if sameVisualStudioCopilotConversationID(
				span.attrMap["gen_ai.conversation.id"], conversationID,
			) {
				spans = append(spans, span)
			}
		}
	}
	return spans, nil
}

// visualStudioCopilotSiblingTraceFiles lists the trace files in a trace file's
// directory. A directory read error is returned rather than swallowed: silently
// treating it as "no siblings" would let the primary trace be reconstructed and
// written as a complete session even though sibling enumeration failed,
// defeating the partial-transcript guard.
func visualStudioCopilotSiblingTraceFiles(path string) ([]string, error) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !IsVisualStudioCopilotTraceFile(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// VisualStudioCopilotTraceFingerprint returns a composite size and mtime
// (nanoseconds) spanning every Visual Studio Copilot trace file in the
// directory holding tracePath. A conversation's transcript is rebuilt from all
// sibling trace files, so a skip fingerprint keyed only on the representative
// trace file would miss spans appended to, rotated into, or removed from a
// sibling. Summing sizes and taking the maximum mtime makes the fingerprint
// change on any of those events. It falls back to the single file's stat when
// the directory cannot be listed.
func VisualStudioCopilotTraceFingerprint(
	tracePath string,
) (size, mtime int64) {
	if isVisualStudioCopilotVS2026SessionPath(tracePath) {
		if info, err := os.Stat(tracePath); err == nil {
			return info.Size(), info.ModTime().UnixNano()
		}
		return 0, 0
	}
	size, mtime, err := visualStudioCopilotTraceFingerprint(tracePath, false)
	if err != nil {
		if info, statErr := os.Stat(tracePath); statErr == nil {
			return info.Size(), info.ModTime().UnixNano()
		}
		return 0, 0
	}
	return size, mtime
}

// VisualStudioCopilotTraceFingerprintStrict is like
// VisualStudioCopilotTraceFingerprint but returns any directory-enumeration or
// per-sibling stat error instead of falling back to the representative file's
// stat or skipping an unstattable sibling. Sync skip checks use it so a read
// error surfaces and is retried rather than being mistaken for an "unchanged"
// fingerprint: when the readable files still match the stored composite, a
// transient ReadDir or stat failure would otherwise be cached as a skip and
// leave the session stale. The best-effort fallback stays for display-only paths
// such as SourceMtime.
func VisualStudioCopilotTraceFingerprintStrict(
	tracePath string,
) (size, mtime int64, err error) {
	if isVisualStudioCopilotVS2026SessionPath(tracePath) {
		info, err := os.Stat(tracePath)
		if err != nil {
			return 0, 0, err
		}
		return info.Size(), info.ModTime().UnixNano(), nil
	}
	return visualStudioCopilotTraceFingerprint(tracePath, true)
}

func visualStudioCopilotTraceFingerprint(
	tracePath string, strict bool,
) (size, mtime int64, err error) {
	siblings, err := visualStudioCopilotSiblingTraceFiles(tracePath)
	if err != nil {
		return 0, 0, err
	}
	for _, sibling := range siblings {
		info, statErr := os.Stat(sibling)
		if statErr != nil {
			if strict {
				return 0, 0, statErr
			}
			continue
		}
		size += info.Size()
		if m := info.ModTime().UnixNano(); m > mtime {
			mtime = m
		}
	}
	return size, mtime, nil
}

func vsCopilotTraceAttrs(
	attrs []vsCopilotTraceAttr,
) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		value := attr.Value.StringValue
		if value == "" && attr.Value.IntValue != "" {
			value = attr.Value.IntValue
		}
		out[attr.Key] = value
	}
	return out
}

func visualStudioCopilotTraceMessages(
	spans []vsCopilotSpan,
) []ParsedMessage {
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].start.Before(spans[j].start)
	})

	executedToolIDs := visualStudioCopilotExecutedToolIDs(spans)
	preferredToolSpans := visualStudioCopilotPreferredToolSpans(spans)
	preferredChatSpans := visualStudioCopilotPreferredChatSpans(
		spans, executedToolIDs,
	)
	preferredChatUsage := visualStudioCopilotPreferredChatUsageSpans(
		spans, executedToolIDs, preferredChatSpans,
	)
	messages := make([]ParsedMessage, 0, len(spans))
	fallbackMessages := make([]ParsedMessage, 0, len(spans))
	seenUserPrompts := map[string]struct{}{}
	seenChatOutputs := map[string]struct{}{}
	seenChatUsage := map[string]struct{}{}
	seenToolSpans := map[string]struct{}{}
	for _, span := range spans {
		if prompt := visualStudioCopilotChatPrompt(span); prompt != "" {
			promptKey := visualStudioCopilotPromptKey(span, prompt)
			if _, seen := seenUserPrompts[promptKey]; !seen {
				messages = append(messages, ParsedMessage{
					Ordinal:       len(messages),
					Role:          RoleUser,
					Content:       prompt,
					Timestamp:     span.start,
					ContentLength: len(prompt),
				})
				seenUserPrompts[promptKey] = struct{}{}
			}
			if content, toolCalls := visualStudioCopilotChatOutput(span, executedToolIDs); content != "" || len(toolCalls) > 0 {
				messages = visualStudioCopilotAppendChatOutput(
					messages, seenChatOutputs, span, content, toolCalls,
					preferredChatSpans, executedToolIDs,
				)
			} else {
				messages = visualStudioCopilotAppendChatTurnUsage(
					messages, seenChatUsage, span,
					preferredChatSpans, preferredChatUsage,
				)
			}
			continue
		}

		if content, toolCalls := visualStudioCopilotChatOutput(span, executedToolIDs); content != "" || len(toolCalls) > 0 {
			messages = visualStudioCopilotAppendChatOutput(
				messages, seenChatOutputs, span, content, toolCalls,
				preferredChatSpans, executedToolIDs,
			)
			continue
		}

		contentSpan, toolKey := visualStudioCopilotToolEmission(
			span, preferredToolSpans,
		)
		if _, seen := seenToolSpans[toolKey]; seen {
			continue
		}
		content, toolCalls := visualStudioCopilotTraceContent(contentSpan)
		if content == "" && len(toolCalls) == 0 {
			continue
		}
		seenToolSpans[toolKey] = struct{}{}
		message := ParsedMessage{
			Role:    RoleAssistant,
			Content: content,
			// Anchor the timestamp to the span being iterated, which sets
			// this message's ordinal via append order. The content may come
			// from a more complete duplicate encountered later, but timing
			// the message by that later copy would let it jump ahead of
			// intervening messages.
			Timestamp:     span.start,
			HasToolUse:    len(toolCalls) > 0,
			ContentLength: len(content),
			ToolCalls:     toolCalls,
		}
		visualStudioCopilotApplyUsage(&message, contentSpan)
		if message.HasToolUse {
			message.Ordinal = len(messages)
			messages = append(messages, message)
		} else {
			message.Ordinal = len(fallbackMessages)
			fallbackMessages = append(fallbackMessages, message)
		}
	}
	if len(messages) == 0 {
		return fallbackMessages
	}
	return messages
}

// visualStudioCopilotAppendChatOutput appends one assistant message for a chat
// turn, skipping turns already emitted. A single conversation can be split
// across sibling trace files and a streaming chat span can be flushed to more
// than one file with a growing payload, so the turn is keyed on span identity
// and emitted from its richest copy. This prevents duplicate assistant messages
// and double-counted token usage while keeping the complete content and tool
// calls. The message is positioned by the iterated span so a later, richer copy
// supplies content without reordering the transcript.
func visualStudioCopilotAppendChatOutput(
	messages []ParsedMessage, seen map[string]struct{},
	span vsCopilotSpan, content string, toolCalls []ParsedToolCall,
	preferred map[string]vsCopilotSpan, executedToolIDs map[string]struct{},
) []ParsedMessage {
	key := visualStudioCopilotChatOutputIdentity(span, content)
	if _, ok := seen[key]; ok {
		return messages
	}
	seen[key] = struct{}{}
	emitSpan := span
	if best, ok := preferred[key]; ok {
		emitSpan = best
		content, toolCalls = visualStudioCopilotChatOutput(best, executedToolIDs)
	}
	message := ParsedMessage{
		Ordinal:       len(messages),
		Role:          RoleAssistant,
		Content:       content,
		Timestamp:     span.end,
		HasToolUse:    len(toolCalls) > 0,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
	}
	visualStudioCopilotApplyUsage(&message, emitSpan)
	return append(messages, message)
}

// visualStudioCopilotChatOutputIdentity identifies a chat turn for
// deduplication. Real spans carry trace and span IDs that stay stable across
// the sibling files one span is flushed to, so keying on identity collapses
// every flush of a turn to a single message regardless of how complete each
// copy was. Only when both IDs are absent does the output content key the
// entry, keeping genuinely distinct turns separate.
func visualStudioCopilotChatOutputIdentity(span vsCopilotSpan, content string) string {
	if span.TraceID != "" || span.SpanID != "" {
		return "id:" + span.TraceID + ":" + span.SpanID
	}
	return "content:" + content
}

// visualStudioCopilotPreferredChatSpans chooses one chat-output span per stable
// identity. A streaming chat span can be flushed to several sibling trace files
// with a growing payload, so the richest copy wins and the turn emits once with
// complete content, tool calls, and token usage rather than once per flush.
func visualStudioCopilotPreferredChatSpans(
	spans []vsCopilotSpan, executedToolIDs map[string]struct{},
) map[string]vsCopilotSpan {
	best := map[string]vsCopilotSpan{}
	for _, span := range spans {
		content, toolCalls := visualStudioCopilotChatOutput(span, executedToolIDs)
		if content == "" && len(toolCalls) == 0 {
			continue
		}
		key := visualStudioCopilotChatOutputIdentity(span, content)
		if current, ok := best[key]; !ok ||
			visualStudioCopilotPreferChatSpan(span, current, executedToolIDs) {
			best[key] = span
		}
	}
	return best
}

// visualStudioCopilotPreferChatSpan reports whether candidate carries a more
// complete chat output than current: more tool calls win, then longer text,
// then more complete token usage, then the later flush. The usage tie-breaker
// keeps two flushes with identical visible output from applying the leaner
// usage copy, which would undercount the turn's tokens.
func visualStudioCopilotPreferChatSpan(
	candidate, current vsCopilotSpan, executedToolIDs map[string]struct{},
) bool {
	candidateContent, candidateTools := visualStudioCopilotChatOutput(
		candidate, executedToolIDs,
	)
	currentContent, currentTools := visualStudioCopilotChatOutput(
		current, executedToolIDs,
	)
	if len(candidateTools) != len(currentTools) {
		return len(candidateTools) > len(currentTools)
	}
	if len(candidateContent) != len(currentContent) {
		return len(candidateContent) > len(currentContent)
	}
	return visualStudioCopilotPreferUsageSpan(candidate, current)
}

// visualStudioCopilotAppendChatTurnUsage records token usage for a chat turn
// whose only output is executed tool calls, which are shown via their
// execute_tool spans. Without this, the LLM turn that produced those calls -
// and its token usage and model - would be dropped from the transcript and
// usage totals. The turn is keyed on span identity and emitted once; a turn
// that produced visible output elsewhere already carries its usage on that
// message, so it is skipped here. Turns carrying no usage add nothing.
func visualStudioCopilotAppendChatTurnUsage(
	messages []ParsedMessage, seen map[string]struct{},
	span vsCopilotSpan, preferred, preferredUsage map[string]vsCopilotSpan,
) []ParsedMessage {
	key := visualStudioCopilotChatOutputIdentity(span, "")
	if _, ok := preferred[key]; ok {
		return messages
	}
	if _, ok := seen[key]; ok {
		return messages
	}
	usageSpan := span
	if best, ok := preferredUsage[key]; ok {
		usageSpan = best
	}
	if !visualStudioCopilotSpanHasUsage(usageSpan) {
		return messages
	}
	seen[key] = struct{}{}
	content := visualStudioCopilotChatSummary(usageSpan)
	message := ParsedMessage{
		Ordinal:       len(messages),
		Role:          RoleAssistant,
		Content:       content,
		Timestamp:     span.end,
		ContentLength: len(content),
	}
	visualStudioCopilotApplyUsage(&message, usageSpan)
	return append(messages, message)
}

// visualStudioCopilotPreferredChatUsageSpans chooses, per chat identity with no
// visible output, the span carrying the most complete token usage. A tool-only
// chat turn can be flushed to several sibling files with growing token counts,
// so the richest copy wins and the turn's usage is recorded once and in full
// rather than from whichever partial copy was seen first.
func visualStudioCopilotPreferredChatUsageSpans(
	spans []vsCopilotSpan, executedToolIDs map[string]struct{},
	visibleOutput map[string]vsCopilotSpan,
) map[string]vsCopilotSpan {
	best := map[string]vsCopilotSpan{}
	for _, span := range spans {
		if !visualStudioCopilotIsChatSpan(span) {
			continue
		}
		content, toolCalls := visualStudioCopilotChatOutput(span, executedToolIDs)
		if content != "" || len(toolCalls) > 0 {
			continue
		}
		if !visualStudioCopilotSpanHasUsage(span) {
			continue
		}
		key := visualStudioCopilotChatOutputIdentity(span, "")
		if _, ok := visibleOutput[key]; ok {
			continue
		}
		if current, ok := best[key]; !ok ||
			visualStudioCopilotPreferUsageSpan(span, current) {
			best[key] = span
		}
	}
	return best
}

// visualStudioCopilotPreferUsageSpan reports whether candidate carries more
// complete token usage than current: present output tokens win, then higher
// output tokens, then present and higher input tokens, then the later flush.
func visualStudioCopilotPreferUsageSpan(candidate, current vsCopilotSpan) bool {
	candOut, candHasOut := visualStudioCopilotTraceIntAttr(
		candidate, "gen_ai.usage.output_tokens",
	)
	curOut, curHasOut := visualStudioCopilotTraceIntAttr(
		current, "gen_ai.usage.output_tokens",
	)
	if candHasOut != curHasOut {
		return candHasOut
	}
	if candOut != curOut {
		return candOut > curOut
	}
	candIn, candHasIn := visualStudioCopilotTraceIntAttr(
		candidate, "gen_ai.usage.input_tokens",
	)
	curIn, curHasIn := visualStudioCopilotTraceIntAttr(
		current, "gen_ai.usage.input_tokens",
	)
	if candHasIn != curHasIn {
		return candHasIn
	}
	if candIn != curIn {
		return candIn > curIn
	}
	return candidate.end.After(current.end)
}

// visualStudioCopilotSpanHasUsage reports whether a span carries any token
// usage attributes.
func visualStudioCopilotSpanHasUsage(span vsCopilotSpan) bool {
	_, _, _, hasContext, hasOutput := visualStudioCopilotTraceUsage(span)
	return hasContext || hasOutput
}

// visualStudioCopilotIsChatSpan reports whether a span records a chat (LLM)
// turn rather than a tool execution or agent-level span.
func visualStudioCopilotIsChatSpan(span vsCopilotSpan) bool {
	return span.attrMap["gen_ai.operation.name"] == "chat" ||
		strings.HasPrefix(span.Name, "chat ")
}

// visualStudioCopilotPromptKey returns the dedup key for a chat span's user
// prompt. Visual Studio emits one user turn as several chat spans (one LLM call
// per step of a multi-step agent turn), each carrying the same last-user prompt,
// so the key must stay constant across a turn's spans yet differ across turns.
// The turn-root request id (copilot_chat.root_request_id) is exactly that key
// when present. Span identity is not a usable fallback: a spanId is per LLM call
// (keying on it would split one turn into several user messages), and a traceId
// is file-wide in this trace format, shared across conversations and not stable
// across rotated sibling files. So when no request id is present the prompt text
// is the only turn-stable signal left.
func visualStudioCopilotPromptKey(span vsCopilotSpan, prompt string) string {
	if requestID := strings.TrimSpace(span.attrMap["copilot_chat.root_request_id"]); requestID != "" {
		return "request:" + requestID
	}
	return "content:" + prompt
}

func visualStudioCopilotExecutedToolIDs(
	spans []vsCopilotSpan,
) map[string]struct{} {
	out := map[string]struct{}{}
	for _, span := range spans {
		if span.attrMap["gen_ai.tool.name"] == "" {
			continue
		}
		if id := span.attrMap["gen_ai.tool.call.id"]; id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

// visualStudioCopilotPreferredToolSpans chooses one span per tool call id. A
// single tool call can be flushed to several sibling trace files in different
// states; the most complete copy (one carrying a result, else the latest)
// wins, so the call emits once with its richest payload.
func visualStudioCopilotPreferredToolSpans(
	spans []vsCopilotSpan,
) map[string]vsCopilotSpan {
	best := map[string]vsCopilotSpan{}
	for _, span := range spans {
		if span.attrMap["gen_ai.tool.name"] == "" {
			continue
		}
		id := span.attrMap["gen_ai.tool.call.id"]
		if id == "" {
			continue
		}
		current, ok := best[id]
		if !ok || visualStudioCopilotPreferToolSpan(span, current) {
			best[id] = span
		}
	}
	return best
}

func visualStudioCopilotPreferToolSpan(candidate, current vsCopilotSpan) bool {
	candidateResult := strings.TrimSpace(candidate.attrMap["gen_ai.tool.call.result"]) != ""
	currentResult := strings.TrimSpace(current.attrMap["gen_ai.tool.call.result"]) != ""
	if candidateResult != currentResult {
		return candidateResult
	}
	return candidate.end.After(current.end)
}

// visualStudioCopilotToolEmission resolves a tool-branch span to the span whose
// content should be emitted and the key under which the emission is
// deduplicated. Tool calls collapse to their preferred span per call id, so a
// call duplicated across sibling files contributes its most complete copy once.
// Other spans dedupe on their own trace/span id: exact duplicates collapse
// while genuinely distinct spans stay separate. The caller anchors the emitted
// message to the iterated span's position, so a later preferred copy supplies
// content without reordering the transcript.
func visualStudioCopilotToolEmission(
	span vsCopilotSpan, preferred map[string]vsCopilotSpan,
) (vsCopilotSpan, string) {
	if span.attrMap["gen_ai.tool.name"] == "" ||
		span.attrMap["gen_ai.tool.call.id"] == "" {
		return span, "span:" + span.TraceID + ":" + span.SpanID
	}
	id := span.attrMap["gen_ai.tool.call.id"]
	if best, ok := preferred[id]; ok {
		return best, "call:" + id
	}
	return span, "call:" + id
}

func visualStudioCopilotApplyUsage(
	msg *ParsedMessage, span vsCopilotSpan,
) {
	if model := visualStudioCopilotTraceModel(span); model != "" {
		msg.Model = model
	}
	usage, contextTokens, outputTokens, hasContext, hasOutput :=
		visualStudioCopilotTraceUsage(span)
	if len(usage) == 0 {
		return
	}
	msg.TokenUsage = usage
	msg.ContextTokens = contextTokens
	msg.OutputTokens = outputTokens
	msg.HasContextTokens = hasContext
	msg.HasOutputTokens = hasOutput
	msg.tokenPresenceKnown = true
}

func visualStudioCopilotTraceModel(span vsCopilotSpan) string {
	if model := strings.TrimSpace(span.attrMap["gen_ai.response.model"]); model != "" {
		return model
	}
	return strings.TrimSpace(span.attrMap["gen_ai.request.model"])
}

func visualStudioCopilotTraceUsage(
	span vsCopilotSpan,
) (json.RawMessage, int, int, bool, bool) {
	input, hasInput := visualStudioCopilotTraceIntAttr(
		span, "gen_ai.usage.input_tokens",
	)
	output, hasOutput := visualStudioCopilotTraceIntAttr(
		span, "gen_ai.usage.output_tokens",
	)
	if !hasInput && !hasOutput {
		return nil, 0, 0, false, false
	}
	normalized := map[string]int{}
	if hasInput {
		normalized["input_tokens"] = input
	}
	if hasOutput {
		normalized["output_tokens"] = output
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, 0, 0, false, false
	}
	return data, input, output, hasInput, hasOutput
}

func visualStudioCopilotTraceIntAttr(
	span vsCopilotSpan, key string,
) (int, bool) {
	raw := strings.TrimSpace(span.attrMap[key])
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func visualStudioCopilotTraceContent(
	span vsCopilotSpan,
) (string, []ParsedToolCall) {
	if summary := visualStudioCopilotChatSummary(span); summary != "" {
		return summary, nil
	}

	if toolName := span.attrMap["gen_ai.tool.name"]; toolName != "" {
		call := ParsedToolCall{
			ToolUseID: span.attrMap["gen_ai.tool.call.id"],
			ToolName:  toolName,
			Category:  visualStudioCopilotToolCategory(toolName),
		}
		call.InputJSON = visualStudioCopilotTraceToolInput(span)
		if result := visualStudioCopilotToolResultText(
			span.attrMap["gen_ai.tool.call.result"],
		); result != "" {
			call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{
				ToolUseID: call.ToolUseID,
				Source:    "visualstudio-copilot",
				Status:    "completed",
				Content:   result,
				Timestamp: span.end,
			})
		}
		content := formatVSCodeCopilotToolCalls([]ParsedToolCall{call})
		return content, []ParsedToolCall{call}
	}

	if strings.HasPrefix(span.Name, "invoke_agent") {
		parts := []string{"GitHub Copilot turn"}
		if mode := span.attrMap["copilot_chat.mode"]; mode != "" {
			parts = append(parts, "mode: "+mode)
		}
		if model := span.attrMap["gen_ai.request.model"]; model != "" {
			parts = append(parts, "model: "+model)
		}
		if turns := span.attrMap["copilot_chat.turn_count"]; turns != "" {
			parts = append(parts, "turns: "+turns)
		}
		return strings.Join(parts, " | "), nil
	}

	return "", nil
}

func visualStudioCopilotTraceToolInput(
	span vsCopilotSpan,
) string {
	return visualStudioCopilotToolInputJSON(
		span.attrMap["gen_ai.tool.name"],
		span.attrMap["gen_ai.tool.call.arguments"],
	)
}

func visualStudioCopilotToolInputJSON(toolName, rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	args := compactJSONOrString(rawArgs)
	if m, ok := asStringAnyMap(args); ok {
		args = normalizeVisualStudioCopilotToolArgs(toolName, m)
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(data)
}

func normalizeVisualStudioCopilotToolArgs(
	toolName string, args map[string]any,
) map[string]any {
	out := make(map[string]any, len(args)+4)
	maps.Copy(out, args)
	switch toolName {
	case "get_file":
		if path := stringValue(args, "filename", "file", "path", "filePath"); path != "" {
			out["file_path"] = path
			out["message"] = path
			if out["path"] == nil {
				out["path"] = path
			}
		}
	case "file_search":
		if query := stringValue(args, "query", "pattern"); query != "" {
			out["pattern"] = query
			out["message"] = query
		}
		if queries := stringSliceValue(args, "queries"); len(queries) > 0 {
			message := strings.Join(queries, ", ")
			out["pattern"] = message
			out["message"] = message
		}
	case "run_command_in_terminal", "run_command", "runInTerminal", "run_build":
		if cmd := stringValue(args, "command", "cmd"); cmd != "" {
			out["command"] = cmd
		}
	case "apply_patch", "edit_file":
		if path := stringValue(args, "filename", "file", "path", "filePath"); path != "" {
			out["file_path"] = path
		}
		if patch := stringValue(args, "patch", "diff"); patch != "" {
			if diff, paths := visualStudioCopilotPatchDiff(patch); diff != "" {
				out["diff"] = diff
				if out["file_path"] == nil && len(paths) == 1 {
					out["file_path"] = paths[0]
				}
			} else {
				out["diff"] = patch
			}
		}
		if oldText := stringValue(args, "old_string", "old_str", "oldString", "oldStr"); oldText != "" {
			out["old_string"] = oldText
		}
		if newText := stringValue(args, "new_string", "new_str", "newString", "newStr"); newText != "" {
			out["new_string"] = newText
		}
	}
	return out
}

func visualStudioCopilotPatchDiff(patch string) (string, []string) {
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	var out []string
	var paths []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))
			if path != "" {
				paths = append(paths, path)
				out = append(out, "--- a/"+path, "+++ b/"+path)
			}
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			if path != "" {
				paths = append(paths, path)
				out = append(out, "--- /dev/null", "+++ b/"+path)
			}
		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			if path != "" {
				paths = append(paths, path)
				out = append(out, "--- a/"+path, "+++ /dev/null")
			}
		case line == "*** Begin Patch", line == "*** End Patch":
			continue
		case strings.HasPrefix(line, "*** "):
			continue
		default:
			out = append(out, line)
		}
	}
	if len(out) == 0 || len(paths) == 0 {
		return "", nil
	}
	return strings.TrimSpace(strings.Join(out, "\n")), paths
}

func visualStudioCopilotToolCategory(toolName string) string {
	return NormalizeToolCategory(
		normalizeVisualStudioCopilotToolName(toolName),
	)
}

func normalizeVisualStudioCopilotToolName(toolName string) string {
	switch toolName {
	case "get_file":
		return "read_file"
	case "file_search":
		return "grep"
	case "get_web_pages":
		return "read_web_page"
	case "run_command_in_terminal", "run_command", "runInTerminal", "run_build":
		return "shell"
	case "apply_patch", "edit_file":
		return "apply_patch"
	}
	return normalizeVSCodeToolName(toolName)
}

func visualStudioCopilotToolResultText(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(visualStudioCopilotResultText(decoded, 0))
}

func visualStudioCopilotResultText(value any, depth int) string {
	if depth > 8 || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := visualStudioCopilotResultText(item, depth+1); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		for _, key := range []string{
			"Content", "content", "Text", "text", "Output", "output",
			"Result", "result", "Value", "value",
		} {
			if text := visualStudioCopilotResultText(v[key], depth+1); text != "" {
				return text
			}
		}
	}
	return ""
}

func compactJSONOrString(value string) any {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return strings.TrimSpace(value)
}

func parseJSONObject(value string) map[string]any {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil
	}
	return decoded
}

func asStringAnyMap(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

func stringValue(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok {
			if s := strings.TrimSpace(value); s != "" {
				return s
			}
		}
	}
	return ""
}

func stringSliceValue(m map[string]any, key string) []string {
	values, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func visualStudioCopilotTraceBounds(
	spans []vsCopilotSpan,
) (time.Time, time.Time) {
	var startedAt time.Time
	var endedAt time.Time
	for _, span := range spans {
		if !span.start.IsZero() &&
			(startedAt.IsZero() || span.start.Before(startedAt)) {
			startedAt = span.start
		}
		if span.end.After(endedAt) {
			endedAt = span.end
		}
	}
	return startedAt, endedAt
}

func visualStudioCopilotTraceFirstMessage(
	spans []vsCopilotSpan, conversationID string,
) string {
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].start.Before(spans[j].start)
	})
	for _, span := range spans {
		if summary := visualStudioCopilotTraceSummary(span); summary != "" {
			return truncate(oneLineSummary(summary), 300)
		}
	}

	for _, span := range spans {
		if strings.HasPrefix(span.Name, "invoke_agent") {
			if summary := visualStudioCopilotInvokeSummary(span); summary != "" {
				return truncate(summary, 300)
			}
		}
	}
	return "Visual Studio Copilot conversation " +
		truncate(conversationID, 8)
}

func visualStudioCopilotTraceSummary(span vsCopilotSpan) string {
	if prompt := visualStudioCopilotChatPrompt(span); prompt != "" {
		return prompt
	}
	if summary := visualStudioCopilotChatSummary(span); summary != "" {
		return summary
	}

	toolName := span.attrMap["gen_ai.tool.name"]
	if toolName == "" {
		return ""
	}
	args := parseJSONObject(span.attrMap["gen_ai.tool.call.arguments"])
	switch toolName {
	case "get_file":
		if path := stringValue(args, "filename", "file", "path", "filePath"); path != "" {
			return "Read file: " + path
		}
	case "file_search":
		if queries := stringSliceValue(args, "queries"); len(queries) > 0 {
			return "Search files: " + strings.Join(queries, ", ")
		}
		if query := stringValue(args, "query", "pattern"); query != "" {
			return "Search files: " + query
		}
	case "run_command_in_terminal", "run_command", "runInTerminal":
		if cmd := stringValue(args, "command"); cmd != "" {
			return "Run command: " + cmd
		}
	case "run_build":
		return "Run build"
	case "apply_patch":
		if explanation := stringValue(args, "explanation"); explanation != "" {
			return "Apply patch: " + explanation
		}
		return "Apply patch"
	}

	if cmd := stringValue(args, "command"); cmd != "" {
		return visualStudioCopilotToolLabel(toolName) + ": " + cmd
	}
	if path := stringValue(args, "filename", "file", "path", "filePath"); path != "" {
		return visualStudioCopilotToolLabel(toolName) + ": " + path
	}
	if query := stringValue(args, "query", "pattern"); query != "" {
		return visualStudioCopilotToolLabel(toolName) + ": " + query
	}
	if explanation := stringValue(args, "explanation", "message"); explanation != "" {
		return visualStudioCopilotToolLabel(toolName) + ": " + explanation
	}
	return visualStudioCopilotToolLabel(toolName)
}

type vsCopilotChatMessage struct {
	Role         string              `json:"role"`
	Parts        []vsCopilotChatPart `json:"parts"`
	FinishReason string              `json:"finish_reason"`
}

type vsCopilotChatPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func visualStudioCopilotChatPrompt(span vsCopilotSpan) string {
	if !visualStudioCopilotIsChatSpan(span) {
		return ""
	}
	raw := span.attrMap["gen_ai.input.messages"]
	if raw == "" {
		return ""
	}
	var messages []vsCopilotChatMessage
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return ""
	}
	for _, v := range slices.Backward(messages) {
		if v.Role != "user" {
			continue
		}
		var parts []string
		for _, part := range v.Parts {
			if part.Content == "" {
				continue
			}
			parts = append(parts, strings.TrimSpace(part.Content))
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text != "" {
			return text
		}
	}
	return ""
}

func oneLineSummary(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func visualStudioCopilotChatOutput(
	span vsCopilotSpan, executedToolIDs map[string]struct{},
) (string, []ParsedToolCall) {
	if !visualStudioCopilotIsChatSpan(span) {
		return "", nil
	}
	raw := span.attrMap["gen_ai.output.messages"]
	if raw == "" {
		return "", nil
	}
	var messages []vsCopilotChatMessage
	if err := json.Unmarshal([]byte(raw), &messages); err != nil {
		return "", nil
	}

	var textParts []string
	var toolCalls []ParsedToolCall
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				if text := strings.TrimSpace(part.Content); text != "" {
					textParts = append(textParts, text)
				}
			case "tool_call":
				if part.Name == "" {
					continue
				}
				if _, ok := executedToolIDs[part.ID]; ok {
					continue
				}
				call := ParsedToolCall{
					ToolUseID: part.ID,
					ToolName:  part.Name,
					Category:  visualStudioCopilotToolCategory(part.Name),
				}
				call.InputJSON = visualStudioCopilotChatToolInput(
					part.Name, part.Arguments,
				)
				toolCalls = append(toolCalls, call)
			}
		}
	}

	content := strings.TrimSpace(strings.Join(textParts, "\n\n"))
	if len(toolCalls) > 0 {
		toolText := formatVSCodeCopilotToolCalls(toolCalls)
		if content == "" {
			content = toolText
		} else {
			content = toolText + "\n\n" + content
		}
	}
	return content, toolCalls
}

func visualStudioCopilotChatToolInput(
	toolName string, raw json.RawMessage,
) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return visualStudioCopilotToolInputJSON(toolName, asString)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	if m, ok := asStringAnyMap(decoded); ok {
		decoded = normalizeVisualStudioCopilotToolArgs(toolName, m)
	}
	data, err := json.Marshal(decoded)
	if err != nil {
		return ""
	}
	return string(data)
}

func visualStudioCopilotChatSummary(span vsCopilotSpan) string {
	if !visualStudioCopilotIsChatSpan(span) {
		return ""
	}
	client := visualStudioCopilotClientLabel(
		span.attrMap["copilot_chat.client_id"],
	)
	model := span.attrMap["gen_ai.request.model"]
	if model == "" {
		model = span.attrMap["gen_ai.response.model"]
	}
	parts := []string{"Visual Studio Copilot chat"}
	if client != "" {
		parts = append(parts, client)
	}
	if model != "" {
		parts = append(parts, model)
	}
	if rootID := span.attrMap["copilot_chat.root_request_id"]; rootID != "" {
		if len(rootID) > 8 {
			rootID = rootID[:8]
		}
		parts = append(parts, rootID)
	}
	return strings.Join(parts, " | ")
}

func visualStudioCopilotInvokeSummary(span vsCopilotSpan) string {
	mode := span.attrMap["copilot_chat.mode"]
	if mode == "" {
		mode = "Copilot"
	}
	client := visualStudioCopilotClientLabel(
		span.attrMap["copilot_chat.client_id"],
	)
	model := span.attrMap["gen_ai.request.model"]
	if model == "" {
		model = span.attrMap["gen_ai.response.model"]
	}
	parts := []string{"Visual Studio Copilot " + mode}
	if client != "" {
		parts = append(parts, client)
	}
	if model != "" {
		parts = append(parts, model)
	}
	if rootID := span.attrMap["copilot_chat.root_request_id"]; rootID != "" {
		if len(rootID) > 8 {
			rootID = rootID[:8]
		}
		parts = append(parts, rootID)
	}
	return strings.Join(parts, " | ")
}

func visualStudioCopilotClientLabel(clientID string) string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return ""
	}
	parts := strings.Split(clientID, ".")
	return parts[len(parts)-1]
}

func visualStudioCopilotToolLabel(toolName string) string {
	label := strings.TrimSpace(toolName)
	label = strings.TrimPrefix(label, "copilot_")
	label = strings.ReplaceAll(label, "_", " ")
	if label == "" {
		return "Tool"
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

func parseUnixNano(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	nanos, err := strconv.ParseInt(value, 10, 64)
	if err != nil || nanos <= 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}
