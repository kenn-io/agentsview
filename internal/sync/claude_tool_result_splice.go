package sync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Claude Code moves a large tool output to a side file under the session's
// tool-results folder and leaves a short placeholder in the transcript
// (parser.IsPersistedToolResultPlaceholder). The parser reads the side file
// back in while it exists, so the sync that first sees the result archives
// the full output. Claude Code deletes those side files after its cleanup
// period, and a later full re-parse of the transcript then holds only the
// placeholder.
//
// The archive is often the only copy of that output, so a placeholder may
// never replace archived text. The rule is per result, not per session: a
// resumed session's new messages, new calls and corrections still land, and a
// side file that is present and changed replaces the archived text, because
// the parser has read it and the result is no longer a placeholder.
//
// A blocked result category (EngineConfig.BlockedResultCategories) archives
// no text, only the result's length. When the archive holds only that length,
// the placeholder's length may not replace it either.

// claudeResultSplice is what spliceArchivedClaudeToolResults found for one
// Claude write.
type claudeResultSplice struct {
	// msgs are the parsed messages, with archived text written back in place
	// of each placeholder result that has it.
	msgs []parser.ParsedMessage
	// lengths holds, by tool_use_id, the archived call of each placeholder
	// result whose archive stored a length but no text.
	lengths map[string]archivedResultLength
	// kept counts the results whose archived text was written back.
	kept       int
	agent      parser.AgentType
	archivedID string
}

// archivedResultLength is an archived call's result stored without its text,
// as a blocked category stores it: the call's length and its result events,
// whose lengths and raw digests are kept by event index.
type archivedResultLength struct {
	length int
	events []db.ToolResultEvent
}

// spliceArchivedClaudeToolResults returns the parsed messages of a Claude
// write with each placeholder result replaced by the archived result of the
// call with the same tool_use_id, when that archived result holds real text.
// The archived text replaces the parsed result's raw content and length
// before ingest.PrepareCandidate pairs results with calls, so the
// blocked-category and image policies and the derived lengths apply to it
// exactly as to parsed text. A placeholder whose archived call holds only a
// length is listed in the returned lengths; pairing measures a length from
// text, so keepArchivedClaudeResultLengths writes it back after pairing.
//
// It returns pw.msgs unchanged for other agents, when no parsed result is a
// placeholder (so the archive is read only when one is present), and during
// the raw archive's scratch reparse (EngineConfig.ArchiveReparse), which
// derives sessions only from verified captured inputs and so takes no text
// from the archive. When it splices a result it returns a copy; pw.msgs
// itself is never modified.
//
// The archive is e.historyStore: the old database during a rebuild, the live
// one otherwise.
func (e *Engine) spliceArchivedClaudeToolResults(
	ctx context.Context, pw pendingWrite,
) (claudeResultSplice, error) {
	splice := claudeResultSplice{msgs: pw.msgs, agent: pw.sess.Agent}
	if pw.sess.Agent != parser.AgentClaude || e.archiveReparse ||
		!hasPersistedToolResultPlaceholder(pw.msgs) {
		return splice, nil
	}
	splice.archivedID = applyIDPrefixToID(e.idPrefix, pw.sess.ID)
	archivedRows, err := e.historyStore().GetAllMessages(ctx, splice.archivedID)
	if err != nil {
		return claudeResultSplice{}, err
	}
	texts, lengths := archivedClaudeToolResults(archivedRows)
	if len(texts) == 0 && len(lengths) == 0 {
		return splice, nil
	}
	msgs := pw.msgs
	cloned := false
	for i := range msgs {
		resultsCloned := false
		for j := range msgs[i].ToolResults {
			result := msgs[i].ToolResults[j]
			text, hasText := texts[result.ToolUseID]
			length, hasLength := lengths[result.ToolUseID]
			if (!hasText && !hasLength) || !persistedPlaceholderResult(result) {
				continue
			}
			if !hasText {
				if splice.lengths == nil {
					splice.lengths = make(map[string]archivedResultLength)
				}
				splice.lengths[result.ToolUseID] = length
				continue
			}
			raw, err := json.Marshal(text)
			if err != nil {
				return claudeResultSplice{}, err
			}
			if !cloned {
				msgs = slices.Clone(msgs)
				cloned = true
			}
			if !resultsCloned {
				msgs[i].ToolResults = slices.Clone(msgs[i].ToolResults)
				resultsCloned = true
			}
			msgs[i].ToolResults[j].ContentRaw = string(raw)
			msgs[i].ToolResults[j].ContentLength = len(text)
			splice.kept++
		}
	}
	splice.msgs = msgs
	return splice, nil
}

