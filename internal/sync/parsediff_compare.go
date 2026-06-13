package sync

// Parse-diff comparator: compares one freshly parsed, sync-normalized
// session against its stored SQLite rows and emits FieldDiff entries.
// It covers the parser-owned session columns, every persisted
// per-message column, and the parser-owned tool_call columns. All
// comparisons mirror the normalization the write path applies (nil <->
// "" equivalence, SanitizeUTF8, TokenPresence inference) so that an
// unchanged parser produces zero diffs against rows it wrote itself.
//
// Three parser-owned-but-derived areas are deliberately not compared:
//   - the session "project" column, rewritten from the mutable
//     worktree_project_mappings table (its parser-owned input cwd is
//     compared instead);
//   - the tool_call result body, possibly redacted to "" by the
//     blocked-category config and unbounded in size; and
//   - the tool_result_events rows, which the same blocked-category
//     config clears wholesale (the events slice is set to nil) and whose
//     content is likewise unbounded.
//
// For the latter two, only the config-stable result_content_length --
// set from the event summary before the redaction check, so it captures
// the dominant content-size signal -- is compared. Session columns that
// the incremental-append path leaves frozen are compared but marked
// informational for the incremental agents, mirroring termination_status.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// maxRenderedValueRunes caps rendered string values in FieldDiff;
// longer values are truncated with the full lengths noted in Detail.
const maxRenderedValueRunes = 80

// compareStoredSession compares the prepared (normalized) parse output
// against the stored session row, lazily reading message and usage
// event rows from the archive only when needed.
func (e *Engine) compareStoredSession(
	ctx context.Context,
	stored *db.Session,
	prepared db.Session,
	msgs []db.Message,
	events []db.UsageEvent,
) ([]FieldDiff, error) {
	diffs := compareSessionFields(stored, prepared)

	// Tier 1: three exact ordered fingerprints over the stored
	// messages. The token fingerprint covers per-message model and
	// token metadata; the role/time fingerprint covers per-message
	// role and timestamp, which the token fingerprint deliberately
	// excludes (its shape is shared with the PG push fast-path); the
	// content fingerprint covers per-message content_length and a
	// body hash. Equal fingerprints prove the messages match on the
	// compared fields without materializing full rows; only a
	// mismatch loads them for attribution.
	storedTokenFP, err := e.db.MessageTokenFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: message fingerprint for %s: %w",
			stored.ID, err,
		)
	}
	storedRoleTimeFP, err := e.db.MessageRoleTimeFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: role/time fingerprint for %s: %w",
			stored.ID, err,
		)
	}
	storedContentFP, err := e.db.MessageContentHashFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: content fingerprint for %s: %w",
			stored.ID, err,
		)
	}
	// The flags fingerprint covers the per-message thinking/system/
	// tool-use columns; the tool-call fingerprint covers the parser-owned
	// tool_calls rows. Neither is reachable through the token, role/time,
	// or content fingerprints, so without them a change confined to those
	// columns would never load the rows and would report identical.
	storedFlagsFP, err := e.db.MessageFlagsFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: flags fingerprint for %s: %w",
			stored.ID, err,
		)
	}
	storedToolFP, err := e.db.ToolCallParseDiffFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: tool-call fingerprint for %s: %w",
			stored.ID, err,
		)
	}

	tokenFPDiffers := messageTokenFingerprintTwin(msgs) != storedTokenFP
	roleTimeFPDiffers :=
		messageRoleTimeFingerprintTwin(msgs) != storedRoleTimeFP
	contentFPDiffers :=
		messageContentHashFingerprintTwin(msgs) != storedContentFP
	flagsFPDiffers :=
		messageFlagsFingerprintTwin(msgs) != storedFlagsFP
	toolFPDiffers :=
		toolCallParseDiffFingerprintTwin(msgs) != storedToolFP
	if tokenFPDiffers || roleTimeFPDiffers || contentFPDiffers ||
		flagsFPDiffers || toolFPDiffers {
		// Tier 2: load stored rows (with tool calls attached) and
		// attribute the mismatch to the per-message contract fields. A
		// mismatch that lands on none of them still yields a fallback
		// diff so a fingerprint inequality is never reported identical.
		storedMsgs, err := e.db.GetAllMessages(ctx, stored.ID)
		if err != nil {
			return nil, fmt.Errorf(
				"parse-diff: messages for %s: %w", stored.ID, err,
			)
		}
		diffs = append(diffs, compareMessageMetadata(
			storedMsgs, msgs,
			tokenFPDiffers || roleTimeFPDiffers || flagsFPDiffers,
			contentFPDiffers, toolFPDiffers,
		)...)
	}

	storedEvents, err := e.db.GetUsageEvents(ctx, stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: usage events for %s: %w", stored.ID, err,
		)
	}
	diffs = append(diffs, compareUsageEvents(storedEvents, events)...)
	return diffs, nil
}

