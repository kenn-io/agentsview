package sync

import (
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// This file is the single centralized validation and sanitization pass
// over parsed output (messages, usage events, session) at the
// sync->persistence seam. Every session write flows through
// validateAndSanitize after the toDB* converters produce the db-layer
// rows but before persistence, so all agents are covered uniformly
// instead of relying on scattered per-extraction-site guards.
//
// POLICY: prefer sanitize/clamp over DROP. A not-yet-considered agent
// format may legitimately carry a value we do not expect; clamping or
// blanking the offending field keeps the rest of the row rather than
// discarding a real session. No row is ever dropped here.
//
// Per-field policy:
//   - Role: out-of-enum (see parser.ValidRole) is coerced to "" rather
//     than persisted as a garbage string. Known roles, including the
//     empty role, pass through unchanged.
//   - Free-text strings (content, thinking text, model, source-tracking
//     ids, usage source/cost fields): passed through db.SanitizeUTF8,
//     which strips NUL bytes, fixes invalid UTF-8, and removes control
//     runes (C0/C1/DEL) other than \n \t \r. The per-rune strip matters
//     because C1 controls are valid two-byte UTF-8 and survive a plain
//     UTF-8 validity check; a verified repro carried an ESC ]0;...BEL
//     terminal escape through intact.
//   - Model: clamped to maxModelLen bytes. Real model ids are well under
//     that; a verified repro pushed a multi-megabyte printable string
//     through as a model id.
//   - Token counts: clamped to maxPlausibleTokens. Implausible values
//     (e.g. a nanosecond-latency counter misread as a token field) are
//     bounded rather than stored verbatim.
//   - Timestamps: an absurd value outside the plausibility window is
//     blanked. Empty-string handling is preserved as-is so downstream
//     localTime treats a blanked timestamp as invalid.
//
// DELIBERATE EXCLUSION: Message.TokenUsage (json.RawMessage) and the
// nested ToolCalls/ToolResults string fields are intentionally NOT run
// through the central sanitizer here. They are sanitized symmetrically by
// the local fingerprint builders and the pg-push path, so idempotency and
// backend parity still hold. Usage aggregation separately clamps numeric
// TokenUsage fields as it reads them, so a retained corrupt raw value cannot
// inflate dashboards or estimated cost; leaving these nested values out only
// means local SQLite columns may retain control bytes in those nested values.
// Expanding sanitization into nested structures is deferred to a
// follow-up to avoid changing the bytes fingerprints are computed over in
// this commit.
//
// CRITICAL idempotency invariant: db.SanitizeUTF8 is the shared
// sanitization seam used here, by the local fingerprint builders, and
// by the PG push/readback path. Because the values stored by this pass
// are already sanitized, re-running SanitizeUTF8 on the readback (as the
// fingerprint and push paths do) is a no-op, so fingerprints computed
// over stored rows match the ones computed on push and a pushed session
// is not needlessly rewritten. validateAndSanitize must therefore stay
// idempotent: validateAndSanitize over its own output produces no
// further fixes.

const (
	// maxModelLen bounds the stored model id length in bytes. Real
	// model identifiers are far shorter; the cap defends against a
	// large printable field misclassified as a model name.
	maxModelLen = 128

	// maxPlausibleTokens bounds token counts to a sane maximum. It
	// mirrors the parser-side constant of the same name in
	// internal/parser/antigravity.go and the db-layer aggregation clamp;
	// all three are intentionally equal.
	maxPlausibleTokens = db.MaxPlausibleTokens

	// minPlausibleYear and maxPlausibleYear bound acceptable
	// timestamps. The window matches the 2000..2100 plausibility range
	// the antigravity proto parser already uses for decoded timestamps.
	minPlausibleYear = 2000
	maxPlausibleYear = 2100
)

// validationStats records per-category fix counts from a single
// validateAndSanitize pass. It exists so a later anomaly-counter task
// can surface these numbers; current callers may ignore it.
type validationStats struct {
	ControlCharsStripped int
	ModelClamped         int
	TokensClamped        int
	RoleCoerced          int
	TimestampsBlanked    int
}

// add accumulates another stats value into the receiver.
func (s *validationStats) add(o validationStats) {
	s.ControlCharsStripped += o.ControlCharsStripped
	s.ModelClamped += o.ModelClamped
	s.TokensClamped += o.TokensClamped
	s.RoleCoerced += o.RoleCoerced
	s.TimestampsBlanked += o.TimestampsBlanked
}

// validateAndSanitize applies the central validation contract in place
// to the db-layer rows for one session and returns the fix counts. Any
// of the arguments may be nil/empty; only the non-empty ones are
// processed. It is the single seam every session write passes through
// before persistence, and it is idempotent.
func validateAndSanitize(
	s *db.Session, msgs []db.Message, events []db.UsageEvent,
) validationStats {
	var stats validationStats
	if s != nil {
		stats.add(sanitizeSession(s))
	}
	for i := range msgs {
		stats.add(sanitizeMessage(&msgs[i]))
	}
	for i := range events {
		stats.add(sanitizeUsageEvent(&events[i]))
	}
	return stats
}

// sanitizeMessage applies the contract to a single message row.
func sanitizeMessage(m *db.Message) validationStats {
	var stats validationStats

	if !parser.ValidRole(parser.RoleType(m.Role)) {
		m.Role = ""
		stats.RoleCoerced++
	}

	// Adjust ContentLength by the DELTA of bytes the sanitizer removed
	// rather than overwriting it with len(Content). Some parsers set
	// ContentLength to a semantic value that intentionally differs from
	// len(Content) -- e.g. a thinking/reasoning-inclusive length, or a
	// tool-only message with empty display Content but a nonzero work
	// length. Overwriting would corrupt content_length (and the
	// fingerprint/diff behavior that reads it) for normal messages where
	// nothing was stripped. By subtracting only the removed bytes we
	// preserve the parser's semantic length when no control runes are
	// stripped, and keep content_length consistent with the stored bytes
	// when they are. The latter also prevents a spurious archive-update
	// rewrite every sync: visualStudioCopilotMessageHasArchiveUpdate
	// treats equal ContentLength with differing Content as an update,
	// which would false-fire forever when stored content was stripped but
	// its length stayed raw; the reconcile path sanitizes re-parsed
	// content before comparing, so the lengths stay aligned.
	origLen := len(m.Content)
	sanitizeStringField(&m.Content, &stats)
	if removed := origLen - len(m.Content); removed > 0 {
		m.ContentLength -= removed
		if m.ContentLength < 0 {
			m.ContentLength = 0
		}
	}
	origThinkingLen := len(m.ThinkingText)
	sanitizeStringField(&m.ThinkingText, &stats)
	if removed := origThinkingLen - len(m.ThinkingText); removed > 0 {
		// Some parsers include ThinkingText in ContentLength. If the
		// current length still has semantic bytes beyond the sanitized
		// visible Content, subtract stripped thinking bytes from that
		// excess without driving the length below stored Content.
		excess := m.ContentLength - len(m.Content)
		if excess > 0 {
			if removed > excess {
				removed = excess
			}
			m.ContentLength -= removed
		}
	}
	sanitizeStringField(&m.ClaudeMessageID, &stats)
	sanitizeStringField(&m.ClaudeRequestID, &stats)
	sanitizeStringField(&m.SourceType, &stats)
	sanitizeStringField(&m.SourceSubtype, &stats)
	sanitizeStringField(&m.SourceUUID, &stats)
	sanitizeStringField(&m.SourceParentUUID, &stats)

	if clampModel(&m.Model) {
		stats.ModelClamped++
	}
	sanitizeStringField(&m.Model, &stats)

	if clampTokens(&m.ContextTokens) {
		stats.TokensClamped++
	}
	if clampTokens(&m.OutputTokens) {
		stats.TokensClamped++
	}

	if blankImplausibleTimestamp(&m.Timestamp) {
		stats.TimestampsBlanked++
	}

	return stats
}

// sanitizeUsageEvent applies the contract to a single usage event row.
func sanitizeUsageEvent(ev *db.UsageEvent) validationStats {
	var stats validationStats

	sanitizeStringField(&ev.Source, &stats)
	sanitizeStringField(&ev.CostStatus, &stats)
	sanitizeStringField(&ev.CostSource, &stats)
	sanitizeStringField(&ev.DedupKey, &stats)

	if clampModel(&ev.Model) {
		stats.ModelClamped++
	}
	sanitizeStringField(&ev.Model, &stats)

	if clampUsageEventTokens(ev.Source, &ev.InputTokens) {
		stats.TokensClamped++
	}
	if clampUsageEventTokens(ev.Source, &ev.OutputTokens) {
		stats.TokensClamped++
	}
	if clampUsageEventTokens(ev.Source, &ev.CacheCreationInputTokens) {
		stats.TokensClamped++
	}
	if clampUsageEventTokens(ev.Source, &ev.CacheReadInputTokens) {
		stats.TokensClamped++
	}
	if clampUsageEventTokens(ev.Source, &ev.ReasoningTokens) {
		stats.TokensClamped++
	}

	if blankImplausibleTimestamp(&ev.OccurredAt) {
		stats.TimestampsBlanked++
	}

	return stats
}

func clampUsageEventTokens(source string, p *int) bool {
	if p == nil {
		return false
	}
	if source == "session" {
		if *p < 0 {
			*p = 0
			return true
		}
		return false
	}
	return clampTokens(p)
}

// sanitizeSession applies the contract to the session row's free-text
// and timestamp fields. Session token totals (TotalOutputTokens,
// PeakContextTokens) are NOT clamped to maxPlausibleTokens here: a
// session total legitimately exceeds the per-message bound by summing
// many rows, so clamping it to that bound would corrupt long sessions.
// Instead the write paths re-derive row-derived totals from the
// per-message and per-usage-event rows AFTER this pass clamps them (see
// the reconciliation in prepareSessionWrite and the clamped accumulation
// feeding writeIncremental), so a corrupt single row cannot stay stranded
// in the session totals while its row is clamped. Summary-derived totals
// (agents that set session totals from a session-level usage summary,
// e.g. Warp/Vibe/Hermes) match no per-row source and are left as-is.
func sanitizeSession(s *db.Session) validationStats {
	var stats validationStats

	sanitizeStringField(&s.Project, &stats)
	sanitizeStringField(&s.Machine, &stats)
	sanitizeStringField(&s.Agent, &stats)
	sanitizeStringField(&s.Cwd, &stats)
	sanitizeStringField(&s.GitBranch, &stats)
	sanitizeStringField(&s.SourceSessionID, &stats)
	sanitizeStringField(&s.SourceVersion, &stats)
	sanitizeStringPtrField(s.FirstMessage, &stats)
	sanitizeStringPtrField(s.SessionName, &stats)
	sanitizeStringPtrField(s.TerminationStatus, &stats)

	if next, blanked := blankImplausibleTimestampPtr(s.StartedAt); blanked {
		s.StartedAt = next
		stats.TimestampsBlanked++
	}
	if next, blanked := blankImplausibleTimestampPtr(s.EndedAt); blanked {
		s.EndedAt = next
		stats.TimestampsBlanked++
	}

	return stats
}

// sanitizeStringField sanitizes a free-text string in place and counts
// a control-char strip when the value changed. db.SanitizeUTF8 also
// removes NUL bytes and fixes invalid UTF-8; any of those changing the
// string counts under the same category since they share the seam.
func sanitizeStringField(p *string, stats *validationStats) {
	clean := db.SanitizeUTF8(*p)
	if clean != *p {
		stats.ControlCharsStripped++
		*p = clean
	}
}

// sanitizeStringPtrField sanitizes the target of a *string in place
// when non-nil. The pointer itself is left as-is (nil stays nil, a
// pointer to "" stays a pointer to "") so pointer presence semantics
// downstream are unchanged.
func sanitizeStringPtrField(p *string, stats *validationStats) {
	if p == nil {
		return
	}
	sanitizeStringField(p, stats)
}

// clampModel truncates an over-long model id in place and reports
// whether it changed. Truncation respects UTF-8 boundaries so the
// result stays valid (and a later SanitizeUTF8 stays a no-op).
func clampModel(p *string) bool {
	if len(*p) <= maxModelLen {
		return false
	}
	cut := maxModelLen
	// Back up to a rune boundary so we never split a multibyte rune.
	for cut > 0 && !utf8RuneStart((*p)[cut]) {
		cut--
	}
	*p = (*p)[:cut]
	return true
}

// utf8RuneStart reports whether b is the first byte of a UTF-8
// encoded rune (i.e. not a continuation byte 0b10xxxxxx).
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

// clampedTokens bounds a token count to [0, maxPlausibleTokens] and
// returns the result. Negative counts (which a corrupt parse could
// produce) are floored at 0. It is the pure form shared by clampTokens
// (in-place message/event sanitization) and the message-derived session
// aggregate accumulation, so both apply the identical per-message bound.
func clampedTokens(v int) int {
	switch {
	case v < 0:
		return 0
	case v > maxPlausibleTokens:
		return maxPlausibleTokens
	default:
		return v
	}
}

// clampTokens bounds a token count in place and reports whether it
// changed.
func clampTokens(p *int) bool {
	c := clampedTokens(*p)
	if c != *p {
		*p = c
		return true
	}
	return false
}

// blankImplausibleTimestamp blanks a stored timestamp string in place
// when it parses to a time outside the plausibility window, returning
// whether it changed. An empty string is left empty (no change), and an
// unparseable-but-nonempty value is left as-is: downstream localTime
// already treats both as invalid, so blanking only the parseable-yet-
// absurd case avoids reformatting otherwise-untouched values.
func blankImplausibleTimestamp(p *string) bool {
	if *p == "" {
		return false
	}
	t, ok := parseStoredTimestamp(*p)
	if !ok {
		return false
	}
	if isPlausibleTime(t) {
		return false
	}
	*p = ""
	return true
}

// blankImplausibleTimestampPtr reports whether the timestamp p points to is
// implausible and, if so, returns nil so the column is stored NULL (matching
// how toDBSession leaves a zero time as nil); otherwise it returns p unchanged.
func blankImplausibleTimestampPtr(p *string) (*string, bool) {
	if p == nil {
		return nil, false
	}
	v := *p
	if blankImplausibleTimestamp(&v) {
		return nil, true
	}
	return p, false
}

// parseStoredTimestamp parses a timestamp stored in the same formats
// the read path (db.localTime) accepts.
func parseStoredTimestamp(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// isPlausibleTime reports whether t falls within the accepted year
// window.
func isPlausibleTime(t time.Time) bool {
	y := t.UTC().Year()
	return y >= minPlausibleYear && y <= maxPlausibleYear
}
