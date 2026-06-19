// ABOUTME: session-list sort registry — maps the public --sort/?order_by keys
// ABOUTME: to dialect-aware ORDER BY expressions and keyset cursor values.
package db

import (
	"fmt"
	"math"
	"strconv"
)

// valueKind classifies a sort column's value so the keyset cursor can bind it
// with the right Go type and per-dialect cast. SQLite relies on this to avoid
// type-affinity surprises (an integer column compared against a bound string
// always sorts below it); PostgreSQL relies on it for ::bigint / ::timestamptz
// casts on numbered placeholders.
type valueKind int

const (
	kindTimestamp valueKind = iota // ISO-8601 text / timestamptz
	kindInt
	kindReal
	kindText
)

// Sentinels make a nullable sort column non-null inside the keyset comparison
// so the row-value predicate is always well-defined and NULLs land last in both
// directions, identically across backends. They sit far outside any real value;
// the SQL literal and the Go value must be numerically equal (string form is
// internal to the cursor token).
const (
	sentinelIntDescSQL  = "-9223372036854775808" // math.MinInt64
	sentinelIntAscSQL   = "9223372036854775807"  // math.MaxInt64
	sentinelRealDescSQL = "-1e18"
	sentinelRealAscSQL  = "1e18"
)

// SessionSort describes one orderable column for `session list`. It is exported
// so the PostgreSQL and DuckDB stores render ordering and keyset pagination from
// this single source of truth rather than duplicating ORDER BY strings.
type SessionSort struct {
	key               string
	kind              valueKind
	defaultDescending bool
	// nullable marks columns with no natural non-null fallback (health,
	// context-pressure); their order expression is wrapped in a direction-aware
	// sentinel COALESCE so NULLs sort last.
	nullable bool
	// baseExpr returns the (possibly nullable) SQL expression for this sort in
	// the given dialect. Timestamp sorts use the dialect's empty-string-aware
	// wrapping; everything else is a plain column name shared across backends.
	baseExpr func(d QueryDialect) string
	// value returns the row's sort value formatted as the cursor stores it, plus
	// ok=false when the underlying column is NULL.
	value func(s *Session) (string, bool)
}

// orderExpr renders the non-null SQL expression used in both ORDER BY and the
// keyset cursor predicate for the resolved direction.
func (sp SessionSort) orderExpr(d QueryDialect, desc bool) string {
	e := sp.baseExpr(d)
	if sp.nullable {
		e = "COALESCE(" + e + ", " + sentinelLiteral(sp.kind, desc) + ")"
	}
	return e
}

// cursorValue returns the string-encoded comparison value for a page's last
// row, substituting the matching sentinel when the column is NULL.
func (sp SessionSort) cursorValue(s *Session, desc bool) string {
	if v, ok := sp.value(s); ok {
		return v
	}
	return sentinelGoString(sp.kind, desc)
}

// ResolveDescending returns the effective direction: the key's canonical default
// unless the caller supplied an explicit override.
func (sp SessionSort) ResolveDescending(descending *bool) bool {
	if descending != nil {
		return *descending
	}
	return sp.defaultDescending
}

// NextCursor builds the pagination token for a page's last row under the
// resolved sort and direction.
func (sp SessionSort) NextCursor(last *Session, desc bool, total int) SessionCursor {
	v := sp.cursorValue(last, desc)
	cur := SessionCursor{
		ID:    last.ID,
		Total: total,
		Sort:  sp.key,
		Desc:  desc,
		Value: v,
	}
	if sp.key == defaultSortKey {
		// Keep the legacy field populated so default-sort cursors remain
		// decodable by older readers during a rollout.
		cur.EndedAt = v
	}
	return cur
}

// CursorPredicateValue validates a decoded cursor against the resolved sort and
// returns the typed value the keyset predicate must bind. A cursor minted under
// a different sort/direction, or with an unparseable value, is rejected as an
// invalid cursor rather than silently producing wrong pages.
func (sp SessionSort) CursorPredicateValue(cur SessionCursor, desc bool) (any, error) {
	if !sp.cursorMatches(cur, desc) {
		return nil, fmt.Errorf("%w: sort mismatch", ErrInvalidCursor)
	}
	raw := cur.Value
	if cur.Sort == "" {
		raw = cur.EndedAt
	}
	v, err := typedCursorValue(raw, sp.kind)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return v, nil
}

// cursorMatches reports whether a decoded cursor was minted under the resolved
// sort and direction. Legacy cursors (no Sort) are only valid for the default
// recent-descending order they were always created under.
func (sp SessionSort) cursorMatches(cur SessionCursor, desc bool) bool {
	if cur.Sort == "" {
		return sp.key == defaultSortKey && desc
	}
	return cur.Sort == sp.key && cur.Desc == desc
}

func sentinelLiteral(kind valueKind, desc bool) string {
	switch kind {
	case kindReal:
		if desc {
			return sentinelRealDescSQL
		}
		return sentinelRealAscSQL
	default:
		if desc {
			return sentinelIntDescSQL
		}
		return sentinelIntAscSQL
	}
}

// sentinelGoString returns the cursor-encoded form of the sentinel, numerically
// equal to sentinelLiteral so a NULL row's cursor compares equal to a NULL row's
// COALESCE result on the next page.
func sentinelGoString(kind valueKind, desc bool) string {
	switch kind {
	case kindReal:
		if desc {
			return strconv.FormatFloat(-1e18, 'g', -1, 64)
		}
		return strconv.FormatFloat(1e18, 'g', -1, 64)
	default:
		if desc {
			return strconv.FormatInt(math.MinInt64, 10)
		}
		return strconv.FormatInt(math.MaxInt64, 10)
	}
}