// compareSessionFields compares the session-row contract fields.
// Pure function over the two rows; no database access.
func compareSessionFields(
	stored *db.Session, prepared db.Session,
) []FieldDiff {
	var diffs []FieldDiff

	if stored.MessageCount != prepared.MessageCount {
		diffs = append(diffs, FieldDiff{
			Field:  FieldMessageCount,
			Stored: strconv.Itoa(stored.MessageCount),
			Parsed: strconv.Itoa(prepared.MessageCount),
		})
	}
	if stored.UserMessageCount != prepared.UserMessageCount {
		diffs = append(diffs, FieldDiff{
			Field:  FieldUserMessageCount,
			Stored: strconv.Itoa(stored.UserMessageCount),
			Parsed: strconv.Itoa(prepared.UserMessageCount),
		})
	}

	diffs = appendTextFieldDiff(
		diffs, FieldFirstMessage,
		stored.FirstMessage, prepared.FirstMessage,
	)
	// session_name is frozen by the incremental-append path
	// (UpdateSessionIncremental never rewrites it) yet mutable
	// mid-session via Claude's /rename, so for the incremental-append
	// agents a difference is benign pipeline history rather than parser
	// drift -- mark it informational, like termination_status. By
	// contrast first_message and started_at, also appended via
	// appendTextFieldDiff, derive from the session head and are
	// byte-stable across appends, so they stay strict.
	before := len(diffs)
	diffs = appendTextFieldDiff(
		diffs, FieldSessionName,
		stored.SessionName, prepared.SessionName,
	)
	if len(diffs) > before {
		markIncrementalHistory(&diffs[len(diffs)-1], prepared.Agent)
	}
	diffs = appendTextFieldDiff(
		diffs, FieldStartedAt,
		stored.StartedAt, prepared.StartedAt,
	)
	diffs = appendTextFieldDiff(
		diffs, FieldEndedAt,
		stored.EndedAt, prepared.EndedAt,
	)

	if tokenAggregateDiffers(
		stored.HasTotalOutputTokens, stored.TotalOutputTokens,
		prepared.HasTotalOutputTokens, prepared.TotalOutputTokens,
	) {
		diffs = append(diffs, FieldDiff{
			Field: FieldTotalOutputTokens,
			Stored: renderTokenAggregate(
				stored.HasTotalOutputTokens, stored.TotalOutputTokens,
			),
			Parsed: renderTokenAggregate(
				prepared.HasTotalOutputTokens,
				prepared.TotalOutputTokens,
			),
		})
	}
	if tokenAggregateDiffers(
		stored.HasPeakContextTokens, stored.PeakContextTokens,
		prepared.HasPeakContextTokens, prepared.PeakContextTokens,
	) {
		diffs = append(diffs, FieldDiff{
			Field: FieldPeakContextTokens,
			Stored: renderTokenAggregate(
				stored.HasPeakContextTokens, stored.PeakContextTokens,
			),
			Parsed: renderTokenAggregate(
				prepared.HasPeakContextTokens,
				prepared.PeakContextTokens,
			),
		})
	}

	// termination_status: NULL and "" are the same pipeline state.
	// Stored NULL with a parsed value is only explained as pipeline
	// history for the incremental-append agents: UpdateSessionIncremental
	// clears the column to NULL by design, and only Claude and Codex
	// take that path. For full-replace agents a stored NULL means the
	// writing parser emitted nothing, so newly producing a value is
	// real parser drift (precisely the new-field detection the tool
	// exists to catch).
	ts := derefString(stored.TerminationStatus)
	tp := derefString(prepared.TerminationStatus)
	if ts != tp {
		d := FieldDiff{
			Field:  FieldTerminationStatus,
			Stored: renderNullableScalar(ts),
			Parsed: renderNullableScalar(tp),
		}
		if ts == "" && usesIncrementalAppend(prepared.Agent) {
			d.Informational = true
			d.Detail = "incremental-append history"
		}
		diffs = append(diffs, d)
	}

	return appendSessionMetadataDiffs(diffs, stored, prepared)
}

// appendSessionMetadataDiffs compares the parser-owned session columns
// beyond the summary set: the cwd/branch/source diagnostics and the
// parent/relationship threading fields. UpdateSessionIncremental does
// not rewrite any of them (see internal/db/sessions.go), so for the
// incremental-append agents (Claude, Codex) the stored value is frozen
// at the last full-replace write while parse-diff always re-parses the
// whole file; a difference there is benign pipeline history, not parser
// drift, and is marked informational exactly as termination_status is.
//
// The session "project" column is intentionally excluded: it is
// overwritten in prepareSessionWrite from the mutable
// worktree_project_mappings table, so a re-parse can legitimately differ
// from the stored value whenever those mappings changed since the last
// sync even with an unchanged parser. Its parser-owned input, cwd, is
// compared here instead.
func appendSessionMetadataDiffs(
	diffs []FieldDiff, stored *db.Session, prepared db.Session,
) []FieldDiff {
	agent := prepared.Agent
	diffs = appendScalarSessionDiff(
		diffs, FieldCwd, agent, stored.Cwd, prepared.Cwd,
	)
	diffs = appendScalarSessionDiff(
		diffs, FieldGitBranch, agent, stored.GitBranch, prepared.GitBranch,
	)
	diffs = appendScalarSessionDiff(
		diffs, FieldRelationshipType, agent,
		stored.RelationshipType, prepared.RelationshipType,
	)
	diffs = appendScalarSessionDiff(
		diffs, FieldSourceSessionID, agent,
		stored.SourceSessionID, prepared.SourceSessionID,
	)
	diffs = appendScalarSessionDiff(
		diffs, FieldSourceVersion, agent,
		stored.SourceVersion, prepared.SourceVersion,
	)
	// parent_session_id is *string: NULL and "" are the same state
	// (toDBSession maps "" to nil via strPtr).
	diffs = appendScalarSessionDiff(
		diffs, FieldParentSessionID, agent,
		derefString(stored.ParentSessionID),
		derefString(prepared.ParentSessionID),
	)
	if stored.ParserMalformedLines != prepared.ParserMalformedLines {
		d := FieldDiff{
			Field:  FieldParserMalformedLines,
			Stored: strconv.Itoa(stored.ParserMalformedLines),
			Parsed: strconv.Itoa(prepared.ParserMalformedLines),
		}
		markIncrementalHistory(&d, agent)
		diffs = append(diffs, d)
	}
	if stored.IsTruncated != prepared.IsTruncated {
		d := FieldDiff{
			Field:  FieldIsTruncated,
			Stored: strconv.FormatBool(stored.IsTruncated),
			Parsed: strconv.FormatBool(prepared.IsTruncated),
		}
		markIncrementalHistory(&d, agent)
		diffs = append(diffs, d)
	}
	return diffs
}

