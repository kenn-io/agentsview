package db

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// archiveContentRanks orders policies from least to most restrictive. The
// atomic on DB stores an index into this slice.
var archiveContentRanks = []config.ArchiveContent{
	config.ArchiveContentFull,
	config.ArchiveContentTranscripts,
	config.ArchiveContentUsage,
}

func archiveContentRank(policy config.ArchiveContent) int32 {
	index := slices.Index(archiveContentRanks, policy)
	if index < 0 {
		return 0
	}
	return int32(index)
}

// SetArchiveContent tightens this handle's storage boundary. The switch is
// monotonic for the lifetime of a DB handle: once a process promises not to
// persist some content, a later caller cannot silently weaken that promise.
func (db *DB) SetArchiveContent(policy config.ArchiveContent) {
	if db == nil {
		return
	}
	rank := archiveContentRank(policy)
	for {
		current := db.archiveContent.Load()
		if current >= rank || db.archiveContent.CompareAndSwap(current, rank) {
			return
		}
	}
}

// ArchiveContent reports the storage boundary this DB handle enforces.
func (db *DB) ArchiveContent() config.ArchiveContent {
	if db == nil {
		return config.ArchiveContentFull
	}
	return archiveContentRanks[db.archiveContent.Load()]
}

func (db *DB) usageOnlyStorage() bool {
	return db.ArchiveContent().UsageOnly()
}

// ErrArchiveContentExcluded reports a write that the archive's content
// policy does not store. Callers can match it with errors.Is.
var ErrArchiveContentExcluded = errors.New(
	"archive content policy excludes this data",
)

// requireDerivedTextStorage rejects insights and recall entries on a usage
// archive. Both hold transcript-derived text, a rebuild under that policy
// leaves them behind, and accepting them only to lose them later would be
// worse than refusing up front.
func (db *DB) requireDerivedTextStorage(kind string) error {
	if !db.usageOnlyStorage() {
		return nil
	}
	return fmt.Errorf(
		"%w: %s are not stored under archive_content = %q",
		ErrArchiveContentExcluded, kind, config.ArchiveContentUsage,
	)
}

func (db *DB) sessionForStorage(session Session) Session {
	if !db.usageOnlyStorage() {
		return session
	}
	// Derive automation while the parser/importer preview is still present.
	// The retained session row is authoritative after transcript text is gone.
	session.IsAutomated = sessionIsAutomated(session)
	session.PreserveStoredAutomation = session.FirstMessage == nil &&
		session.UserMessageCount <= 1
	session.FirstMessage = nil
	session.DisplayName = nil
	session.SessionName = nil
	session.PreserveSessionName = false
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	return session
}

func (db *DB) sessionAndMessagesForStorage(
	session Session, messages []Message,
) (Session, []Message) {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		// Some importers do not precompute IsAutomated. Classify from the raw
		// messages before the storage projection drops user text.
		session.IsAutomated = sessionIsAutomated(session) ||
			IsAutomatedTranscript(
				session.UserMessageCount, messages, session.FirstMessage,
			)
		return db.sessionForStorage(session), usageOnlyMessages(messages)
	case config.ArchiveContentTranscripts:
		return session, transcriptMessages(messages)
	default:
		return session, messages
	}
}

// ProjectSessionForStorage applies this database handle's storage policy to a
// prepared session without writing it. Report-only callers use the same
// projection as the write boundary before comparing prepared and stored rows.
func (db *DB) ProjectSessionForStorage(
	session Session, messages []Message,
) (Session, []Message) {
	return db.sessionAndMessagesForStorage(session, messages)
}

func (db *DB) messagesForStorage(messages []Message) []Message {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		return usageOnlyMessages(messages)
	case config.ArchiveContentTranscripts:
		return transcriptMessages(messages)
	default:
		return messages
	}
}

func (db *DB) subagentLinksForStorage(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		return usageOnlySubagentLinks(links)
	case config.ArchiveContentTranscripts:
		return transcriptSubagentLinks(links)
	default:
		return links
	}
}

