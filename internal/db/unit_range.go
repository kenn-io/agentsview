package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// UnitAnchor classifies one match's anchor message row.
type UnitAnchor struct {
	SessionID  string
	Ordinal    int
	Role       string // "user"/"assistant"/other; "" when Missing
	Sidechain  bool
	Embeddable bool // is_system = 0 AND content not system-prefixed
	Missing    bool // anchor row absent (tool_result_events orphan)
}

// UnitProbe asks for the nearest embeddable-user boundaries around Ordinal.
type UnitProbe struct {
	SessionID string
	Ordinal   int
}

// UnitBounds carries exclusive user boundaries; sentinel values when absent:
// Prev = -1, Next = unitOrdinalMax.
type UnitBounds struct{ Prev, Next int }

// ExtentProbe asks for the first/last member ordinals of the anchor's
// same-sidechain run within the exclusive interval (Lo, Hi).
type ExtentProbe struct {
	SessionID string
	Ordinal   int
	Lo, Hi    int // sentinels as above
	Sidechain bool
}

// unitOrdinalMax bounds Hi sentinels; ordinals are int32-safe on all
// backends (PG INTEGER).
const unitOrdinalMax = 1<<31 - 1

// UnitBoundsQuerier is the backend seam. Both methods are BATCHED: one or
// two SQL statements per call, probe lists chunked to the dialect's
// variable limit. Results align 1:1 with probes.
type UnitBoundsQuerier interface {
	NearestUserBoundaries(ctx context.Context, probes []UnitProbe) ([]UnitBounds, error)
	RunExtents(ctx context.Context, probes []ExtentProbe) ([][2]int, error)
}

// SubordinateSession reports the session-level subordinate classification
// (subagent/fork-typed, or parent-linked non-continuation). Exported
// wrapper over the existing isSubordinateSession logic so the PG and
// DuckDB packages compute the same subordinate flag.
func SubordinateSession(relationshipType, parentSessionID string) bool {
	return isSubordinateSession(relationshipType, sql.NullString{
		String: parentSessionID, Valid: parentSessionID != "",
	})
}