// appendScalarSessionDiff compares one parser-owned session string
// column. Both sides pass through SanitizeUTF8 the way the write path
// does, NULL and "" compare equal (callers deref *string first), long
// values are truncated, and a difference is marked informational for the
// incremental-append agents via markIncrementalHistory.
func appendScalarSessionDiff(
	diffs []FieldDiff, field, agent, stored, parsed string,
) []FieldDiff {
	sv := db.SanitizeUTF8(stored)
	pv := db.SanitizeUTF8(parsed)
	if sv == pv {
		return diffs
	}
	d := FieldDiff{
		Field:  field,
		Stored: truncateRunes(renderNullableScalar(sv), maxRenderedValueRunes),
		Parsed: truncateRunes(renderNullableScalar(pv), maxRenderedValueRunes),
	}
	markIncrementalHistory(&d, agent)
	return append(diffs, d)
}

// markIncrementalHistory tags a session-field diff as benign pipeline
// history for the incremental-append agents (Claude, Codex). See
// appendSessionMetadataDiffs for why those agents legitimately diverge.
// Any existing Detail (e.g. a long-value rune count) is preserved.
func markIncrementalHistory(d *FieldDiff, agent string) {
	if !usesIncrementalAppend(agent) {
		return
	}
	d.Informational = true
	if d.Detail == "" {
		d.Detail = "incremental-append history"
	} else {
		d.Detail += " (incremental-append history)"
	}
}

// usesIncrementalAppend reports whether an agent's sync path can clear
// termination_status to NULL via UpdateSessionIncremental. Only the
// JSONL-tail agents (Claude, Codex) take that path; see
// tryIncrementalJSONL call sites in engine.go.
func usesIncrementalAppend(agent string) bool {
	return agent == string(parser.AgentClaude) ||
		agent == string(parser.AgentCodex)
}

// tokenAggregateDiffers compares a session token aggregate as a
// (value, has-flag) unit. A coverage-flag flip is a real difference
// even when the numeric values match; values are only meaningful
// when coverage is present on both sides.
func tokenAggregateDiffers(
	storedHas bool, storedVal int,
	parsedHas bool, parsedVal int,
) bool {
	if storedHas != parsedHas {
		return true
	}
	return storedHas && storedVal != parsedVal
}

func renderTokenAggregate(has bool, v int) string {
	if !has {
		return "absent"
	}
	return strconv.Itoa(v)
}

func renderNullableScalar(s string) string {
	if s == "" {
		return "(null)"
	}
	return s
}

// appendTextFieldDiff compares a nullable text column. NULL and ""
// are equivalent (toDBSession and db.ParsedSessionName map "" to
// nil), and both sides pass through SanitizeUTF8 the way the PG-push
// fingerprint readers do.
func appendTextFieldDiff(
	diffs []FieldDiff, field string, stored, parsed *string,
) []FieldDiff {
	sv := db.SanitizeUTF8(derefString(stored))
	pv := db.SanitizeUTF8(derefString(parsed))
	if sv == pv {
		return diffs
	}
	d := FieldDiff{
		Field:  field,
		Stored: renderTextValue(stored, sv),
		Parsed: renderTextValue(parsed, pv),
	}
	var notes []string
	if utf8.RuneCountInString(sv) > maxRenderedValueRunes {
		notes = append(notes, fmt.Sprintf(
			"stored %d runes", utf8.RuneCountInString(sv),
		))
	}
	if utf8.RuneCountInString(pv) > maxRenderedValueRunes {
		notes = append(notes, fmt.Sprintf(
			"parsed %d runes", utf8.RuneCountInString(pv),
		))
	}
	d.Detail = strings.Join(notes, "; ")
	return append(diffs, d)
}

func renderTextValue(ptr *string, sanitized string) string {
	if ptr == nil {
		return "(null)"
	}
	return truncateRunes(sanitized, maxRenderedValueRunes)
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "..."
}

