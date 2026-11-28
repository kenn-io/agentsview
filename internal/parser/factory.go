// ABOUTME: Parses Factory Droid JSONL session files into structured session data.
// Droid stores one <uuid>.jsonl per session under a per-cwd directory, with a
// session_start header and Anthropic-shaped message content blocks.
package parser

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// factorySettingsSuffix is the sidecar carrying session-level token
	// totals.
	factorySettingsSuffix = ".settings.json"
	// maxFactorySettingsBytes caps the sidecar read. Real sidecars are tens
	// of kilobytes even with hundreds of subagent entries.
	maxFactorySettingsBytes = 16 << 20
)

// parseFactorySession parses a Factory Droid <uuid>.jsonl transcript.
//
// Line types: session_start (id, title, cwd, and callingSessionId for
// subagent sessions), message (role + content blocks), and bookkeeping
// records (todo_state, agent_turn_outcome, compaction_state, session_end)
// that carry no transcript content. parentId links are linear in practice,
// so entries are read in file order.
func parseFactorySession(
	ctx context.Context, path, projectHint, machine string,
) ([]ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// The file name is the ID discovery and lookup use, so session_start.id
	// is not allowed to override it.
	rawID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	var (
		messages     []ParsedMessage
		title        string
		titlePresent bool
		cwd          string
		parentID     string
		startedAt    time.Time
		endedAt      time.Time
		ordinal      int
		userCount    int
		firstMsg     string
		malformed    int
		lastModel    string
		lastProvider string
	)

	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}
		switch gjson.Get(line, "type").Str {
		case "session_start":
			if t := gjson.Get(line, "title"); t.Exists() {
				title = strings.TrimSpace(t.Str)
				titlePresent = true
			}
			cwd = gjson.Get(line, "cwd").Str
			parentID = ""
			if id := gjson.Get(line, "callingSessionId").Str; IsValidSessionID(id) && id != rawID {
				parentID = id
			}
			continue
		case "message":
		default:
			// todo_state / agent_turn_outcome / compaction_state /
			// session_end: keep their timestamps for session bounds.
			if ts := parseTimestamp(gjson.Get(line, "timestamp").Str); !ts.IsZero() {
				startedAt = earlierTime(startedAt, ts)
				endedAt = laterTime(endedAt, ts)
			}
			continue
		}

		msg := gjson.Get(line, "message")
		role := RoleType(msg.Get("role").Str)
		if role != RoleUser && role != RoleAssistant {
			continue
		}
		ts := parseTimestamp(gjson.Get(line, "timestamp").Str)
		if !ts.IsZero() {
			startedAt = earlierTime(startedAt, ts)
			endedAt = laterTime(endedAt, ts)
		}
		// visibility=llm_only marks injected <system-reminder> context, not
		// something the human typed.
		visibility := msg.Get("visibility").Str
		if role == RoleUser && visibility == "llm_only" {
			continue
		}

		text, thinkingText, dialogueText, hasThinking, hasToolUse, tcs, trs := ExtractTextContent(ctx, msg.Get("content"))
		// A prompt with pasted images opens with an "Attached image local
		// file paths" reminder ahead of the typed text; keep the typed text
		// and mark each image, so an image-only prompt still shows.
		if role == RoleUser {
			if rest, ok := stripLeadingClaudeSystemReminderBlocks(text); ok {
				text, dialogueText = rest, rest
			}
			if marks := factoryImageMarks(msg.Get("content")); marks != "" {
				text = strings.TrimSpace(text + "\n" + marks)
				dialogueText = text
			}
		}
		if strings.TrimSpace(text) == "" && len(tcs) == 0 && len(trs) == 0 {
			continue
		}
		if role == RoleUser && isFactorySystemMessage(text) {
			continue
		}
		// visibility=user_only on a user text row is a Droid UI notice (BYOK
		// errors, MCP status, skill activation), not a typed prompt. The same
		// flag on tool_use/tool_result pairs marks nested Script tool calls,
		// which are real activity and stay as ordinary rows.
		isNotice := role == RoleUser && visibility == "user_only" &&
			len(tcs) == 0 && len(trs) == 0

		model := msg.Get("modelId").Str
		if model != "" {
			lastModel = model
		}
		if p := msg.Get("apiProvider").Str; p != "" {
			lastProvider = p
		}
		if role == RoleUser && text != "" && !isNotice {
			userCount++
			if firstMsg == "" {
				firstMsg = truncate(strings.ReplaceAll(text, "\n", " "), 300)
			}
		}
		messages = append(messages, ParsedMessage{
			Ordinal:         ordinal,
			Role:            role,
			Content:         text,
			DialogueText:    new(dialogueText),
			ThinkingText:    thinkingText,
			Timestamp:       ts,
			Model:           model,
			ReasoningEffort: msg.Get("reasoningEffort").Str,
			HasThinking:     hasThinking,
			HasToolUse:      hasToolUse,
			IsSystem:        isNotice,
			ContentLength:   len(text),
			ToolCalls:       tcs,
			ToolResults:     trs,
			SourceUUID:      gjson.Get(line, "id").Str,
		})
		ordinal++
	}
	if err := lr.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	sessionID := "factory:" + rawID
	project := projectHint
	if cwd != "" {
		if p := ExtractProjectFromCwdWithBranchContext(ctx, cwd, ""); p != "" {
			project = p
		}
	}
	if project == "" {
		project = GetProjectName(filepath.Base(filepath.Dir(path)))
	}

	sess := ParsedSession{
		ID:                 sessionID,
		Project:            project,
		Machine:            machine,
		Agent:              AgentFactory,
		FirstMessage:       firstMsg,
		SessionName:        title,
		SessionNamePresent: titlePresent,
		Cwd:                cwd,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		MessageCount:       len(messages),
		UserMessageCount:   userCount,
		MalformedLines:     malformed + lr.SkippedLines(),
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	if parentID != "" {
		sess.ParentSessionID = "factory:" + parentID
		sess.RelationshipType = RelSubagent
	}
	// A subagent's usage is already in its parent's sidecar, which reports
	// every child whether or not the child left a transcript. Counting it
	// only there keeps each child counted once however its files come and go.
	var usageEvents []ParsedUsageEvent
	if parentID == "" {
		usageEvents = factoryUsageEvents(
			ctx, path, sessionID, lastModel, lastProvider, startedAt, endedAt,
		)
	}
	// Project lookup and the subagent loop stop early on cancellation; return
	// the error rather than a partial session.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Every transcript is always exactly one session, even with no visible
	// messages or usage: the sidebar already hides zero-message sessions,
	// and returning nothing would leave a previously stored session's
	// messages or usage in place after the transcript or sidecar loses them.
	// The sidecar holds cumulative totals, not per-request usage, so peak
	// context stays unknown.
	sess.TotalOutputTokens, sess.HasTotalOutputTokens, _, _ = UsageEventTokenAggregate(usageEvents)
	return []ParseResult{{
		Session: sess, Messages: messages, UsageEvents: usageEvents,
	}}, nil
}

