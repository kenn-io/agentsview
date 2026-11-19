package sync

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (e *Engine) applyIDPrefixToSessionIDs(ids []string) []string {
	return applyIDPrefixToIDs(e.idPrefix, ids)
}

// applyRemoteRewrites rewrites stored source paths and, when configured,
// prefixes session IDs for remote sync.
func (e *Engine) applyRemoteRewrites(
	s *db.Session, msgs []db.Message,
) {
	_ = e.applyRemoteRewritesContext(context.Background(), s, msgs)
}

func (e *Engine) applyRemoteRewritesContext(
	ctx context.Context, s *db.Session, msgs []db.Message,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.pathRewriter != nil && s.FilePath != nil {
		fp := e.pathRewriter(*s.FilePath)
		s.FilePath = &fp
	}
	if e.idPrefix == "" {
		return nil
	}
	s.ID = applyIDPrefixToID(e.idPrefix, s.ID)
	if s.ParentSessionID != nil && *s.ParentSessionID != "" {
		p := applyIDPrefixToID(e.idPrefix, *s.ParentSessionID)
		s.ParentSessionID = &p
	}
	if s.ParserParentSessionID != nil && *s.ParserParentSessionID != "" {
		p := applyIDPrefixToID(e.idPrefix, *s.ParserParentSessionID)
		s.ParserParentSessionID = &p
	}
	for i := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		msgs[i].SessionID = s.ID
		for j := range msgs[i].ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			msgs[i].ToolCalls[j].SessionID = s.ID
			if msgs[i].ToolCalls[j].SubagentSessionID != "" {
				msgs[i].ToolCalls[j].SubagentSessionID = applyIDPrefixToID(
					e.idPrefix,
					msgs[i].ToolCalls[j].SubagentSessionID,
				)
			}
			for k := range msgs[i].ToolCalls[j].ResultEvents {
				re := &msgs[i].ToolCalls[j].ResultEvents[k]
				if re.SubagentSessionID != "" {
					re.SubagentSessionID = applyIDPrefixToID(
						e.idPrefix,
						re.SubagentSessionID,
					)
				}
			}
		}
	}
	return ctx.Err()
}

// toDBSession converts a pendingWrite to a db.Session.
func toDBSession(pw pendingWrite) db.Session {
	s, _ := toDBSessionContext(context.Background(), pw)
	return s
}

func toDBSessionContext(
	ctx context.Context, pw pendingWrite,
) (db.Session, error) {
	return ingest.ConvertSessionContext(ctx, pw.sess, pw.msgs)
}

// toDBMessages converts parsed messages to db.Message rows
// with tool-result pairing and filtering applied.
func toDBMessages(pw pendingWrite, blocked map[string]bool) []db.Message {
	msgs, _ := toDBMessagesContext(context.Background(), pw, blocked)
	return msgs
}

func toDBMessagesContext(
	ctx context.Context, pw pendingWrite, blocked map[string]bool,
) ([]db.Message, error) {
	return ingest.ConvertMessagesContext(
		ctx, pw.sess.ID, pw.sess.Agent, pw.msgs, blocked,
	)
}

// toDBUsageEvents converts parser usage events for one session.
// sessionID is the final ID after remote rewrites; parser-stamped
// event session IDs predate the idPrefix and are ignored. It returns the
// fix counts from the central validation/sanitization pass so write paths
// can surface them in the sync summary; diagnostic callers may discard.
func toDBUsageEvents(
	sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, validationStats) {
	out, stats, _ := toDBUsageEventsContext(
		context.Background(), sessionID, events,
	)
	return out, stats
}

func toDBUsageEventsContext(
	ctx context.Context, sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, validationStats, error) {
	return ingest.ConvertUsageEventsContext(ctx, sessionID, events)
}

// postFilterCounts returns the total and user message counts
// from a filtered message slice. System-injected messages
// (e.g. Zencoder compaction, continuation notices) are excluded
// from the user count.
func postFilterCounts(msgs []db.Message) (total, user int) {
	total, user, _ = postFilterCountsContext(context.Background(), msgs)
	return
}

func postFilterCountsContext(
	ctx context.Context, msgs []db.Message,
) (total, user int, err error) {
	for _, m := range msgs {
		if err = ctx.Err(); err != nil {
			return
		}
		if m.Role == "user" && !m.IsSystem && m.SourceSubtype != parser.SourceSubtypeToolResult {
			user++
		}
	}
	return len(msgs), user, ctx.Err()
}