// messageTokenFingerprintTwin is the in-memory twin of
// db.MessageTokenFingerprint (internal/db/messages.go): identical
// field order, identical SanitizeUTF8 application, identical format
// string, over a slice ordered by ordinal ascending. Any drift
// between the two breaks the tier-1 fast path, so a white-box test
// pins them against each other through the real write pipeline.
func messageTokenFingerprintTwin(msgs []db.Message) string {
	ordered := make([]db.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	var b strings.Builder
	for _, m := range ordered {
		model := db.SanitizeUTF8(m.Model)
		tokenUsage := db.SanitizeUTF8(string(m.TokenUsage))
		claudeMsgID := db.SanitizeUTF8(m.ClaudeMessageID)
		claudeReqID := db.SanitizeUTF8(m.ClaudeRequestID)
		srcType := db.SanitizeUTF8(m.SourceType)
		srcSubtype := db.SanitizeUTF8(m.SourceSubtype)
		srcUUID := db.SanitizeUTF8(m.SourceUUID)
		srcParentUUID := db.SanitizeUTF8(m.SourceParentUUID)
		fmt.Fprintf(&b,
			"%d|%d:%s|%d:%s|%d|%d|%t|%t|%s|%s|"+
				"%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
			m.Ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			m.ContextTokens, m.OutputTokens,
			m.HasContextTokens, m.HasOutputTokens,
			claudeMsgID, claudeReqID,
			len(srcType), srcType,
			len(srcSubtype), srcSubtype,
			len(srcUUID), srcUUID,
			len(srcParentUUID), srcParentUUID,
			m.IsSidechain, m.IsCompactBoundary,
		)
	}
	return b.String()
}

// messageRoleTimeFingerprintTwin is the in-memory twin of
// db.MessageRoleTimeFingerprint (internal/db/messages.go): identical
// field order, identical sanitization, identical format string, over
// a slice ordered by ordinal ascending. Like the token twin above,
// parity with the DB query is pinned by a white-box test through the
// real write pipeline.
func messageRoleTimeFingerprintTwin(msgs []db.Message) string {
	ordered := make([]db.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	var b strings.Builder
	for _, m := range ordered {
		role := db.SanitizeUTF8(m.Role)
		fmt.Fprintf(&b, "%d|%d:%s|%d:%s;",
			m.Ordinal, len(role), role,
			len(m.Timestamp), m.Timestamp,
		)
	}
	return b.String()
}

// messageContentHashFingerprintTwin is the in-memory twin of
// db.MessageContentHashFingerprint (internal/db/messages.go):
// identical field order, identical sanitization, identical format
// string, over a slice ordered by ordinal ascending. Like the other
// twins, parity with the DB query is pinned by a white-box test
// through the real write pipeline.
func messageContentHashFingerprintTwin(msgs []db.Message) string {
	ordered := make([]db.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	var b strings.Builder
	for _, m := range ordered {
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(m.Content)))
		fmt.Fprintf(&b, "%d|%d|%x;", m.Ordinal, m.ContentLength, sum)
	}
	return b.String()
}

// messageFlagsFingerprintTwin is the in-memory twin of
// db.MessageFlagsFingerprint (internal/db/messages.go): identical field
// order, identical sanitization, identical format string, over a slice
// ordered by ordinal ascending. Parity with the DB query is pinned by a
// white-box test through the real write pipeline.
func messageFlagsFingerprintTwin(msgs []db.Message) string {
	ordered := make([]db.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	var b strings.Builder
	for _, m := range ordered {
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(m.ThinkingText)))
		fmt.Fprintf(&b, "%d|%t|%t|%t|%x;",
			m.Ordinal, m.IsSystem, m.HasThinking, m.HasToolUse, sum)
	}
	return b.String()
}

