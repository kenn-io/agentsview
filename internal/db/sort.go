// ABOUTME: session-list sort registry — maps the public --sort/?order_by keys
// ABOUTME: to dialect-aware ORDER BY expressions and keyset cursor values.
package db

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
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
	// expr renders the (possibly nullable) SQL expression for this sort. It
	// receives the builder so the secrets sort can add bind parameters for its
	// version-aware CASE, and the filter so it can mirror the active scanner
	// rule versions. Timestamp sorts use the dialect's empty-string-aware
	// wrapping; most sorts are a plain column name shared across backends.
	expr func(b *QueryBuilder, f SessionFilter) string
	// value returns the row's sort value formatted as the cursor stores it, plus
	// ok=false when the underlying column is NULL. It receives the filter so the
	// secrets sort encodes the same version-gated value it sorts by.
	value func(s *Session, f SessionFilter) (string, bool)
}

// orderExpr renders the non-null SQL expression used in both ORDER BY and the
// keyset cursor predicate for the resolved direction. It may add bind
// parameters to b, so callers must render it at its textual position.
func (sp SessionSort) orderExpr(b *QueryBuilder, desc bool, f SessionFilter) string {
	e := sp.expr(b, f)
	if sp.nullable {
		e = "COALESCE(" + e + ", " + sentinelLiteral(sp.kind, desc) + ")"
	}
	return e
}