// factoryUsageEvents reads the <uuid>.settings.json sidecar. Droid keeps
// only session-cumulative totals there (no per-message usage in the
// transcript), so emit one aggregate event per session, dated at the
// session end, and let the pricing catalog cost it.
//
// Subagents started from this session usually have no transcript of their
// own; their totals live only in childInclusiveTokenUsageBySessionId. Each
// child becomes its own event on this session, priced as the session model
// because the sidecar does not record the child's model.
//
// Every event uses the "session" source: these are cumulative summaries
// spanning many requests, so per-request token bounds and per-turn rollups
// must not apply to them.
func factoryUsageEvents(
	ctx context.Context, transcriptPath, sessionID, model, provider string,
	startedAt, endedAt time.Time,
) []ParsedUsageEvent {
	settings, ok := readFactorySettings(transcriptPath)
	if !ok {
		return nil
	}
	if m := settings.Get("model").Str; m != "" {
		model = m
	}
	// providerLock is the current key; apiProviderLock is kept for
	// transcripts written by earlier Droid releases.
	if p := firstNonEmptyJSONLString(
		settings.Get("providerLock").Str, settings.Get("apiProviderLock").Str,
	); p != "" {
		provider = p
	}
	if model == "" {
		model = "factory-unknown"
	}
	base := ParsedUsageEvent{
		SessionID:  sessionID,
		Model:      model,
		ProviderID: provider,
		OccurredAt: timeString(endedAt, startedAt),
	}

	var events []ParsedUsageEvent
	if ev, ok := factoryUsageEvent(
		base, settings.Get("tokenUsage"),
		"session:"+sessionID+":settings:"+model,
	); ok {
		events = append(events, ev)
	}
	settings.Get("childInclusiveTokenUsageBySessionId").ForEach(
		func(key, usage gjson.Result) bool {
			if ctx.Err() != nil {
				return false
			}
			childID := key.Str
			if !IsValidSessionID(childID) {
				return true
			}
			if ev, ok := factoryUsageEvent(
				base, usage,
				"session:"+sessionID+":subagent:"+childID,
			); ok {
				events = append(events, ev)
			}
			return true
		},
	)
	return events
}