// DeriveUnitRanges applies the spec's rules 1-3 + missing-anchor fallback.
// Result aligns 1:1 with anchors.
//
// Rule-1 (embeddable user), rule-3 (system rows, other roles, non-embeddable
// rows), and missing anchors resolve to [o, o] with no queries. Embeddable
// assistant anchors resolve with ONE batched NearestUserBoundaries call and
// ONE batched RunExtents call covering every pending anchor up front —
// duplicate (session, ordinal, sidechain) anchors share a single probe — so
// a page costs one call per seam method no matter how its anchors spread
// across sessions and runs.
func DeriveUnitRanges(
	ctx context.Context, q UnitBoundsQuerier, anchors []UnitAnchor,
) ([][2]int, error) {
	out := make([][2]int, len(anchors))
	pending := classifyUnitAnchors(anchors, out)
	if len(pending) == 0 {
		return out, nil
	}
	keys, keyIdx := dedupUnitProbeKeys(anchors, pending)
	extents, err := deriveProbeExtents(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	for _, i := range pending {
		out[i] = extents[keyIdx[unitProbeKeyOf(anchors[i])]]
	}
	return out, nil
}

// unitProbeKey identifies one distinct rule-2 probe. Sidechain is part of the
// key defensively: anchors at the same ordinal always describe the same
// message row, but a mismatched flag must not silently share a result.
type unitProbeKey struct {
	sessionID string
	ordinal   int
	sidechain bool
}

func unitProbeKeyOf(a UnitAnchor) unitProbeKey {
	return unitProbeKey{
		sessionID: a.SessionID, ordinal: a.Ordinal, sidechain: a.Sidechain,
	}
}

// runDerivable reports whether an anchor needs rule-2 run derivation: an
// embeddable assistant row that was actually found.
func runDerivable(a UnitAnchor) bool {
	return !a.Missing && a.Embeddable && a.Role == "assistant"
}

// classifyUnitAnchors fills [o, o] for every anchor that resolves locally
// (rules 1/3 and missing anchors) and returns the indexes of the anchors
// that need run derivation.
func classifyUnitAnchors(anchors []UnitAnchor, out [][2]int) []int {
	var pending []int
	for i, a := range anchors {
		if runDerivable(a) {
			pending = append(pending, i)
			continue
		}
		out[i] = [2]int{a.Ordinal, a.Ordinal}
	}
	return pending
}

// dedupUnitProbeKeys collects the distinct probe keys of the pending anchors
// in first-seen order and returns them with a key -> slot lookup.
func dedupUnitProbeKeys(
	anchors []UnitAnchor, pending []int,
) ([]unitProbeKey, map[unitProbeKey]int) {
	keys := make([]unitProbeKey, 0, len(pending))
	keyIdx := make(map[unitProbeKey]int, len(pending))
	for _, i := range pending {
		k := unitProbeKeyOf(anchors[i])
		if _, ok := keyIdx[k]; ok {
			continue
		}
		keyIdx[k] = len(keys)
		keys = append(keys, k)
	}
	return keys, keyIdx
}

// deriveProbeExtents runs one batched NearestUserBoundaries call and one
// batched RunExtents call for the distinct probe keys, returning run extents
// aligned 1:1 with keys. Every extent must cover its own anchor ordinal (the
// anchor row qualifies for its run by construction).
func deriveProbeExtents(
	ctx context.Context, q UnitBoundsQuerier, keys []unitProbeKey,
) ([][2]int, error) {
	probes := make([]UnitProbe, len(keys))
	for i, k := range keys {
		probes[i] = UnitProbe{SessionID: k.sessionID, Ordinal: k.ordinal}
	}
	bounds, err := q.NearestUserBoundaries(ctx, probes)
	if err != nil {
		return nil, err
	}
	if len(bounds) != len(keys) {
		return nil, fmt.Errorf(
			"deriving unit ranges: NearestUserBoundaries returned %d results for %d probes",
			len(bounds), len(keys))
	}
	extentProbes := make([]ExtentProbe, len(keys))
	for i, k := range keys {
		extentProbes[i] = ExtentProbe{
			SessionID: k.sessionID, Ordinal: k.ordinal,
			Lo: bounds[i].Prev, Hi: bounds[i].Next,
			Sidechain: k.sidechain,
		}
	}
	extents, err := q.RunExtents(ctx, extentProbes)
	if err != nil {
		return nil, err
	}
	if len(extents) != len(keys) {
		return nil, fmt.Errorf(
			"deriving unit ranges: RunExtents returned %d results for %d probes",
			len(extents), len(keys))
	}
	for i, k := range keys {
		if extents[i][0] > k.ordinal || extents[i][1] < k.ordinal {
			return nil, fmt.Errorf(
				"deriving unit ranges: run extent [%d, %d] does not cover anchor %s#%d",
				extents[i][0], extents[i][1], k.sessionID, k.ordinal)
		}
	}
	return extents, nil
}

// SQLite seam implementation.
var _ UnitBoundsQuerier = (*DB)(nil)

// unitSpanChunk caps spans per statement so the VALUES CTE stays inside
// SQLite's bind-variable limit (see maxSQLVars): a span binds 4 variables
// (idx, session_id, lo, hi).
const unitSpanChunk = maxSQLVars / 4

// embeddableUserSQL is the SQL predicate matching an embeddable user row
// under the messages alias: user role, is_system = 0, and the SQLite dialect
// SystemPrefixSQL check, exactly as ScanEmbeddableUnits' scan predicate.
// (The assistant-side member predicate in scanRunMemberSpans skips the
// prefix check: SystemPrefixSQL constrains user rows only.)
func embeddableUserSQL(alias string) string {
	return fmt.Sprintf("%[1]s.role = 'user' AND %[1]s.is_system = 0 AND %[2]s",
		alias, SystemPrefixSQL(alias+".content", alias+".role"))
}

// userBoundarySpan accumulates one session's boundary data: the embeddable
// user ordinals inside [lo, hi] (the session's probe-ordinal span), plus the
// nearest embeddable user ordinal below lo and above hi (COALESCEd to the
// -1 / unitOrdinalMax sentinels in SQL).
type userBoundarySpan struct {
	sessionID    string
	lo, hi       int
	inner        []int
	below, above int
	belowSeen    bool
	aboveSeen    bool
}

// NearestUserBoundaries returns, per probe, the nearest embeddable user
// ordinals strictly before and after the probe ordinal (sidechain is
// irrelevant: the reducer closes runs on any embeddable user row regardless
// of its is_sidechain). Sentinels -1 / unitOrdinalMax stand in for missing
// boundaries.
//
// Probes are grouped into one span per session covering [min, max] probe
// ordinal: one statement per unitSpanChunk spans scans each span's stretch
// ONCE for embeddable user rows (plus one below-lo and one above-hi point
// lookup per span), and every probe's boundaries resolve from the collected
// ordinals in Go. Probes deep in the same user-free stretch therefore share
// a single scan instead of each rescanning it.
func (db *DB) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	out := make([]UnitBounds, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	spanIdx := make(map[string]int)
	spans := make([]userBoundarySpan, 0, len(probes))
	for _, p := range probes {
		i, ok := spanIdx[p.SessionID]
		if !ok {
			spanIdx[p.SessionID] = len(spans)
			spans = append(spans, userBoundarySpan{
				sessionID: p.SessionID, lo: p.Ordinal, hi: p.Ordinal,
			})
			continue
		}
		spans[i].lo = min(spans[i].lo, p.Ordinal)
		spans[i].hi = max(spans[i].hi, p.Ordinal)
	}
	for start := 0; start < len(spans); start += unitSpanChunk {
		chunk := spans[start:min(start+unitSpanChunk, len(spans))]
		if err := db.scanUserBoundarySpans(ctx, chunk); err != nil {
			return nil, err
		}
	}
	for i := range spans {
		if !spans[i].belowSeen || !spans[i].aboveSeen {
			return nil, fmt.Errorf(
				"nearest user boundaries: missing sentinel rows for session %s",
				spans[i].sessionID)
		}
		sort.Ints(spans[i].inner)
	}
	for i, p := range probes {
		out[i] = spans[spanIdx[p.SessionID]].boundsAround(p.Ordinal)
	}
	return out, nil
}

// boundsAround resolves one probe's exclusive boundaries from the span's
// sorted inner ordinals and its below/above lookups, mirroring the strict
// MAX(< ordinal) / MIN(> ordinal) semantics of the SQL form.
func (sp *userBoundarySpan) boundsAround(ordinal int) UnitBounds {
	b := UnitBounds{Prev: sp.below, Next: sp.above}
	i := sort.SearchInts(sp.inner, ordinal)
	if i > 0 {
		b.Prev = sp.inner[i-1]
	}
	for ; i < len(sp.inner); i++ {
		if sp.inner[i] > ordinal {
			b.Next = sp.inner[i]
			break
		}
	}
	return b
}

// scanUserBoundarySpans runs the one batched statement for a chunk of spans:
// a VALUES CTE joined against messages for the in-span embeddable user rows
// (second column NULL), plus one sentinel row per span carrying the
// correlated point lookups below lo and above hi (COALESCEd to the -1 /
// unitOrdinalMax sentinels when absent).
func (db *DB) scanUserBoundarySpans(
	ctx context.Context, spans []userBoundarySpan,
) error {
	values := make([]string, len(spans))
	args := make([]any, 0, len(spans)*4)
	for i := range spans {
		values[i] = "(?, ?, ?, ?)"
		args = append(args, i, spans[i].sessionID, spans[i].lo, spans[i].hi)
	}
	user := embeddableUserSQL("m")
	query := fmt.Sprintf(`
		WITH spans(idx, session_id, lo, hi) AS (VALUES %s)
		SELECT sp.idx, m.ordinal, NULL
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		  AND m.ordinal >= sp.lo AND m.ordinal <= sp.hi
		WHERE %s
		UNION ALL
		SELECT sp.idx,
		  COALESCE((SELECT MAX(m.ordinal) FROM messages m
		    WHERE m.session_id = sp.session_id AND m.ordinal < sp.lo AND %s), -1),
		  COALESCE((SELECT MIN(m.ordinal) FROM messages m
		    WHERE m.session_id = sp.session_id AND m.ordinal > sp.hi AND %s), %d)
		FROM spans sp`,
		strings.Join(values, ", "), user, user, user, unitOrdinalMax)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying nearest user boundaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx, ordinal int
		var above sql.NullInt64
		if err := rows.Scan(&idx, &ordinal, &above); err != nil {
			return fmt.Errorf("scanning nearest user boundaries: %w", err)
		}
		if idx < 0 || idx >= len(spans) {
			return fmt.Errorf(
				"nearest user boundaries: span index %d out of range [0, %d)",
				idx, len(spans))
		}
		if !above.Valid {
			spans[idx].inner = append(spans[idx].inner, ordinal)
			continue
		}
		spans[idx].below, spans[idx].belowSeen = ordinal, true
		spans[idx].above, spans[idx].aboveSeen = int(above.Int64), true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating nearest user boundaries: %w", err)
	}
	return nil
}