// keepArchivedClaudeResultLengths finishes a splice on the paired candidate
// rows, after ingest.PrepareCandidate. For each call listed in
// splice.lengths whose category is blocked now, so its text is blank, it sets
// the call's length and each blank result event's length (and raw digest)
// to the archived values: the archived event at the same index, or the
// call's length when the call has one event. The text stays blank, and the
// write keeps a length supplied with blank text
// (db.ResolveResultContentLength). It then logs the session once when any
// archived result, text or length, was kept, naming the length-only count
// only when it is not zero.
func (e *Engine) keepArchivedClaudeResultLengths(
	splice claudeResultSplice, messages []db.Message,
) {
	lengthOnly := 0
	for i := range messages {
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			archived, ok := splice.lengths[call.ToolUseID]
			if !ok || !e.blockedResultCategories[call.Category] ||
				call.ResultContent != "" ||
				(call.ResultContentLength == 0 && len(call.ResultEvents) == 0) {
				continue
			}
			if archived.applyTo(call) {
				lengthOnly++
			}
		}
	}
	kept := splice.kept + lengthOnly
	if kept == 0 {
		return
	}
	lengthNote := ""
	if lengthOnly > 0 {
		lengthNote = fmt.Sprintf(" (%d as length only)", lengthOnly)
	}
	log.Printf(
		"preserve %s %s: kept %d archived tool results%s whose persisted output files are missing",
		splice.agent, splice.archivedID, kept, lengthNote,
	)
}

// applyTo writes the archived length onto call and its blank result events,
// and reports whether anything changed.
func (a archivedResultLength) applyTo(call *db.ToolCall) bool {
	changed := false
	if a.length > 0 && call.ResultContentLength != a.length {
		call.ResultContentLength = a.length
		changed = true
	}
	for k := range call.ResultEvents {
		event := &call.ResultEvents[k]
		if event.Content != "" {
			continue
		}
		length, digest := 0, []byte(nil)
		switch {
		case k < len(a.events) && a.events[k].ContentLength > 0:
			length, digest = a.events[k].ContentLength, a.events[k].RawContentDigest
		case len(call.ResultEvents) == 1:
			length = a.length
		}
		if length > 0 && event.ContentLength != length {
			event.ContentLength = length
			changed = true
		}
		if digest != nil {
			event.RawContentDigest = slices.Clone(digest)
		}
	}
	return changed
}

// hasPersistedToolResultPlaceholder reports whether any parsed tool result is
// a persisted-output placeholder.
func hasPersistedToolResultPlaceholder(msgs []parser.ParsedMessage) bool {
	for _, m := range msgs {
		if slices.ContainsFunc(m.ToolResults, persistedPlaceholderResult) {
			return true
		}
	}
	return false
}

// persistedPlaceholderResult reports whether a parsed tool result is a
// persisted-output placeholder. A placeholder's raw JSON holds one of its
// markers verbatim (neither contains a character JSON escapes), so a result
// without them is rejected before it is decoded.
func persistedPlaceholderResult(result parser.ParsedToolResult) bool {
	if !strings.Contains(result.ContentRaw, "persisted-output") &&
		!strings.Contains(result.ContentRaw, "Full output saved to:") {
		return false
	}
	return parser.IsPersistedToolResultPlaceholder(
		parser.DecodeContent(result.ContentRaw),
	)
}

// archivedClaudeToolResults reads the archived calls a placeholder may not
// replace. texts maps each tool_use_id to its call's result text: the summary
// when it is stored, else the first result event with content. Only calls
// holding a result that is not itself a placeholder (the side file was
// already gone when the call was archived) are kept. lengths maps each
// tool_use_id whose call stored no text but a length, as a blocked category
// does, to that length. The first qualifying call wins for a repeated ID.
func archivedClaudeToolResults(
	rows []db.Message,
) (map[string]string, map[string]archivedResultLength) {
	texts := make(map[string]string)
	lengths := make(map[string]archivedResultLength)
	for _, m := range rows {
		for _, call := range m.ToolCalls {
			if call.ToolUseID == "" {
				continue
			}
			text := archivedResultText(call)
			if text == "" {
				if _, seen := lengths[call.ToolUseID]; !seen &&
					storedResultLengthOnly(call) {
					lengths[call.ToolUseID] = archivedResultLength{
						length: call.ResultContentLength,
						events: call.ResultEvents,
					}
				}
				continue
			}
			if _, seen := texts[call.ToolUseID]; seen {
				continue
			}
			if parser.IsPersistedToolResultPlaceholder(text) {
				continue
			}
			texts[call.ToolUseID] = text
		}
	}
	return texts, lengths
}

// storedResultLengthOnly reports whether an archived call without result text
// still recorded the result's length, on the call or on a result event.
func storedResultLengthOnly(call db.ToolCall) bool {
	return call.ResultContentLength > 0 ||
		slices.ContainsFunc(call.ResultEvents, func(e db.ToolResultEvent) bool {
			return e.ContentLength > 0
		})
}

// archivedResultText returns an archived call's result text: its summary when
// non-empty, else the content of its first result event that has content.
func archivedResultText(call db.ToolCall) string {
	if call.ResultContent != "" {
		return call.ResultContent
	}
	for _, event := range call.ResultEvents {
		if event.Content != "" {
			return event.Content
		}
	}
	return ""
}