// transcriptMessages keeps every message row and tool call while dropping
// the payloads that carry file contents and command output. Tool names,
// categories, skill names, the target file path, result lengths, statuses,
// and delegation links are retained metadata: tool analytics and the session
// tree depend on them, and the configuration docs list them as kept.
// Providers such as Devin and Omnigent emit tool output as standalone
// tool-role messages, so those rows lose their text as well.
func transcriptMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	stored := slices.Clone(messages)
	for i := range stored {
		if isToolOutputRow(stored[i]) {
			stored[i].Content = ""
			stored[i].ContentLength = 0
		}
		stored[i].ToolResults = nil
		if len(stored[i].ToolCalls) == 0 {
			continue
		}
		if redacted, changed := redactToolUseRenderings(
			stored[i].Content, stored[i].ToolCalls,
		); changed {
			stored[i].Content = redacted
			stored[i].ContentLength = len(redacted)
		}
		calls := slices.Clone(stored[i].ToolCalls)
		for j := range calls {
			calls[j].InputJSON = ""
			calls[j].ResultContent = ""
			if len(calls[j].ResultEvents) == 0 {
				continue
			}
			events := slices.Clone(calls[j].ResultEvents)
			for k := range events {
				events[k].Content = ""
			}
			calls[j].ResultEvents = events
		}
		stored[i].ToolCalls = calls
	}
	return stored
}

// redactToolUseRenderings rewrites the tool call summaries that parsers
// inline into assistant text so they keep the tool label and target path but
// lose every other argument. Only exact renderings are replaced; the rest of
// the message stays as written.
func redactToolUseRenderings(content string, calls []ToolCall) (string, bool) {
	var pairs []parser.ToolUseRenderingPair
	for _, call := range calls {
		full, redacted := toolUseRenderingToReplace(content, call)
		if full == "" || full == redacted {
			continue
		}
		pairs = append(pairs, parser.ToolUseRenderingPair{Full: full, Redacted: redacted})
	}
	if len(pairs) == 0 {
		return content, false
	}
	// Resolve every call against the original content, then give longer
	// renderings priority at the same position. A shorter rendering must
	// not consume a longer one's prefix and leave its arguments behind.
	slices.SortStableFunc(pairs, func(a, b parser.ToolUseRenderingPair) int {
		return cmp.Compare(len(b.Full), len(a.Full))
	})
	replacements := make([]string, 0, 2*len(pairs))
	for _, pair := range pairs {
		replacements = append(replacements, pair.Full, pair.Redacted)
	}
	// Replacer scans once without reprocessing its output. Repeated or
	// quoted renderings are still replaced at every occurrence.
	redacted := strings.NewReplacer(replacements...).Replace(content)
	return redacted, redacted != content
}

// toolUseRenderingToReplace finds the text a parser inlined for call inside
// content. A freshly parsed call carries the exact text; a copied row only
// has the call's name and input, so every renderer's output is tried.
func toolUseRenderingToReplace(content string, call ToolCall) (string, string) {
	if call.Rendering != "" && strings.Contains(content, call.Rendering) {
		return call.Rendering, parser.RedactToolUseRendering(
			call.Rendering, call.Category, call.ToolName, call.InputJSON,
		)
	}
	// A recorded rendering that no longer appears verbatim (the content was
	// rewritten after parsing) falls back to regenerating it from the
	// stored input, the same way copied rows are handled.
	if call.InputJSON == "" {
		return "", ""
	}
	// Renderers share prefixes (a bare "[Bash]" header opens several of
	// them), so the longest candidate present in the content is the one
	// that was inlined; a shorter match would leave the body behind.
	var full, redacted string
	for _, pair := range parser.ToolUseRenderingCandidates(
		call.Category, call.ToolName, call.InputJSON,
	) {
		if len(pair.Full) > len(full) && strings.Contains(content, pair.Full) {
			full, redacted = pair.Full, pair.Redacted
		}
	}
	return full, redacted
}