// toolCallParseDiffFingerprintTwin is the in-memory twin of
// db.ToolCallParseDiffFingerprint (internal/db/messages.go). It iterates messages
// in ordinal order and, within each, its tool calls in array order --
// the same (ordinal, insertion) order the DB query reproduces via
// ORDER BY m.ordinal, tc.id. Field order, sanitization, and the format
// string match the query exactly; parity is pinned by a white-box test
// through the real write pipeline.
func toolCallParseDiffFingerprintTwin(msgs []db.Message) string {
	ordered := make([]db.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	var b strings.Builder
	for _, m := range ordered {
		for _, tc := range m.ToolCalls {
			toolName := db.SanitizeUTF8(tc.ToolName)
			category := db.SanitizeUTF8(tc.Category)
			tu := db.SanitizeUTF8(tc.ToolUseID)
			skill := db.SanitizeUTF8(tc.SkillName)
			sub := db.SanitizeUTF8(tc.SubagentSessionID)
			sum := sha256.Sum256([]byte(db.SanitizeUTF8(tc.InputJSON)))
			fmt.Fprintf(&b,
				"%d|%d:%s|%d:%s|%d:%s|%x|%d:%s|%d:%s|%d;",
				m.Ordinal,
				len(toolName), toolName,
				len(category), category,
				len(tu), tu,
				sum,
				len(skill), skill,
				len(sub), sub,
				tc.ResultContentLength,
			)
		}
	}
	return b.String()
}

// compareMessageMetadata is the tier-2 per-message comparison, run when
// any tier-1 fingerprint (token, role/time, content, flags, or
// tool-call) mismatched. metaFPDiffers carries the token, role/time, and
// flags fingerprint results combined, since all attribute to the same
// per-message fields; toolFPDiffers carries the tool-call fingerprint.
// Both slices are aligned by ordinal value and only the overlap is
// compared; a length mismatch is message_count's job. The FP-differ
// flags guarantee a non-empty result: if the fingerprints proved
// inequality but none of the attributed fields differ on the overlap, a
// fallback diff is emitted so the session never classifies identical
// after its own fingerprint proved otherwise.
func compareMessageMetadata(
	storedMsgs, parsedMsgs []db.Message,
	metaFPDiffers, contentFPDiffers, toolFPDiffers bool,
) []FieldDiff {
	pairs := alignByOrdinal(storedMsgs, parsedMsgs)
	n := len(pairs)

	var (
		modelDiffs       int
		firstModelOrd    int
		firstModelStored string
		firstModelParsed string

		tokenDiffs       int
		firstTokenOrd    int
		firstTokenStored string
		firstTokenParsed string

		contentDiffs     int
		firstContentOrd  int
		firstContentSize string

		metaDiffs     int
		firstMetaOrd  int
		firstMetaWhat string

		toolDiffs     int
		firstToolOrd  int
		firstToolWhat string
	)
	for _, p := range pairs {
		sModel := db.SanitizeUTF8(p.stored.Model)
		pModel := db.SanitizeUTF8(p.parsed.Model)
		if sModel != pModel {
			if modelDiffs == 0 {
				firstModelOrd = p.stored.Ordinal
				firstModelStored = sModel
				firstModelParsed = pModel
			}
			modelDiffs++
		}
		if messageTokensDiffer(p.stored, p.parsed) {
			if tokenDiffs == 0 {
				firstTokenOrd = p.stored.Ordinal
				firstTokenStored = renderMessageTokenState(p.stored)
				firstTokenParsed = renderMessageTokenState(p.parsed)
			}
			tokenDiffs++
		}
		if messageContentDiffers(p.stored, p.parsed) {
			if contentDiffs == 0 {
				firstContentOrd = p.stored.Ordinal
				firstContentSize = renderContentChange(
					p.stored, p.parsed,
				)
			}
			contentDiffs++
		}
		if what := messageMetadataDiff(p.stored, p.parsed); what != "" {
			if metaDiffs == 0 {
				firstMetaOrd = p.stored.Ordinal
				firstMetaWhat = what
			}
			metaDiffs++
		}
		if what := messageToolCallsDiff(p.stored, p.parsed); what != "" {
			if toolDiffs == 0 {
				firstToolOrd = p.stored.Ordinal
				firstToolWhat = what
			}
			toolDiffs++
		}
	}

	var diffs []FieldDiff
	if modelDiffs > 0 {
		diffs = append(diffs, FieldDiff{
			Field:  FieldModels,
			Stored: renderModelSet(pairs, true),
			Parsed: renderModelSet(pairs, false),
			Detail: fmt.Sprintf(
				"%d/%d messages differ; first at ordinal %d: %s -> %s",
				modelDiffs, n, firstModelOrd,
				renderNullableScalar(firstModelStored),
				renderNullableScalar(firstModelParsed),
			),
		})
	}
	if tokenDiffs > 0 {
		diffs = append(diffs, FieldDiff{
			Field:  FieldMessageTokens,
			Stored: firstTokenStored,
			Parsed: firstTokenParsed,
			Detail: fmt.Sprintf(
				"%d/%d messages differ; first at ordinal %d",
				tokenDiffs, n, firstTokenOrd,
			),
		})
	}
	if contentDiffs > 0 {
		diffs = append(diffs, FieldDiff{
			Field:  FieldMessageContent,
			Stored: "see detail",
			Parsed: "see detail",
			Detail: fmt.Sprintf(
				"%d/%d messages differ; first at ordinal %d: %s",
				contentDiffs, n, firstContentOrd, firstContentSize,
			),
		})
	}
	if metaDiffs > 0 {
		diffs = append(diffs, FieldDiff{
			Field:  FieldMessageMetadata,
			Stored: "see detail",
			Parsed: "see detail",
			Detail: fmt.Sprintf(
				"%d/%d messages differ; first at ordinal %d: %s",
				metaDiffs, n, firstMetaOrd, firstMetaWhat,
			),
		})
	}
	if toolDiffs > 0 {
		diffs = append(diffs, FieldDiff{
			Field:  FieldToolCalls,
			Stored: "see detail",
			Parsed: "see detail",
			Detail: fmt.Sprintf(
				"%d/%d messages differ; first at ordinal %d: %s",
				toolDiffs, n, firstToolOrd, firstToolWhat,
			),
		})
	}

	// The fingerprints proved inequality but nothing on the aligned
	// overlap accounts for it (e.g. an equal-count ordinal-set shift,
	// or drift that only changed a non-overlapping message). Emit a
	// fallback so the session is never reported identical, attributed to
	// the field family whose fingerprint moved.
	if len(diffs) == 0 {
		field, which := FieldMessageMetadata, ""
		switch {
		case metaFPDiffers:
			which = "token/metadata"
		case contentFPDiffers:
			which = "content"
		case toolFPDiffers:
			field, which = FieldToolCalls, "tool-call"
		}
		if which != "" {
			diffs = append(diffs, FieldDiff{
				Field:  field,
				Stored: "fingerprint",
				Parsed: "fingerprint",
				Detail: "per-message " + which +
					" fingerprint differs but no aligned-ordinal field " +
					"accounts for it (likely an ordinal-set change)",
			})
		}
	}
	return diffs
}

// messageContentDiffers compares the per-message content contract:
// the content_length column and the body itself (sanitized like the
// fingerprint), so equal-length rewrites still attribute to
// message_content instead of falling through to the generic
// fingerprint-mismatch diff.
func messageContentDiffers(stored, parsed db.Message) bool {
	return stored.ContentLength != parsed.ContentLength ||
		db.SanitizeUTF8(stored.Content) != db.SanitizeUTF8(parsed.Content)
}

// renderContentChange describes the first content change. Bodies are
// never echoed (they can be huge and are exactly what the terminal
// renderer must not leak); only sizes are reported.
func renderContentChange(stored, parsed db.Message) string {
	if stored.ContentLength != parsed.ContentLength {
		return fmt.Sprintf(
			"%d -> %d bytes",
			stored.ContentLength, parsed.ContentLength,
		)
	}
	return fmt.Sprintf(
		"body differs at equal length (%d bytes)", stored.ContentLength,
	)
}

// messageMetadataDiff reports the first differing per-message field
// among the fingerprint-covered identity columns that models/tokens do
// not separately surface: role, timestamp, source-tracking ids, the
// sidechain/compact-boundary flags, and the thinking/system/tool-use
// flags (is_system, has_thinking, has_tool_use, thinking_text). Returns
// "" when they match.
func messageMetadataDiff(stored, parsed db.Message) string {
	switch {
	case db.SanitizeUTF8(stored.Role) != db.SanitizeUTF8(parsed.Role):
		return fmt.Sprintf("role %q -> %q", stored.Role, parsed.Role)
	case stored.Timestamp != parsed.Timestamp:
		return fmt.Sprintf(
			"timestamp %q -> %q", stored.Timestamp, parsed.Timestamp,
		)
	case stored.IsSidechain != parsed.IsSidechain:
		return fmt.Sprintf(
			"is_sidechain %t -> %t",
			stored.IsSidechain, parsed.IsSidechain,
		)
	case stored.IsCompactBoundary != parsed.IsCompactBoundary:
		return fmt.Sprintf(
			"is_compact_boundary %t -> %t",
			stored.IsCompactBoundary, parsed.IsCompactBoundary,
		)
	case stored.IsSystem != parsed.IsSystem:
		return fmt.Sprintf(
			"is_system %t -> %t", stored.IsSystem, parsed.IsSystem,
		)
	case stored.HasThinking != parsed.HasThinking:
		return fmt.Sprintf(
			"has_thinking %t -> %t", stored.HasThinking, parsed.HasThinking,
		)
	case stored.HasToolUse != parsed.HasToolUse:
		return fmt.Sprintf(
			"has_tool_use %t -> %t", stored.HasToolUse, parsed.HasToolUse,
		)
	case db.SanitizeUTF8(stored.ThinkingText) !=
		db.SanitizeUTF8(parsed.ThinkingText):
		return "thinking_text differs"
	case db.SanitizeUTF8(stored.SourceType) !=
		db.SanitizeUTF8(parsed.SourceType):
		return "source_type differs"
	case db.SanitizeUTF8(stored.SourceSubtype) !=
		db.SanitizeUTF8(parsed.SourceSubtype):
		return "source_subtype differs"
	case db.SanitizeUTF8(stored.SourceUUID) !=
		db.SanitizeUTF8(parsed.SourceUUID):
		return "source_uuid differs"
	case db.SanitizeUTF8(stored.SourceParentUUID) !=
		db.SanitizeUTF8(parsed.SourceParentUUID):
		return "source_parent_uuid differs"
	case db.SanitizeUTF8(stored.ClaudeMessageID) !=
		db.SanitizeUTF8(parsed.ClaudeMessageID):
		return "claude_message_id differs"
	case db.SanitizeUTF8(stored.ClaudeRequestID) !=
		db.SanitizeUTF8(parsed.ClaudeRequestID):
		return "claude_request_id differs"
	default:
		return ""
	}
}

// messageToolCallsDiff reports the first parser-owned tool_call
// difference between two aligned messages: a count change, or a
// per-position change in tool_name, category, tool_use_id, input_json,
// skill_name, subagent_session_id, or result_content_length. Tool calls
// are compared by array position, which matches the (ordinal, insertion)
// order the stored fingerprint and attachToolCalls reproduce. Returns ""
// when they match.
func messageToolCallsDiff(stored, parsed db.Message) string {
	if len(stored.ToolCalls) != len(parsed.ToolCalls) {
		return fmt.Sprintf(
			"tool_call count %d -> %d",
			len(stored.ToolCalls), len(parsed.ToolCalls),
		)
	}
	for i := range stored.ToolCalls {
		if what := toolCallDiff(
			stored.ToolCalls[i], parsed.ToolCalls[i],
		); what != "" {
			return what
		}
	}
	return ""
}

// toolCallDiff reports the first differing parser-owned column of one
// tool call. The database-assigned ids and the (possibly blocked)
// result body are not compared; result content is compared only by
// length, the same sizes-not-bodies rule message content follows.
func toolCallDiff(stored, parsed db.ToolCall) string {
	switch {
	case db.SanitizeUTF8(stored.ToolName) !=
		db.SanitizeUTF8(parsed.ToolName):
		return fmt.Sprintf(
			"tool_name %q -> %q", stored.ToolName, parsed.ToolName,
		)
	case db.SanitizeUTF8(stored.Category) !=
		db.SanitizeUTF8(parsed.Category):
		return fmt.Sprintf(
			"category %q -> %q", stored.Category, parsed.Category,
		)
	case db.SanitizeUTF8(stored.ToolUseID) !=
		db.SanitizeUTF8(parsed.ToolUseID):
		return "tool_use_id differs"
	case db.SanitizeUTF8(stored.InputJSON) !=
		db.SanitizeUTF8(parsed.InputJSON):
		return "input_json differs"
	case db.SanitizeUTF8(stored.SkillName) !=
		db.SanitizeUTF8(parsed.SkillName):
		return fmt.Sprintf(
			"skill_name %q -> %q", stored.SkillName, parsed.SkillName,
		)
	case db.SanitizeUTF8(stored.SubagentSessionID) !=
		db.SanitizeUTF8(parsed.SubagentSessionID):
		return "subagent_session_id differs"
	case stored.ResultContentLength != parsed.ResultContentLength:
		return fmt.Sprintf(
			"result_content_length %d -> %d",
			stored.ResultContentLength, parsed.ResultContentLength,
		)
	default:
		return ""
	}
}

type ordinalPair struct {
	stored db.Message
	parsed db.Message
}

// alignByOrdinal intersects two message slices on ordinal value.
// Both inputs are sorted by ordinal (defensively re-sorted here) and
// ordinals within a session are unique.
func alignByOrdinal(stored, parsed []db.Message) []ordinalPair {
	s := make([]db.Message, len(stored))
	copy(s, stored)
	sort.SliceStable(s, func(i, j int) bool {
		return s[i].Ordinal < s[j].Ordinal
	})
	p := make([]db.Message, len(parsed))
	copy(p, parsed)
	sort.SliceStable(p, func(i, j int) bool {
		return p[i].Ordinal < p[j].Ordinal
	})

	var pairs []ordinalPair
	i, j := 0, 0
	for i < len(s) && j < len(p) {
		switch {
		case s[i].Ordinal < p[j].Ordinal:
			i++
		case s[i].Ordinal > p[j].Ordinal:
			j++
		default:
			pairs = append(pairs, ordinalPair{
				stored: s[i], parsed: p[j],
			})
			i++
			j++
		}
	}
	return pairs
}

// messageTokensDiffer compares the per-message token contract
// fields: raw token_usage payload, context/output values, and
// coverage via Message.TokenPresence semantics (which preserve
// legacy-row inference).
func messageTokensDiffer(stored, parsed db.Message) bool {
	if db.SanitizeUTF8(string(stored.TokenUsage)) !=
		db.SanitizeUTF8(string(parsed.TokenUsage)) {
		return true
	}
	if stored.ContextTokens != parsed.ContextTokens ||
		stored.OutputTokens != parsed.OutputTokens {
		return true
	}
	sCtx, sOut := stored.TokenPresence()
	pCtx, pOut := parsed.TokenPresence()
	return sCtx != pCtx || sOut != pOut
}

func renderMessageTokenState(m db.Message) string {
	hasCtx, hasOut := m.TokenPresence()
	ctx := "absent"
	if hasCtx {
		ctx = strconv.Itoa(m.ContextTokens)
	}
	out := "absent"
	if hasOut {
		out = strconv.Itoa(m.OutputTokens)
	}
	return fmt.Sprintf(
		"context=%s output=%s usage_bytes=%d",
		ctx, out, len(m.TokenUsage),
	)
}

// renderModelSet renders the distinct sorted models of one side of
// the aligned pairs.
func renderModelSet(pairs []ordinalPair, storedSide bool) string {
	set := map[string]bool{}
	for _, p := range pairs {
		m := p.parsed.Model
		if storedSide {
			m = p.stored.Model
		}
		set[db.SanitizeUTF8(m)] = true
	}
	models := make([]string, 0, len(set))
	for m := range set {
		if m == "" {
			m = "(none)"
		}
		models = append(models, m)
	}
	sort.Strings(models)
	return strings.Join(models, ", ")
}

// usageTokenTotals aggregates the per-token-class sums of a usage
// event set. Cost columns are pricing enrichment, not parser output,
// and are deliberately excluded.
type usageTokenTotals struct {
	input         int
	output        int
	cacheCreation int
	cacheRead     int
	reasoning     int
}

func sumUsageTokenTotals(events []db.UsageEvent) usageTokenTotals {
	var t usageTokenTotals
	for _, ev := range events {
		t.input += ev.InputTokens
		t.output += ev.OutputTokens
		t.cacheCreation += ev.CacheCreationInputTokens
		t.cacheRead += ev.CacheReadInputTokens
		t.reasoning += ev.ReasoningTokens
	}
	return t
}

func (t usageTokenTotals) render() string {
	return fmt.Sprintf(
		"input=%d output=%d cache_creation=%d cache_read=%d reasoning=%d",
		t.input, t.output, t.cacheCreation, t.cacheRead, t.reasoning,
	)
}

func usageTotalsDetail(stored, parsed usageTokenTotals) string {
	var parts []string
	add := func(name string, s, p int) {
		if s != p {
			parts = append(parts, fmt.Sprintf("%s %d -> %d", name, s, p))
		}
	}
	add("input", stored.input, parsed.input)
	add("output", stored.output, parsed.output)
	add("cache_creation", stored.cacheCreation, parsed.cacheCreation)
	add("cache_read", stored.cacheRead, parsed.cacheRead)
	add("reasoning", stored.reasoning, parsed.reasoning)
	return strings.Join(parts, "; ")
}

// usageEventKey identifies one event inside the order-insensitive
// multiset. The DedupKey form still folds in source, model, and
// parser-owned token fields so that attribution drift (e.g. the same
// event re-tagged to a different model) or per-event token
// redistribution under stable dedup keys surfaces as a composition
// diff rather than passing silently.
func usageEventKey(ev db.UsageEvent) string {
	if ev.DedupKey != "" {
		return strings.Join([]string{
			"dedup",
			ev.DedupKey,
			ev.Source,
			ev.Model,
			strconv.Itoa(ev.InputTokens),
			strconv.Itoa(ev.OutputTokens),
			strconv.Itoa(ev.CacheCreationInputTokens),
			strconv.Itoa(ev.CacheReadInputTokens),
			strconv.Itoa(ev.ReasoningTokens),
		}, "|")
	}
	ord := "-"
	if ev.MessageOrdinal != nil {
		ord = strconv.Itoa(*ev.MessageOrdinal)
	}
	return strings.Join([]string{
		"tuple",
		ev.Source,
		ev.Model,
		strconv.Itoa(ev.InputTokens),
		strconv.Itoa(ev.OutputTokens),
		strconv.Itoa(ev.CacheCreationInputTokens),
		strconv.Itoa(ev.CacheReadInputTokens),
		strconv.Itoa(ev.ReasoningTokens),
		ev.OccurredAt,
		ord,
	}, "|")
}

func usageEventMultiset(events []db.UsageEvent) map[string]int {
	set := make(map[string]int, len(events))
	for _, ev := range events {
		set[usageEventKey(ev)]++
	}
	return set
}

func multisetsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// firstDifferingUsageKey returns the lexicographically smallest key
// whose multiplicity differs between the two multisets.
func firstDifferingUsageKey(a, b map[string]int) string {
	var keys []string
	seen := map[string]bool{}
	for k := range a {
		if a[k] != b[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k := range b {
		if !seen[k] && a[k] != b[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

// compareUsageEvents compares the two event sets as order-insensitive
// multisets. Stored rows come back ordered by (occurred_at, id) while
// in-memory events carry no ids, so ordering must not matter.
func compareUsageEvents(
	storedEvents, parsedEvents []db.UsageEvent,
) []FieldDiff {
	if len(storedEvents) == 0 && len(parsedEvents) == 0 {
		return nil
	}

	storedSet := usageEventMultiset(storedEvents)
	parsedSet := usageEventMultiset(parsedEvents)
	storedTotals := sumUsageTokenTotals(storedEvents)
	parsedTotals := sumUsageTokenTotals(parsedEvents)

	countDiff := len(storedEvents) != len(parsedEvents)
	totalsDiff := storedTotals != parsedTotals
	compositionDiff := !multisetsEqual(storedSet, parsedSet)
	if !countDiff && !totalsDiff && !compositionDiff {
		return nil
	}

	var diffs []FieldDiff
	if countDiff {
		diffs = append(diffs, FieldDiff{
			Field:  FieldUsageEventCount,
			Stored: strconv.Itoa(len(storedEvents)),
			Parsed: strconv.Itoa(len(parsedEvents)),
		})
	}
	switch {
	case totalsDiff:
		diffs = append(diffs, FieldDiff{
			Field:  FieldUsageEventTotals,
			Stored: storedTotals.render(),
			Parsed: parsedTotals.render(),
			Detail: usageTotalsDetail(storedTotals, parsedTotals),
		})
	case !countDiff:
		// Composition drift with equal cardinality and token
		// totals (e.g. a model or timestamp attribution change).
		diffs = append(diffs, FieldDiff{
			Field:  FieldUsageEventTotals,
			Stored: storedTotals.render(),
			Parsed: parsedTotals.render(),
			Detail: "event composition differs; token totals are equal; " +
				"first differing event: " +
				firstDifferingUsageKey(storedSet, parsedSet),
		})
	}
	return diffs
}

// classifyParseDiffSession resolves the classification precedence for
// one parsed session. Pure function so the precedence is unit
// testable; the caller is responsible for totals and field counts.
//   - needsRetry: the parse was flagged transient low-fidelity.
//   - prepared: prepareSessionWrite accepted the session (false means
//     the OpenCode archive-preserve veto fired).
//   - hasStored / storedTrashed: archive row state.
//   - pendingResync: stored data_version is behind the binary.
//   - realDiffs: count of non-informational field diffs.
func classifyParseDiffSession(
	needsRetry, prepared, hasStored, storedTrashed,
	pendingResync bool,
	realDiffs int,
) (DiffClass, string) {
	switch {
	case needsRetry:
		return DiffNeedsRetry,
			"transient low-fidelity parse; differences expected"
	case !prepared:
		return DiffExcluded, "archive-preserved"
	case !hasStored:
		return DiffNewOnDisk, ""
	case storedTrashed:
		// The parser still emits this session, but the user trashed it
		// in the archive; sync preserves trash (UpsertSession returns
		// ErrSessionTrashed) rather than deleting, so this is neither a
		// parser exclusion nor drift. Bucket it as skipped, matching
		// how a not-re-parsed trashed row is reported.
		return DiffSkipped, "trashed in archive"
	case pendingResync:
		return DiffPendingResync, ""
	case realDiffs > 0:
		return DiffChanged, ""
	default:
		return DiffIdentical, ""
	}
}
