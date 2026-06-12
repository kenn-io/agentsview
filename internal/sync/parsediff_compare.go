package sync

// Parse-diff comparator: compares one freshly parsed, sync-normalized
// session against its stored SQLite rows and emits FieldDiff entries
// for the contract fields only. All comparisons mirror the
// normalization the write path applies (nil <-> "" equivalence,
// SanitizeUTF8, TokenPresence inference) so that an unchanged parser
// produces zero diffs against rows it wrote itself.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
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

	// Tier 1: cheap exact fingerprint over per-message model and
	// token metadata. Equal fingerprints prove models and message
	// tokens are identical without loading any message rows.
	storedFP, err := e.db.MessageTokenFingerprint(stored.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff: message fingerprint for %s: %w",
			stored.ID, err,
		)
	}
	if messageTokenFingerprintTwin(msgs) != storedFP {
		// Tier 2: load stored rows and attribute the mismatch to
		// the contract fields (models, message tokens).
		storedMsgs, err := e.db.GetAllMessages(ctx, stored.ID)
		if err != nil {
			return nil, fmt.Errorf(
				"parse-diff: messages for %s: %w", stored.ID, err,
			)
		}
		diffs = append(
			diffs, compareMessageMetadata(storedMsgs, msgs)...,
		)
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
	diffs = appendTextFieldDiff(
		diffs, FieldSessionName,
		stored.SessionName, prepared.SessionName,
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
	// Stored NULL with a parsed value is explained by incremental
	// appends (UpdateSessionIncremental clears the column to NULL by
	// design), so it is informational rather than parser drift.
	ts := derefString(stored.TerminationStatus)
	tp := derefString(prepared.TerminationStatus)
	if ts != tp {
		d := FieldDiff{
			Field:  FieldTerminationStatus,
			Stored: renderNullableScalar(ts),
			Parsed: renderNullableScalar(tp),
		}
		if ts == "" {
			d.Informational = true
			d.Detail = "incremental-append history"
		}
		diffs = append(diffs, d)
	}
	return diffs
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

// compareMessageMetadata is the tier-2 per-message comparison. Both
// slices are aligned by ordinal value and only the overlap is
// compared; a length mismatch is message_count's job.
func compareMessageMetadata(
	storedMsgs, parsedMsgs []db.Message,
) []FieldDiff {
	pairs := alignByOrdinal(storedMsgs, parsedMsgs)
	n := len(pairs)
	if n == 0 {
		return nil
	}

	var (
		modelDiffs       int
		firstModelOrd    int
		firstModelStored string
		firstModelParsed string

		tokenDiffs       int
		firstTokenOrd    int
		firstTokenStored string
		firstTokenParsed string
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
	return diffs
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
// multiset: DedupKey when present, otherwise the full content tuple.
func usageEventKey(ev db.UsageEvent) string {
	if ev.DedupKey != "" {
		return "dedup:" + ev.DedupKey
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
		return DiffExcluded, "trashed in archive"
	case pendingResync:
		return DiffPendingResync, ""
	case realDiffs > 0:
		return DiffChanged, ""
	default:
		return DiffIdentical, ""
	}
}