// isToolOutputRow reports whether a message row's text is tool output: a
// standalone tool-role record, a user/system fallback row a provider marked
// because it has no separate tool role, or a row whose text is byte-identical
// to one of the tool results it carries. The last check is a write-time
// safety net for providers that were never marked; copied rows no longer
// carry ToolResults, so the resync SQL relies on the role and the marker.
func isToolOutputRow(message Message) bool {
	if message.Role == "tool" ||
		message.SourceSubtype == parser.SourceSubtypeToolResult {
		return true
	}
	if message.Role == "assistant" || message.Content == "" {
		return false
	}
	return slices.ContainsFunc(message.ToolResults, func(r ToolResult) bool {
		return r.ContentRaw == message.Content ||
			parser.DecodeContent(r.ContentRaw) == message.Content
	})
}

func transcriptSubagentLinks(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	if links == nil {
		return nil
	}
	stored := slices.Clone(links)
	for i := range stored {
		stored[i].ResultContent = ""
	}
	return stored
}

func usageOnlyMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	stored := make([]Message, 0, len(messages))
	for _, message := range messages {
		if !usageOnlyMessageRequired(message) {
			continue
		}
		message.Content = ""
		message.ThinkingText = ""
		message.ToolCalls = usageOnlyToolCalls(message.ToolCalls)
		message.ToolResults = nil
		message.HasThinking = false
		message.HasToolUse = len(message.ToolCalls) > 0
		message.ContentLength = 0
		message.IsSystem = false
		message.ContextTokens = 0
		message.OutputTokens = 0
		message.HasContextTokens = false
		message.HasOutputTokens = false
		message.SourceType = ""
		message.SourceSubtype = ""
		message.PromptSource = ""
		message.SourceParentUUID = ""
		message.IsSidechain = false
		message.IsCompactBoundary = false
		stored = append(stored, message)
	}
	return stored
}

func usageOnlyMessageRequired(message Message) bool {
	tokenEligible := len(message.TokenUsage) > 0 && message.Model != "" &&
		message.Model != "<synthetic>"
	activityEligible := message.Role == "assistant" &&
		message.Model != "<synthetic>"
	return tokenEligible || activityEligible ||
		usageOnlyMessageHasSubagentCall(message)
}

func usageOnlyMessageHasSubagentCall(message Message) bool {
	return slices.ContainsFunc(
		message.ToolCalls, usageOnlySubagentCallRequired,
	)
}

func usageOnlySubagentCallRequired(call ToolCall) bool {
	return call.SubagentSessionID != "" || call.Category == "Task" ||
		strings.Contains(call.ToolName, "subagent")
}

// usageOnlyToolCalls retains the opaque identifiers needed to reconstruct
// delegated-session relationships. Everything that can carry transcript or
// tool-result content is replaced or discarded at the storage boundary.
func usageOnlyToolCalls(calls []ToolCall) []ToolCall {
	var stored []ToolCall
	for _, call := range calls {
		if !usageOnlySubagentCallRequired(call) {
			continue
		}
		stored = append(stored, ToolCall{
			ToolName:          "subagent",
			Category:          "Task",
			ToolUseID:         call.ToolUseID,
			SubagentSessionID: call.SubagentSessionID,
		})
	}
	return stored
}

func usageOnlySubagentLinks(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	var stored []ToolCallSubagentLink
	for _, link := range links {
		if link.ToolUseID == "" || link.SubagentSessionID == "" {
			continue
		}
		stored = append(stored, ToolCallSubagentLink{
			ToolUseID:         link.ToolUseID,
			SubagentSessionID: link.SubagentSessionID,
		})
	}
	return stored
}