// factoryUsageEvent fills base with one Droid tokenUsage object. It reports
// false when the object is missing or carries no tokens.
func factoryUsageEvent(
	base ParsedUsageEvent, usage gjson.Result, dedupKey string,
) (ParsedUsageEvent, bool) {
	if !usage.IsObject() {
		return ParsedUsageEvent{}, false
	}
	ev := base
	ev.Source = "session"
	ev.DedupKey = dedupKey
	ev.InputTokens = factoryTokenCount(usage, "inputTokens")
	ev.OutputTokens = factoryTokenCount(usage, "outputTokens")
	ev.CacheCreationInputTokens = factoryTokenCount(usage, "cacheCreationTokens")
	ev.CacheReadInputTokens = factoryTokenCount(usage, "cacheReadTokens")
	ev.ReasoningTokens = factoryTokenCount(usage, "thinkingTokens")
	if ev.InputTokens+ev.OutputTokens+
		ev.CacheCreationInputTokens+ev.CacheReadInputTokens == 0 {
		return ParsedUsageEvent{}, false
	}
	return ev, true
}

// factoryImageMarks returns one "[image]" marker per image block, joined by
// spaces, or "" when the content has no images.
func factoryImageMarks(content gjson.Result) string {
	var marks []string
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").Str == "image" {
			marks = append(marks, "[image]")
		}
		return true
	})
	return strings.Join(marks, " ")
}

// factoryTokenCount reads one counter, flooring negative or non-numeric
// values at zero.
func factoryTokenCount(usage gjson.Result, key string) int {
	return max(int(usage.Get(key).Int()), 0)
}

// readFactorySettings reads a settings sidecar. The sidecar is untrusted
// input like the transcript: only a regular file is opened, so a FIFO or
// device cannot block the parse, and the read is capped. A missing,
// unreadable, oversized, or invalid sidecar yields no usage rather than
// failing the transcript.
func readFactorySettings(transcriptPath string) (gjson.Result, bool) {
	path := factorySettingsPath(transcriptPath)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return gjson.Result{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return gjson.Result{}, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return gjson.Result{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFactorySettingsBytes+1))
	if err != nil {
		return gjson.Result{}, false
	}
	if int64(len(data)) > maxFactorySettingsBytes {
		log.Printf("factory: skipping usage for %s: settings exceed %d bytes",
			path, maxFactorySettingsBytes)
		return gjson.Result{}, false
	}
	if !gjson.ValidBytes(data) {
		return gjson.Result{}, false
	}
	return gjson.ParseBytes(data), true
}

func factorySettingsPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl") + factorySettingsSuffix
}

// isFactorySystemMessage reports a user row that is still Droid-injected
// reminder text after closed reminder blocks were stripped, such as an
// unterminated <system-reminder>.
func isFactorySystemMessage(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "<system-reminder>")
}