// extentSpanKey groups extent probes that share one exclusive interval, so
// the interval's member rows are fetched once no matter how many anchors
// fall inside it.
type extentSpanKey struct {
	sessionID string
	lo, hi    int
}

// runMemberRow is one embeddable assistant row inside an extent interval.
type runMemberRow struct {
	ordinal   int
	sidechain bool
}

// RunExtents returns, per probe, the first and last member ordinals of the
// anchor's same-sidechain run: the nearest embeddable assistant rows of the
// anchor's sidechain around the anchor, bounded exclusively by (Lo, Hi) and
// by the nearest embeddable assistant row of the opposite sidechain inside
// that interval (the reducer's flip rule; flips only matter among embeddable
// assistant rows). The anchor row itself always qualifies, so a probe whose
// interval holds no run around its anchor was built for a row that is not an
// embeddable assistant row — an internal invariant violation reported as an
// error.
//
// Probes sharing an interval are grouped: one statement per unitSpanChunk
// intervals fetches each interval's embeddable assistant rows ONCE (ordinal
// plus sidechain flag), and every probe's extent — flip bounds and first/last
// member — resolves from the fetched rows in Go.
func (db *DB) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	out := make([][2]int, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	spanIdx := make(map[extentSpanKey]int)
	spans := make([]extentSpanKey, 0, len(probes))
	for _, p := range probes {
		k := extentSpanKey{sessionID: p.SessionID, lo: p.Lo, hi: p.Hi}
		if _, ok := spanIdx[k]; ok {
			continue
		}
		spanIdx[k] = len(spans)
		spans = append(spans, k)
	}
	members := make([][]runMemberRow, len(spans))
	for start := 0; start < len(spans); start += unitSpanChunk {
		chunk := spans[start:min(start+unitSpanChunk, len(spans))]
		if err := db.scanRunMemberSpans(ctx, chunk, members[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for _, m := range members {
		sort.Slice(m, func(a, b int) bool { return m[a].ordinal < m[b].ordinal })
	}
	for i, p := range probes {
		ext, err := runExtentWithin(members[spanIdx[extentSpanKey{
			sessionID: p.SessionID, lo: p.Lo, hi: p.Hi,
		}]], p)
		if err != nil {
			return nil, err
		}
		out[i] = ext
	}
	return out, nil
}

// runExtentWithin walks the interval's sorted member rows around the probe
// anchor: descending below the anchor and ascending above it, collecting the
// farthest same-sidechain member on each side and stopping at the first
// opposite-sidechain member (the flip that closes the run). No member at the
// anchor's side means the probe was not built for a rule-2 anchor.
func runExtentWithin(members []runMemberRow, p ExtentProbe) ([2]int, error) {
	first, firstOK := 0, false
	up := sort.Search(len(members), func(k int) bool {
		return members[k].ordinal > p.Ordinal
	})
	for k := up - 1; k >= 0; k-- {
		if members[k].sidechain != p.Sidechain {
			break
		}
		first, firstOK = members[k].ordinal, true
	}
	last, lastOK := 0, false
	lo := sort.Search(len(members), func(k int) bool {
		return members[k].ordinal >= p.Ordinal
	})
	for k := lo; k < len(members); k++ {
		if members[k].sidechain != p.Sidechain {
			break
		}
		last, lastOK = members[k].ordinal, true
	}
	if !firstOK || !lastOK {
		return [2]int{}, fmt.Errorf(
			"run extents: anchor %s#%d is not an embeddable assistant row "+
				"(probe must only be built for rule-2 anchors)",
			p.SessionID, p.Ordinal)
	}
	return [2]int{first, last}, nil
}

// scanRunMemberSpans runs the one batched statement for a chunk of extent
// intervals: a VALUES CTE joined against messages for each interval's
// embeddable assistant rows, exclusive of the (lo, hi) bounds. The member
// predicate is role + is_system only: SystemPrefixSQL constrains user rows
// exclusively, so it is identically TRUE for assistant rows and deliberately
// omitted here.
func (db *DB) scanRunMemberSpans(
	ctx context.Context, spans []extentSpanKey, out [][]runMemberRow,
) error {
	values := make([]string, len(spans))
	args := make([]any, 0, len(spans)*4)
	for i, sp := range spans {
		values[i] = "(?, ?, ?, ?)"
		args = append(args, i, sp.sessionID, sp.lo, sp.hi)
	}
	query := fmt.Sprintf(`
		WITH spans(idx, session_id, lo, hi) AS (VALUES %s)
		SELECT sp.idx, m.ordinal, m.is_sidechain
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		  AND m.ordinal > sp.lo AND m.ordinal < sp.hi
		WHERE m.role = 'assistant' AND m.is_system = 0`,
		strings.Join(values, ", "))

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying run extents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var m runMemberRow
		if err := rows.Scan(&idx, &m.ordinal, &m.sidechain); err != nil {
			return fmt.Errorf("scanning run extents: %w", err)
		}
		if idx < 0 || idx >= len(out) {
			return fmt.Errorf("run extents: span index %d out of range [0, %d)",
				idx, len(out))
		}
		out[idx] = append(out[idx], m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating run extents: %w", err)
	}
	return nil
}