// clearUsageOnlyTextTx removes the free text a usage archive never stores
// from rows the write path only updates in part: session titles the upsert
// leaves untouched and pin notes a message replacement carries across.
func clearUsageOnlyTextTx(
	tx interface {
		Exec(string, ...any) (sql.Result, error)
	},
	sessionID string,
) error {
	if _, err := tx.Exec(
		`UPDATE sessions
		    SET first_message = NULL, display_name = NULL, session_name = NULL
		  WHERE id = ? AND (first_message IS NOT NULL
		     OR display_name IS NOT NULL OR session_name IS NOT NULL)`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing usage-only titles for %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(
		`UPDATE pinned_messages SET note = NULL
		  WHERE session_id = ? AND note IS NOT NULL`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing usage-only pin notes for %s: %w", sessionID, err)
	}
	return nil
}

// settleUsageOnlySessionTx brings an existing session row that predates the
// usage policy in line with it: titles and pin notes go, and transcript-
// derived signals and secret findings are cleared and marked settled.
func settleUsageOnlySessionTx(tx transactionQueries, sessionID string) error {
	if err := clearUsageOnlyTextTx(tx, sessionID); err != nil {
		return err
	}
	return settleUsageOnlySignalsTx(tx, sessionID)
}

func updateUsageOnlyAutomationTx(
	tx transactionQueries, sessionID string, messages []Message,
) error {
	if err := clearUsageOnlyTextTx(tx, sessionID); err != nil {
		return err
	}
	var userMessageCount int
	var automated bool
	var agent, sessionKind string
	err := tx.QueryRow(
		`SELECT user_message_count, is_automated, agent, session_kind
		   FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&userMessageCount, &automated, &agent, &sessionKind)
	if err != nil {
		return err
	}
	if IsAutomatedSessionMetadata(agent, sessionKind) {
		if automated {
			return nil
		}
		return setSessionAutomationTx(tx, sessionID, true)
	}
	if userMessageCount > 1 {
		if !automated {
			return nil
		}
		return setSessionAutomationTx(tx, sessionID, false)
	}
	// An incremental tail may omit the first user message whose text was
	// discarded after the original classification. Preserve an existing
	// one-turn verdict; new raw text can still promote an unclassified row.
	if automated || !IsAutomatedTranscript(userMessageCount, messages, nil) {
		return nil
	}
	return setSessionAutomationTx(tx, sessionID, true)
}

func messagesForSession(messages []Message, sessionID string) []Message {
	selected := make([]Message, 0, len(messages))
	for _, message := range messages {
		if message.SessionID == sessionID {
			selected = append(selected, message)
		}
	}
	return selected
}

// applyArchiveContentToCopiedSessionsTx projects sessions copied verbatim
// from another archive onto this database's storage policy. Resync copies
// archived rows with ATTACH, so the write-time projection never sees them.
// The SQL mirrors usageOnlyMessages and transcriptMessages column for column.
func applyArchiveContentToCopiedSessionsTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
	policy config.ArchiveContent,
) error {
	switch policy {
	case config.ArchiveContentTranscripts:
		return dropCopiedToolContentTx(ctx, tx, tempIDsTable)
	case config.ArchiveContentUsage:
		return compactCopiedSessionsForUsageTx(ctx, tx, tempIDsTable)
	default:
		return nil
	}
}

// toolOutputMarkerDataVersion is the first data version whose parsers mark
// tool output stored as ordinary message text (see the data version notes
// in db.go). Sessions parsed earlier and never re-parsed, because their
// source files are gone, carry no marker.
const toolOutputMarkerDataVersion = 100

func dropCopiedToolContentTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	inCopied := ` IN (SELECT id FROM ` + tempIDsTable + `)`
	// Rows of sessions parsed before the marker existed cannot be told apart
	// by marker, so the copy fails closed per provider: every row shape the
	// old parser used for tool output loses its text, even where that shape
	// also carried a notice or a prompt. Only sessions whose source files
	// are gone reach this path; anything with a source is re-parsed instead.
	inUnmarked := func(agents string) string {
		return ` IN (SELECT s.id FROM sessions s
			WHERE s.id` + inCopied + `
			  AND s.data_version < ` +
			fmt.Sprint(toolOutputMarkerDataVersion) + `
			  AND s.agent IN (` + agents + `))`
	}
	statements := []struct {
		label string
		sql   string
	}{
		{"tool payloads", `
			UPDATE tool_calls SET input_json = NULL, result_content = NULL
			WHERE session_id` + inCopied},
		{"tool result events", `
			UPDATE tool_result_events SET content = ''
			WHERE session_id` + inCopied},
		{"tool-role message text", `
			UPDATE messages SET content = '', content_length = 0
			WHERE (role = 'tool' OR source_subtype = '` +
			parser.SourceSubtypeToolResult + `')
			  AND session_id` + inCopied},
		// RooCode and Kilo Legacy stored unpaired tool output as system-flagged
		// user or system rows.
		{"unmarked RooCode and Kilo Legacy tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE is_system = 1 AND session_id` +
			inUnmarked(`'`+string(parser.AgentRooCode)+`', '`+
				string(parser.AgentKiloLegacy)+`'`)},
		// Zencoder stored system blocks embedded in tool results as
		// system-flagged user rows, just like ordinary system notices.
		{"unmarked Zencoder tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE is_system = 1 AND session_id` +
			inUnmarked(`'`+string(parser.AgentZencoder)+`'`)},
		// Codex and TraeX stored unpaired agent notifications as ordinary
		// user rows, with no field that distinguishes them from prompts.
		{"unmarked Codex and TraeX tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE role = 'user' AND session_id` +
			inUnmarked(`'`+string(parser.AgentCodex)+`', '`+
				string(parser.AgentTraeX)+`'`)},
		// gptme stored tool output as assistant rows without a model, while
		// model replies carry the model name.
		{"unmarked gptme tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE role = 'assistant' AND model = '' AND session_id` +
			inUnmarked(`'`+string(parser.AgentGptme)+`'`)},
		// OpenHands stored unpaired observations as user rows that are
		// indistinguishable from prompts, so every user row loses its text.
		{"unmarked OpenHands tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE role = 'user' AND session_id` +
			inUnmarked(`'`+string(parser.AgentOpenHands)+`'`)},
		// Aider surfaced its tool channel as assistant rows that are
		// indistinguishable from replies, so every assistant row loses its
		// text.
		{"unmarked Aider tool output", `
			UPDATE messages SET content = '', content_length = 0
			WHERE role = 'assistant' AND session_id` +
			inUnmarked(`'`+string(parser.AgentAider)+`'`)},
	}
	// Tool call summaries inside assistant text need the copied inputs to
	// regenerate, so they are rewritten before the inputs are dropped.
	if err := redactCopiedToolUseRenderingsTx(ctx, tx, tempIDsTable); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("dropping copied %s: %w", statement.label, err)
		}
	}
	// The source archive computed signals and secret findings from payloads
	// this archive does not keep. Clearing the counters and their version
	// markers hides the stale values now and lets the startup backfill
	// recompute both from the projected rows, matching what the write path
	// stores for freshly parsed sessions.
	rows, err := tx.QueryContext(ctx, "SELECT id FROM "+tempIDsTable)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	for _, id := range ids {
		if err := updateSessionSignalsTx(
			tx, id, SessionSignalUpdate{},
		); err != nil {
			return fmt.Errorf("clearing copied signals for %s: %w", id, err)
		}
		if err := replaceSecretFindingsTx(tx, id, nil, 0, ""); err != nil {
			return fmt.Errorf("clearing copied findings for %s: %w", id, err)
		}
	}
	return nil
}

func compactCopiedSessionsForUsageTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	inCopied := ` IN (SELECT id FROM ` + tempIDsTable + `)`
	statements := []struct {
		label string
		sql   string
	}{
		{"tool result events", `
			DELETE FROM tool_result_events WHERE session_id` + inCopied},
		{"non-delegation tool calls", `
			DELETE FROM tool_calls
			WHERE session_id` + inCopied + `
			  AND NOT (COALESCE(subagent_session_id, '') != ''
			       OR category = 'Task'
			       OR tool_name LIKE '%subagent%')`},
		{"delegation tool calls", `
			UPDATE tool_calls
			SET tool_name = 'subagent', category = 'Task',
			    input_json = NULL, skill_name = NULL,
			    result_content_length = NULL, result_content = NULL,
			    file_path = NULL
			WHERE session_id` + inCopied},
		{"messages outside usage accounting", `
			DELETE FROM messages
			WHERE session_id` + inCopied + `
			  AND NOT (
			    (length(token_usage) > 0 AND model != ''
			       AND model != '<synthetic>')
			    OR (role = 'assistant' AND model != '<synthetic>')
			    OR EXISTS (SELECT 1 FROM tool_calls tc
			               WHERE tc.message_id = messages.id))`},
		{"message payloads", `
			UPDATE messages
			SET content = '', thinking_text = '', has_thinking = 0,
			    has_tool_use = EXISTS (SELECT 1 FROM tool_calls tc
			                           WHERE tc.message_id = messages.id),
			    content_length = 0, is_system = 0,
			    context_tokens = 0, output_tokens = 0,
			    has_context_tokens = 0, has_output_tokens = 0,
			    source_type = '', source_subtype = '', prompt_source = '',
			    source_parent_uuid = '', is_sidechain = 0,
			    is_compact_boundary = 0
			WHERE session_id` + inCopied},
		{"session titles", `
			UPDATE sessions
			SET first_message = NULL, display_name = NULL,
			    session_name = NULL, secret_leak_count = 0,
			    secrets_rules_version = ''
			WHERE id` + inCopied},
		{"pin notes", `
			UPDATE pinned_messages SET note = NULL
			WHERE session_id` + inCopied},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf(
				"compacting copied %s: %w", statement.label, err,
			)
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM "+tempIDsTable)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	for _, id := range ids {
		if err := settleUsageOnlySignalsTx(tx, id); err != nil {
			return fmt.Errorf("settling copied signals for %s: %w", id, err)
		}
	}
	return nil
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func redactCopiedToolUseRenderingsTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.content, s.agent, tc.tool_name, tc.category,
		       COALESCE(tc.input_json, '')
		  FROM messages m
		  JOIN tool_calls tc ON tc.message_id = m.id
		  JOIN sessions s ON s.id = m.session_id
		 WHERE m.session_id IN (SELECT id FROM `+tempIDsTable+`)
		 ORDER BY m.id, tc.call_index`)
	if err != nil {
		return fmt.Errorf("listing copied tool renderings: %w", err)
	}
	type pending struct {
		id      int64
		content string
		agent   string
		calls   []ToolCall
	}
	// Rows arrive ordered by message, so each message's calls are
	// contiguous and the last entry is the one being extended.
	var entries []pending
	for rows.Next() {
		var id int64
		var content, agent string
		var call ToolCall
		if err := rows.Scan(
			&id, &content, &agent, &call.ToolName, &call.Category, &call.InputJSON,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scanning copied tool rendering: %w", err)
		}
		if len(entries) == 0 || entries[len(entries)-1].id != id {
			entries = append(entries, pending{id: id, content: content, agent: agent})
		}
		last := &entries[len(entries)-1]
		last.calls = append(last.calls, call)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("listing copied tool renderings: %w", err)
	}
	rows.Close()
	for _, entry := range entries {
		content := entry.content
		if entry.agent == string(parser.AgentOpenHands) {
			content = redactCopiedOpenHandsSummaries(content, entry.calls)
		}
		redacted, _ := redactToolUseRenderings(content, entry.calls)
		if redacted == entry.content {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE messages SET content = ?, content_length = ? WHERE id = ?",
			redacted, len(redacted), entry.id,
		); err != nil {
			return fmt.Errorf(
				"redacting copied tool rendering %d: %w", entry.id, err,
			)
		}
	}
	return nil
}

// OpenHands summaries are not persisted separately and may contain brackets
// and newlines. Their end cannot be recovered reliably, so discard the tail
// starting at a summary-bearing tool header, including any appended thinking.
// The thought before the action stays. Calls whose renderer ignores the event
// summary continue through the normal exact-rendering replacement.
func redactCopiedOpenHandsSummaries(content string, calls []ToolCall) string {
	for _, call := range calls {
		label := call.ToolName
		switch label {
		case "file_editor", "delegate", "task_tracker":
			continue
		case "terminal":
			label = "Bash"
		}
		if before, _, found := strings.Cut(content, "["+label+": "); found {
			content = before + "[" + label + "]"
		}
	}
	return content
}