// typedCursorValue parses a cursor's string value into the Go type the keyset
// comparison must bind for this kind. A malformed value is reported so callers
// can surface ErrInvalidCursor rather than a query error.
func typedCursorValue(value string, kind valueKind) (any, error) {
	switch kind {
	case kindInt:
		return strconv.ParseInt(value, 10, 64)
	case kindReal:
		return strconv.ParseFloat(value, 64)
	default:
		return value, nil
	}
}

func tsValue(s *Session) (string, bool) {
	v := s.CreatedAt
	if s.StartedAt != nil && *s.StartedAt != "" {
		v = *s.StartedAt
	}
	if s.EndedAt != nil && *s.EndedAt != "" {
		v = *s.EndedAt
	}
	return v, true
}

func startedValue(s *Session) (string, bool) {
	if s.StartedAt != nil && *s.StartedAt != "" {
		return *s.StartedAt, true
	}
	return s.CreatedAt, true
}

func intValue(get func(*Session) int) func(*Session) (string, bool) {
	return func(s *Session) (string, bool) {
		return strconv.Itoa(get(s)), true
	}
}

func recentExpr(d QueryDialect) string {
	return "COALESCE(" + d.timestampExpr("ended_at") + ", " +
		d.timestampExpr("started_at") + ", created_at)"
}

func startedExpr(d QueryDialect) string {
	return "COALESCE(" + d.timestampExpr("started_at") + ", created_at)"
}

func plainExpr(col string) func(QueryDialect) string {
	return func(QueryDialect) string { return col }
}

// sessionSorts is the allow-list of session-list sort keys, in display order.
// The ordering is kept in sync with the huma `order_by` enum tag by
// TestSortKeysMatchHumaEnum.
var sessionSorts = []SessionSort{
	{key: "recent", kind: kindTimestamp, defaultDescending: true, baseExpr: recentExpr, value: tsValue},
	{key: "started", kind: kindTimestamp, baseExpr: startedExpr, value: startedValue},
	{key: "messages", kind: kindInt, baseExpr: plainExpr("message_count"), value: intValue(func(s *Session) int { return s.MessageCount })},
	{key: "user-messages", kind: kindInt, baseExpr: plainExpr("user_message_count"), value: intValue(func(s *Session) int { return s.UserMessageCount })},
	{key: "output-tokens", kind: kindInt, baseExpr: plainExpr("total_output_tokens"), value: intValue(func(s *Session) int { return s.TotalOutputTokens })},
	{key: "peak-context", kind: kindInt, baseExpr: plainExpr("peak_context_tokens"), value: intValue(func(s *Session) int { return s.PeakContextTokens })},
	{key: "failures", kind: kindInt, baseExpr: plainExpr("tool_failure_signal_count"), value: intValue(func(s *Session) int { return s.ToolFailureSignalCount })},
	{key: "retries", kind: kindInt, baseExpr: plainExpr("tool_retry_count"), value: intValue(func(s *Session) int { return s.ToolRetryCount })},
	{key: "edit-churn", kind: kindInt, baseExpr: plainExpr("edit_churn_count"), value: intValue(func(s *Session) int { return s.EditChurnCount })},
	{key: "compactions", kind: kindInt, baseExpr: plainExpr("compaction_count"), value: intValue(func(s *Session) int { return s.CompactionCount })},
	{key: "context-pressure", kind: kindReal, nullable: true, baseExpr: plainExpr("context_pressure_max"), value: func(s *Session) (string, bool) {
		if s.ContextPressureMax == nil {
			return "", false
		}
		return strconv.FormatFloat(*s.ContextPressureMax, 'g', -1, 64), true
	}},
	{key: "health", kind: kindInt, nullable: true, baseExpr: plainExpr("health_score"), value: func(s *Session) (string, bool) {
		if s.HealthScore == nil {
			return "", false
		}
		return strconv.Itoa(*s.HealthScore), true
	}},
	{key: "secrets", kind: kindInt, baseExpr: plainExpr("secret_leak_count"), value: intValue(func(s *Session) int { return s.SecretLeakCount })},
	{key: "id", kind: kindText, baseExpr: plainExpr("id"), value: func(s *Session) (string, bool) { return s.ID, true }},
}

var sessionSortByKey = func() map[string]SessionSort {
	m := make(map[string]SessionSort, len(sessionSorts))
	for _, s := range sessionSorts {
		m[s.key] = s
	}
	return m
}()

// defaultSortKey is the sort applied when OrderBy is empty; it preserves the
// historical most-recent-activity-first behavior.
const defaultSortKey = "recent"

// SessionSortFor resolves a sort key (empty means the default). ok is false for
// unknown keys; callers that have not pre-validated should treat that as the
// default rather than erroring, so the store never panics on bad input.
func SessionSortFor(key string) (SessionSort, bool) {
	if key == "" {
		return sessionSortByKey[defaultSortKey], true
	}
	sp, ok := sessionSortByKey[key]
	if !ok {
		return sessionSortByKey[defaultSortKey], false
	}
	return sp, true
}

// ValidSortKey reports whether key is an accepted session-list sort (empty is
// accepted and means the default).
func ValidSortKey(key string) bool {
	if key == "" {
		return true
	}
	_, ok := sessionSortByKey[key]
	return ok
}

// SortDefaultDescending returns the canonical direction for a sort key, used by
// the CLI to translate --reverse (flip) into an absolute Descending value.
func SortDefaultDescending(key string) bool {
	sp, _ := SessionSortFor(key)
	return sp.defaultDescending
}

// SortKeys returns the accepted sort keys in display order.
func SortKeys() []string {
	keys := make([]string, len(sessionSorts))
	for i, s := range sessionSorts {
		keys[i] = s.key
	}
	return keys
}