// chunkHasRealUserPrompt reports whether msgs contains a message the
// Claude parser would use as first_message (mirroring the firstMsg
// rule in firstMessageAndUserCount): role user, not system-injected,
// non-empty content, and not a bare slash command. Promoted system
// records (IDE context, continuation, stop-hook), tool-result-only
// rows, and preview-skipped commands like /clear do not qualify —
// a full parse triggered by those would leave first_message empty
// and the next append would full-parse again.
func chunkHasRealUserPrompt(msgs []parser.ParsedMessage) bool {
	for _, m := range msgs {
		if m.Role == parser.RoleUser && !m.IsSystem &&
			m.SourceSubtype != parser.SourceSubtypeToolResult && m.Content != "" &&
			!parser.IsSkippablePreviewCommand(m.Content) {
			return true
		}
	}
	return false
}

// countUserMsgs counts user messages in parsed messages.
func countUserMsgs(msgs []parser.ParsedMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == parser.RoleUser && m.SourceSubtype != parser.SourceSubtypeToolResult {
			n++
		}
	}
	return n
}

func nextParsedOrdinal(
	current int, msgs []parser.ParsedMessage,
) int {
	return ingest.NextParsedOrdinal(current, msgs)
}

func lastParsedSourceUUID(
	current string, msgs []parser.ParsedMessage,
) string {
	return ingest.LastParsedSourceUUID(current, msgs)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// convertToolCalls maps parsed tool calls to db.ToolCall
// structs. MessageID is resolved later during insert.
func convertToolCalls(
	sessionID string, parsed []parser.ParsedToolCall,
) []db.ToolCall {
	calls, _ := convertToolCallsContext(
		context.Background(), sessionID, parsed,
	)
	return calls
}

func convertToolCallsContext(
	ctx context.Context, sessionID string, parsed []parser.ParsedToolCall,
) ([]db.ToolCall, error) {
	return ingest.ConvertToolCallsContext(ctx, sessionID, parsed)
}

// prepareCodexResultIdentities runs in parse workers before their payloads
// enter the serialized archive writer. Direct write callers capture missing
// metadata at conversion; normal imports only copy the small digest.
func prepareCodexResultIdentities(
	msgs []parser.ParsedMessage, updates []parser.ParsedToolCallUpdate,
) {
	prepare := func(events []parser.ParsedToolResultEvent) {
		for i := range events {
			event := db.ToolResultEvent{
				Content:          events[i].Content,
				RawContentDigest: events[i].RawContentDigest,
			}
			db.PrepareToolResultEvent(&event)
			events[i].RawContentDigest = event.RawContentDigest
		}
	}
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			prepare(msgs[i].ToolCalls[j].ResultEvents)
		}
	}
	for i := range updates {
		prepare(updates[i].ResultEvents)
	}
}

func convertToolResultEventsContext(
	ctx context.Context, parsed []parser.ParsedToolResultEvent,
) ([]db.ToolResultEvent, error) {
	return ingest.ConvertToolResultEventsContext(ctx, parsed)
}

// pairAndFilter pairs tool results with their corresponding
// tool calls, then removes user messages that carried only
// tool_result blocks (no displayable text).
func pairAndFilter(msgs []db.Message, blocked map[string]bool) []db.Message {
	filtered, _ := pairAndFilterContext(
		context.Background(), msgs, blocked,
	)
	return filtered
}

func pairAndFilterContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) ([]db.Message, error) {
	return ingest.PairAndFilterContext(ctx, msgs, blocked)
}

// pairToolResults matches tool_result content to their
// corresponding tool_calls across message boundaries using
// tool_use_id. Categories in blocked are stored without content.
func pairToolResults(msgs []db.Message, blocked map[string]bool) {
	_ = pairToolResultsContext(context.Background(), msgs, blocked)
}

func pairToolResultsContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) error {
	return ingest.PairToolResultsContext(ctx, msgs, blocked)
}

func pairToolResultEventSummaries(
	msgs []db.Message, blocked map[string]bool,
) {
	_ = pairToolResultEventSummariesContext(
		context.Background(), msgs, blocked,
	)
}

func pairToolResultEventSummariesContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) error {
	return ingest.PairToolResultEventSummariesContext(ctx, msgs, blocked)
}