// cursorValue returns the string-encoded comparison value for a page's last
// row, substituting the matching sentinel when the column is NULL.
func (sp SessionSort) cursorValue(s *Session, desc bool, f SessionFilter) string {
	if v, ok := sp.value(s, f); ok {
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

// SortKey is one ordered sort term for a SessionFilter: a registry key with an
// optional direction override. A nil Descending means the key's canonical
// default direction applies (recent is descending; every other key ascending).
type SortKey struct {
	Key        string
	Descending *bool
}

// ResolvedSort pairs a registered SessionSort with the concrete direction
// resolved for one term, after applying any per-key override or fallback.
type ResolvedSort struct {
	Sort SessionSort
	Desc bool
}

// ParseSortSpec parses the public sort specification used by --sort and
// ?order_by: a comma-separated list of terms, each "key" or "key:asc"/"key:desc".
// An empty spec yields no terms (callers apply the default). Unknown keys,
// unknown direction tokens, duplicate keys, and empty terms are reported as
// errors so the service layer can reject bad input with a clear message.
func ParseSortSpec(spec string) ([]SortKey, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	keys := make([]SortKey, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty sort term")
		}
		key := part
		var dir *bool
		if before, after, ok := strings.Cut(part, ":"); ok {
			key = strings.TrimSpace(before)
			switch strings.TrimSpace(after) {
			case "asc":
				d := false
				dir = &d
			case "desc":
				d := true
				dir = &d
			default:
				return nil, fmt.Errorf(
					"invalid sort direction in %q: want asc or desc", part)
			}
		}
		if _, ok := sessionSortByKey[key]; !ok {
			return nil, fmt.Errorf("unknown sort key %q", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate sort key %q", key)
		}
		seen[key] = true
		keys = append(keys, SortKey{Key: key, Descending: dir})
	}
	return keys, nil
}

// FormatSortSpec renders sort terms back into the canonical --sort/?order_by
// string. A term with an explicit direction renders "key:asc"/"key:desc"; a term
// left at its canonical default renders the bare key.
func FormatSortSpec(keys []SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		switch {
		case k.Descending == nil:
			parts[i] = k.Key
		case *k.Descending:
			parts[i] = k.Key + ":desc"
		default:
			parts[i] = k.Key + ":asc"
		}
	}
	return strings.Join(parts, ",")
}

// ApplyFallbackDirection fills in the direction of any term left unspecified
// (the legacy descending param / single-key default). Terms that carry an
// explicit per-key direction are untouched. A nil fallback leaves every
// unspecified term at its canonical default.
func ApplyFallbackDirection(keys []SortKey, descending *bool) []SortKey {
	if descending == nil {
		return keys
	}
	out := make([]SortKey, len(keys))
	for i, k := range keys {
		if k.Descending == nil {
			d := *descending
			k.Descending = &d
		}
		out[i] = k
	}
	return out
}

// ResolveSort turns a filter's sort specification into the ordered list of
// columns the query is sorted by. The structured Sort field wins; otherwise the
// legacy OrderBy string (folded with Descending) is parsed. Unknown or duplicate
// keys are dropped defensively so a store never panics on unvalidated input, and
// an empty result falls back to the default recent sort. Callers that must
// reject bad input (the service layer) should validate via ParseSortSpec first.
func ResolveSort(f SessionFilter) []ResolvedSort {
	terms := f.Sort
	if len(terms) == 0 {
		parsed, err := ParseSortSpec(f.OrderBy)
		if err != nil {
			parsed = nil
		}
		terms = ApplyFallbackDirection(parsed, f.Descending)
	}
	rs := make([]ResolvedSort, 0, len(terms))
	seen := make(map[string]bool, len(terms))
	for _, t := range terms {
		sp, ok := sessionSortByKey[t.Key]
		if !ok || seen[sp.key] {
			continue
		}
		seen[sp.key] = true
		rs = append(rs, ResolvedSort{Sort: sp, Desc: sp.ResolveDescending(t.Descending)})
	}
	if len(rs) == 0 {
		sp := sessionSortByKey[defaultSortKey]
		desc := sp.defaultDescending
		// Preserve the single-key behavior where a legacy Descending override
		// applies to the implicit default recent key (e.g. descending=false with
		// no order_by means recent ascending).
		if len(f.Sort) == 0 && f.Descending != nil {
			desc = *f.Descending
		}
		rs = append(rs, ResolvedSort{Sort: sp, Desc: desc})
	}
	return rs
}

// appendIDTiebreaker ensures the ordered columns end with the unique id column
// so keyset pagination is deterministic. When id is already an explicit sort
// term the list is returned unchanged; otherwise id is appended in the last
// term's direction (the tie-breaker behavior id has always had).
func appendIDTiebreaker(rs []ResolvedSort) []ResolvedSort {
	for _, r := range rs {
		if r.Sort.key == "id" {
			return rs
		}
	}
	out := make([]ResolvedSort, len(rs), len(rs)+1)
	copy(out, rs)
	return append(out, ResolvedSort{Sort: sessionSortByKey["id"], Desc: rs[len(rs)-1].Desc})
}

// NextSessionCursor builds the pagination token for a page's last row under the
// resolved multi-key sort. Each sort term contributes a typed keyset value. For
// a single-key sort the legacy Sort/Desc/Value fields (and EndedAt for the
// default recent sort) are also populated so cursors stay decodable by readers
// from before multi-key sorting existed.
func NextSessionCursor(last *Session, rs []ResolvedSort, total int, f SessionFilter) SessionCursor {
	cur := SessionCursor{ID: last.ID, Total: total}
	cur.Keys = make([]SessionCursorKey, len(rs))
	for i, r := range rs {
		cur.Keys[i] = SessionCursorKey{
			Sort:  r.Sort.key,
			Desc:  r.Desc,
			Value: r.Sort.cursorValue(last, r.Desc, f),
		}
	}
	if len(rs) == 1 {
		k := cur.Keys[0]
		cur.Sort = k.Sort
		cur.Desc = k.Desc
		cur.Value = k.Value
		if k.Sort == defaultSortKey && k.Desc {
			cur.EndedAt = k.Value
		}
	}
	return cur
}

// CursorPredicateValues validates a decoded cursor against the resolved sort and
// returns the typed keyset values, one per term, that the predicate must bind. A
// cursor minted under a different sort (different keys, order, or directions), or
// carrying an unparseable value, is rejected as an invalid cursor rather than
// silently producing wrong pages.
func CursorPredicateValues(cur SessionCursor, rs []ResolvedSort) ([]any, error) {
	keys := cur.resolvedKeys()
	if len(keys) != len(rs) {
		return nil, fmt.Errorf("%w: sort mismatch", ErrInvalidCursor)
	}
	vals := make([]any, len(rs))
	for i, r := range rs {
		if keys[i].Sort != r.Sort.key || keys[i].Desc != r.Desc {
			return nil, fmt.Errorf("%w: sort mismatch", ErrInvalidCursor)
		}
		v, err := typedCursorValue(keys[i].Value, r.Sort.kind)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		vals[i] = v
	}
	return vals, nil
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

func tsValue(s *Session, _ SessionFilter) (string, bool) {
	v := s.CreatedAt
	if s.StartedAt != nil && *s.StartedAt != "" {
		v = *s.StartedAt
	}
	if s.EndedAt != nil && *s.EndedAt != "" {
		v = *s.EndedAt
	}
	return v, true
}

func startedValue(s *Session, _ SessionFilter) (string, bool) {
	if s.StartedAt != nil && *s.StartedAt != "" {
		return *s.StartedAt, true
	}
	return s.CreatedAt, true
}

func intValue(get func(*Session) int) func(*Session, SessionFilter) (string, bool) {
	return func(s *Session, _ SessionFilter) (string, bool) {
		return strconv.Itoa(get(s)), true
	}
}

func recentExpr(b *QueryBuilder, _ SessionFilter) string {
	return "COALESCE(" + b.dialect.timestampExpr("ended_at") + ", " +
		b.dialect.timestampExpr("started_at") + ", created_at)"
}

func startedExpr(b *QueryBuilder, _ SessionFilter) string {
	return "COALESCE(" + b.dialect.timestampExpr("started_at") + ", created_at)"
}

func plainExpr(col string) func(*QueryBuilder, SessionFilter) string {
	return func(*QueryBuilder, SessionFilter) string { return col }
}

// secretsExpr orders by the same secret count the service layer displays: when
// active scanner rule versions are known, counts from stale versions are gated
// to 0 (matching hideStaleSecretCount), so sessions shown with 0 secrets do not
// rank above sessions with current findings. With no versions (raw db callers),
// it falls back to the raw column, mirroring the HasSecret filter's convention.
func secretsExpr(b *QueryBuilder, f SessionFilter) string {
	versions := nonEmpty(f.SecretsRulesVersions)
	if len(versions) == 0 {
		return "secret_leak_count"
	}
	return "CASE WHEN " + inPredicate("secrets_rules_version", versions, b) +
		" THEN secret_leak_count ELSE 0 END"
}

func secretsValue(s *Session, f SessionFilter) (string, bool) {
	n := s.SecretLeakCount
	versions := nonEmpty(f.SecretsRulesVersions)
	if len(versions) > 0 && !slices.Contains(versions, s.SecretsRulesVersion) {
		n = 0
	}
	return strconv.Itoa(n), true
}

// sessionSorts is the allow-list of session-list sort keys, in display order.
// The keys here are kept documented on the huma order_by param by
// TestSortKeysDocumented / TestSortKeysDocOmitsStaleKeys (internal/server).
var sessionSorts = []SessionSort{
	{key: "recent", kind: kindTimestamp, defaultDescending: true, expr: recentExpr, value: tsValue},
	{key: "started", kind: kindTimestamp, expr: startedExpr, value: startedValue},
	{key: "messages", kind: kindInt, expr: plainExpr("message_count"), value: intValue(func(s *Session) int { return s.MessageCount })},
	{key: "user-messages", kind: kindInt, expr: plainExpr("user_message_count"), value: intValue(func(s *Session) int { return s.UserMessageCount })},
	{key: "output-tokens", kind: kindInt, expr: plainExpr("total_output_tokens"), value: intValue(func(s *Session) int { return s.TotalOutputTokens })},
	{key: "peak-context", kind: kindInt, expr: plainExpr("peak_context_tokens"), value: intValue(func(s *Session) int { return s.PeakContextTokens })},
	{key: "failures", kind: kindInt, expr: plainExpr("tool_failure_signal_count"), value: intValue(func(s *Session) int { return s.ToolFailureSignalCount })},
	{key: "retries", kind: kindInt, expr: plainExpr("tool_retry_count"), value: intValue(func(s *Session) int { return s.ToolRetryCount })},
	{key: "edit-churn", kind: kindInt, expr: plainExpr("edit_churn_count"), value: intValue(func(s *Session) int { return s.EditChurnCount })},
	{key: "compactions", kind: kindInt, expr: plainExpr("compaction_count"), value: intValue(func(s *Session) int { return s.CompactionCount })},
	{key: "context-pressure", kind: kindReal, nullable: true, expr: plainExpr("context_pressure_max"), value: func(s *Session, _ SessionFilter) (string, bool) {
		if s.ContextPressureMax == nil {
			return "", false
		}
		return strconv.FormatFloat(*s.ContextPressureMax, 'g', -1, 64), true
	}},
	{key: "health", kind: kindInt, nullable: true, expr: plainExpr("health_score"), value: func(s *Session, _ SessionFilter) (string, bool) {
		if s.HealthScore == nil {
			return "", false
		}
		return strconv.Itoa(*s.HealthScore), true
	}},
	{key: "secrets", kind: kindInt, expr: secretsExpr, value: secretsValue},
	{key: "id", kind: kindText, expr: plainExpr("id"), value: func(s *Session, _ SessionFilter) (string, bool) { return s.ID, true }},
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

// DefaultSortKey returns the sort key applied when no sort is requested. The CLI
// uses it to materialize the implicit default so --reverse can flip it even when
// --sort is empty.
func DefaultSortKey() string { return defaultSortKey }

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
