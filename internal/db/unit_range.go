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
// Prev = -1, Next = UnitOrdinalMax.
type UnitBounds struct{ Prev, Next int }

// ExtentProbe asks for the first/last member ordinals of the anchor's
// same-sidechain run within the exclusive interval (Lo, Hi).
type ExtentProbe struct {
	SessionID string
	Ordinal   int
	Lo, Hi    int // sentinels as above
	Sidechain bool
}

// UnitOrdinalMax bounds Hi sentinels; ordinals are int32-safe on all
// backends (PG INTEGER). Exported so every backend seam shares the exact
// sentinel value.
const UnitOrdinalMax = 1<<31 - 1

// UnitBoundsQuerier is the backend seam. Both methods are BATCHED:
// NearestUserBoundaries groups probes per session and RunExtents dedups
// probes, and each chunk of sessions or probes costs one SQL statement —
// never one statement, or query, per probe. Results align 1:1 with probes.
// RunExtents' stop set includes the unit boundaries NearestUserBoundaries
// reports, so DeriveUnitRanges consults NearestUserBoundaries only on
// session-dense pages where pre-fetched bounds pay for themselves (see
// UnitBoundsFlowFactor).
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
// assistant anchors resolve with at most TWO batched RunExtents calls
// covering every pending anchor — duplicate (session, ordinal, sidechain)
// anchors share a single probe, and anchors in the same run share one
// representative probe (see deriveProbeExtents) — so a page's query count
// stays constant no matter how its anchors spread across sessions and runs.
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

// UnitBoundsFlowFactor gates the optional NearestUserBoundaries round in
// deriveProbeExtents: real user bounds are fetched only when the page packs
// at least this many probes per distinct session on average. The boundary
// fetch costs one statement over every user row of each probed session, so
// it amortizes only on dense pages — where it pays twice, by pruning the
// stop scans and by splitting probe groups at unit boundaries so run sharing
// resolves the page in one round. Exported so backend test suites can seed
// pages that provably exercise the dense flow instead of hardcoding the
// threshold.
const UnitBoundsFlowFactor = 8

// deriveProbeExtents runs at most two batched RunExtents calls (plus one
// optional NearestUserBoundaries call on session-dense pages, see
// UnitBoundsFlowFactor) for the distinct probe keys, returning run extents
// aligned 1:1 with keys. Every extent must cover its own anchor ordinal (the
// anchor row qualifies for its run by construction). Without the boundary
// round, probes carry the -1 / UnitOrdinalMax sentinel bounds: RunExtents'
// stop set already includes embeddable user rows, so real bounds are an
// optimization, never a correctness requirement.
//
// The two RunExtents rounds share runs between anchors: probes with the same
// (session, bounds, sidechain) group key that land in the same run have
// identical extents, so round one queries one representative per group and
// hands its extent to every group sibling the extent covers — sound because
// a rule-2 anchor is itself a run member, and a same-sidechain member inside
// [first, last] belongs to that exact run (a stop row strictly inside the
// extent would have closed the run before it reached first or last). The
// representative is the group's ordinal MEDIAN: page anchors cluster in hot
// runs, so a central anchor's run covers the most siblings (a group edge
// anchor may sit in a small neighboring run and cover nobody). Siblings in
// other runs (a page whose anchors straddle a user boundary or a sidechain
// flip) resolve in one second batch, so a page never costs more than two
// RunExtents statements.
func deriveProbeExtents(
	ctx context.Context, q UnitBoundsQuerier, keys []unitProbeKey,
) ([][2]int, error) {
	extentProbes, err := buildExtentProbes(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	extents := make([][2]int, len(keys))
	resolved := make([]bool, len(keys))
	groups := groupExtentProbes(extentProbes)
	reps := make([]int, 0, len(groups))
	for _, g := range groups {
		reps = append(reps, g[len(g)/2])
	}
	sort.Ints(reps)
	if err := resolveExtentRound(ctx, q, extentProbes, reps, extents, resolved); err != nil {
		return nil, err
	}
	shareGroupExtents(groups, extentProbes, extents, resolved)
	var rest []int
	for i := range keys {
		if !resolved[i] {
			rest = append(rest, i)
		}
	}
	if len(rest) > 0 {
		if err := resolveExtentRound(ctx, q, extentProbes, rest, extents, resolved); err != nil {
			return nil, err
		}
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

// buildExtentProbes maps probe keys to RunExtents probes. Dense pages (at
// least UnitBoundsFlowFactor probes per distinct session) fetch the real
// exclusive user bounds with one batched NearestUserBoundaries call; sparse
// pages skip that round trip and probe with the -1 / UnitOrdinalMax
// sentinels, leaning on RunExtents' user-row stops instead.
func buildExtentProbes(
	ctx context.Context, q UnitBoundsQuerier, keys []unitProbeKey,
) ([]ExtentProbe, error) {
	sessions := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		sessions[k.sessionID] = struct{}{}
	}
	probes := make([]ExtentProbe, len(keys))
	for i, k := range keys {
		probes[i] = ExtentProbe{
			SessionID: k.sessionID, Ordinal: k.ordinal,
			Lo: -1, Hi: UnitOrdinalMax,
			Sidechain: k.sidechain,
		}
	}
	if len(keys) < UnitBoundsFlowFactor*len(sessions) {
		return probes, nil
	}
	boundProbes := make([]UnitProbe, len(keys))
	for i, k := range keys {
		boundProbes[i] = UnitProbe{SessionID: k.sessionID, Ordinal: k.ordinal}
	}
	bounds, err := q.NearestUserBoundaries(ctx, boundProbes)
	if err != nil {
		return nil, err
	}
	if len(bounds) != len(keys) {
		return nil, fmt.Errorf(
			"deriving unit ranges: NearestUserBoundaries returned %d results for %d probes",
			len(bounds), len(keys))
	}
	for i := range probes {
		probes[i].Lo, probes[i].Hi = bounds[i].Prev, bounds[i].Next
	}
	return probes, nil
}

// extentGroupKey identifies the probes that can share a run: same session,
// same exclusive bounds, same sidechain.
type extentGroupKey struct {
	sessionID string
	lo, hi    int
	sidechain bool
}

// groupExtentProbes buckets probe indexes by extentGroupKey, each bucket
// sorted by anchor ordinal (so a bucket's median element is its central
// anchor).
func groupExtentProbes(probes []ExtentProbe) [][]int {
	idx := make(map[extentGroupKey]int)
	groups := make([][]int, 0, len(probes))
	for i, p := range probes {
		k := extentGroupKey{
			sessionID: p.SessionID, lo: p.Lo, hi: p.Hi, sidechain: p.Sidechain,
		}
		gi, ok := idx[k]
		if !ok {
			idx[k] = len(groups)
			groups = append(groups, []int{i})
			continue
		}
		groups[gi] = append(groups[gi], i)
	}
	for _, g := range groups {
		sort.Slice(g, func(a, b int) bool {
			return probes[g[a]].Ordinal < probes[g[b]].Ordinal
		})
	}
	return groups
}

// resolveExtentRound issues one batched RunExtents call for the probes at
// idxs and records their extents.
func resolveExtentRound(
	ctx context.Context, q UnitBoundsQuerier, probes []ExtentProbe,
	idxs []int, extents [][2]int, resolved []bool,
) error {
	batch := make([]ExtentProbe, len(idxs))
	for k, i := range idxs {
		batch[k] = probes[i]
	}
	res, err := q.RunExtents(ctx, batch)
	if err != nil {
		return err
	}
	if len(res) != len(batch) {
		return fmt.Errorf(
			"deriving unit ranges: RunExtents returned %d results for %d probes",
			len(res), len(batch))
	}
	for k, i := range idxs {
		extents[i], resolved[i] = res[k], true
	}
	return nil
}

// shareGroupExtents hands each resolved group member's extent to the group
// siblings it covers (see deriveProbeExtents for why coverage implies the
// same run).
func shareGroupExtents(
	groups [][]int, probes []ExtentProbe,
	extents [][2]int, resolved []bool,
) {
	for _, g := range groups {
		for _, r := range g {
			if !resolved[r] {
				continue
			}
			ext := extents[r]
			for _, i := range g {
				if !resolved[i] && probes[i].Ordinal >= ext[0] && probes[i].Ordinal <= ext[1] {
					extents[i], resolved[i] = ext, true
				}
			}
		}
	}
}

// Shared backend resolvers. Every UnitBoundsQuerier implementation is the
// same backend-neutral orchestration around one batched SQL statement per
// chunk; the resolvers below own that orchestration (session/probe dedup,
// chunking, boundary resolution, alignment and invariant checks) so each
// backend supplies only its dialect's SQL builder.

// ResolveUserBoundaries implements NearestUserBoundaries' shared
// orchestration: it dedups probe sessions in first-seen order, fetches each
// chunk of at most sessionChunk distinct sessions with one call to fetch
// (which must run ONE batched statement appending each session's embeddable
// user ordinals to its out slot, aligned 1:1 with sessions), sorts the
// ordinals, and resolves every probe's exclusive boundaries in Go. The
// result aligns 1:1 with probes, with the -1 / UnitOrdinalMax sentinels for
// missing boundaries.
func ResolveUserBoundaries(
	ctx context.Context, probes []UnitProbe, sessionChunk int,
	fetch func(ctx context.Context, sessions []string, out [][]int) error,
) ([]UnitBounds, error) {
	out := make([]UnitBounds, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	sessionIdx := make(map[string]int)
	sessions := make([]string, 0, len(probes))
	for _, p := range probes {
		if _, ok := sessionIdx[p.SessionID]; ok {
			continue
		}
		sessionIdx[p.SessionID] = len(sessions)
		sessions = append(sessions, p.SessionID)
	}
	ordinals := make([][]int, len(sessions))
	for start := 0; start < len(sessions); start += sessionChunk {
		chunk := sessions[start:min(start+sessionChunk, len(sessions))]
		if err := fetch(ctx, chunk, ordinals[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for _, o := range ordinals {
		sort.Ints(o)
	}
	for i, p := range probes {
		out[i] = boundsAround(ordinals[sessionIdx[p.SessionID]], p.Ordinal)
	}
	return out, nil
}

// boundsAround resolves one probe's exclusive boundaries from a session's
// sorted embeddable user ordinals: the strict MAX(< ordinal) / MIN(> ordinal)
// neighbors, with the -1 / UnitOrdinalMax sentinels when absent.
func boundsAround(userOrdinals []int, ordinal int) UnitBounds {
	b := UnitBounds{Prev: -1, Next: UnitOrdinalMax}
	i := sort.SearchInts(userOrdinals, ordinal)
	if i > 0 {
		b.Prev = userOrdinals[i-1]
	}
	for ; i < len(userOrdinals); i++ {
		if userOrdinals[i] > ordinal {
			b.Next = userOrdinals[i]
			break
		}
	}
	return b
}

// ScanUserBoundaryRows consumes one batched boundary statement's (idx,
// ordinal) rows into out, validating each session index against the chunk —
// the shared scan half of every backend's ResolveUserBoundaries fetch.
func ScanUserBoundaryRows(rows *sql.Rows, out [][]int) error {
	for rows.Next() {
		var idx, ordinal int
		if err := rows.Scan(&idx, &ordinal); err != nil {
			return fmt.Errorf("scanning nearest user boundaries: %w", err)
		}
		if idx < 0 || idx >= len(out) {
			return fmt.Errorf(
				"nearest user boundaries: session index %d out of range [0, %d)",
				idx, len(out))
		}
		out[idx] = append(out[idx], ordinal)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating nearest user boundaries: %w", err)
	}
	return nil
}

// ResolveRunExtents implements RunExtents' shared orchestration: duplicate
// probes share one slot, and each chunk of at most probeChunk distinct
// probes resolves with one call to lookup (which must run ONE batched
// statement filling out aligned 1:1 with its probes). The result aligns 1:1
// with probes.
func ResolveRunExtents(
	ctx context.Context, probes []ExtentProbe, probeChunk int,
	lookup func(ctx context.Context, probes []ExtentProbe, out [][2]int) error,
) ([][2]int, error) {
	out := make([][2]int, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	keyIdx := make(map[ExtentProbe]int, len(probes))
	keys := make([]ExtentProbe, 0, len(probes))
	for _, p := range probes {
		if _, ok := keyIdx[p]; ok {
			continue
		}
		keyIdx[p] = len(keys)
		keys = append(keys, p)
	}
	extents := make([][2]int, len(keys))
	for start := 0; start < len(keys); start += probeChunk {
		chunk := keys[start:min(start+probeChunk, len(keys))]
		if err := lookup(ctx, chunk, extents[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for i, p := range probes {
		out[i] = extents[keyIdx[p]]
	}
	return out, nil
}

// ScanRunExtentRows consumes one batched extent statement's (idx, first,
// last) rows into out, enforcing the shared invariants — every probe index
// in range, no NULL extent side (a NULL means no same-sidechain member
// exists at the anchor, i.e. the probe was not built for a rule-2 anchor),
// and exactly one row per probe.
func ScanRunExtentRows(rows *sql.Rows, probes []ExtentProbe, out [][2]int) error {
	seen := 0
	for rows.Next() {
		var idx int
		var first, last sql.NullInt64
		if err := rows.Scan(&idx, &first, &last); err != nil {
			return fmt.Errorf("scanning run extents: %w", err)
		}
		if idx < 0 || idx >= len(out) {
			return fmt.Errorf("run extents: probe index %d out of range [0, %d)",
				idx, len(out))
		}
		if !first.Valid || !last.Valid {
			return fmt.Errorf(
				"run extents: anchor %s#%d is not an embeddable assistant row "+
					"(probe must only be built for rule-2 anchors)",
				probes[idx].SessionID, probes[idx].Ordinal)
		}
		out[idx] = [2]int{int(first.Int64), int(last.Int64)}
		seen++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating run extents: %w", err)
	}
	if seen != len(probes) {
		return fmt.Errorf("run extents: statement returned %d rows for %d probes",
			seen, len(probes))
	}
	return nil
}

// SQLite seam implementation.
var _ UnitBoundsQuerier = (*DB)(nil)

// unitSessionChunk caps sessions per NearestUserBoundaries statement so the
// VALUES CTE stays inside SQLite's bind-variable limit (see maxSQLVars): a
// session binds 2 variables (idx, session_id).
const unitSessionChunk = maxSQLVars / 2

// embeddableUserSQL is the SQL predicate matching an embeddable user row
// under the messages alias: user role, is_system = 0, and the SQLite dialect
// SystemPrefixSQL check, exactly as ScanEmbeddableUnits' scan predicate.
// (The assistant-side member predicate in runExtentSelectSQL skips the
// prefix check: SystemPrefixSQL constrains user rows only.)
func embeddableUserSQL(alias string) string {
	return fmt.Sprintf("%[1]s.role = 'user' AND %[1]s.is_system = 0 AND %[2]s",
		alias, SystemPrefixSQL(alias+".content", alias+".role"))
}

// NearestUserBoundaries returns, per probe, the nearest embeddable user
// ordinals strictly before and after the probe ordinal (sidechain is
// irrelevant: the reducer closes runs on any embeddable user row regardless
// of its is_sidechain). Sentinels -1 / UnitOrdinalMax stand in for missing
// boundaries. Orchestration is the shared ResolveUserBoundaries; one
// statement per unitSessionChunk distinct sessions fetches each session's
// embeddable user ordinals ONCE.
func (db *DB) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	return ResolveUserBoundaries(ctx, probes, unitSessionChunk,
		db.scanUserBoundaryOrdinals)
}

// scanUserBoundaryOrdinals runs the one batched statement for a chunk of
// distinct sessions: a VALUES CTE joined against messages for every
// embeddable user ordinal of each session. out aligns 1:1 with sessions.
// Constraining the fetch to session + role only keeps it on
// idx_messages_session_role, so the statement touches each session's
// (typically sparse) user rows instead of stepping every message in an
// ordinal range.
func (db *DB) scanUserBoundaryOrdinals(
	ctx context.Context, sessions []string, out [][]int,
) error {
	values := make([]string, len(sessions))
	args := make([]any, 0, len(sessions)*2)
	for i, sessionID := range sessions {
		values[i] = "(?, ?)"
		args = append(args, i, sessionID)
	}
	query := fmt.Sprintf(`
		WITH spans(idx, session_id) AS (VALUES %s)
		SELECT sp.idx, m.ordinal
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		WHERE %s`,
		strings.Join(values, ", "), embeddableUserSQL("m"))

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying nearest user boundaries: %w", err)
	}
	defer rows.Close()
	return ScanUserBoundaryRows(rows, out)
}

// unitExtentChunk caps extent probes per statement: a probe binds 6
// variables (idx, session_id, o, lo, hi, sc).
const unitExtentChunk = maxSQLVars / 6

// RunExtents returns, per probe, the first and last member ordinals of the
// anchor's same-sidechain run: the nearest embeddable assistant rows of the
// anchor's sidechain around the anchor, bounded exclusively by (Lo, Hi) and
// by the nearest STOP row inside that interval — an embeddable user row (the
// reducer closes every run at unit boundaries) or an embeddable assistant
// row of the opposite sidechain (the flip rule; both stops only matter among
// embeddable rows). Probing with the -1 / UnitOrdinalMax sentinels therefore
// derives the full rule-2 extent on its own. The anchor row itself always
// qualifies, so a probe whose interval holds no run around its anchor was
// built for a row that is not an embeddable assistant row — an internal
// invariant violation reported as an error.
//
// Orchestration is the shared ResolveRunExtents; one statement per
// unitExtentChunk distinct probes resolves every probe with correlated point
// lookups on idx_messages_session_ordinal (nearest stop row on each side,
// then the farthest same-sidechain member inside the stop-narrowed interval)
// instead of transferring each interval's member rows to Go: an
// interval-span scan moves O(interval) rows per page across the driver
// boundary, the point lookups move exactly one result row per probe.
func (db *DB) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	return ResolveRunExtents(ctx, probes, unitExtentChunk,
		db.lookupRunExtentChunk)
}

// runExtentSelectSQL builds the correlated point-lookup SELECT under a probes
// CTE with columns (idx, session_id, o, lo, hi, sc). Per probe and per side:
// the inner subquery seeks the nearest STOP row between the anchor and the
// interval bound — an embeddable user row (the reducer closes every run
// there) or an opposite-sidechain embeddable assistant row (the flip rule);
// ORDER BY ordinal DESC/ASC LIMIT 1 walks idx_messages_session_ordinal from
// the anchor outward and stops at the first hit. The outer subquery then
// seeks the farthest same-sidechain member inside the stop-narrowed
// interval. Folding the user boundary into the stop set is what lets
// DeriveUnitRanges probe with sentinel (Lo, Hi) bounds instead of paying a
// NearestUserBoundaries round trip first. The member predicate is role +
// is_system only: SystemPrefixSQL constrains user rows exclusively, so it is
// identically TRUE for assistant rows and deliberately omitted there.
func runExtentSelectSQL() string {
	return fmt.Sprintf(`
	SELECT p.idx,
	  (SELECT m.ordinal FROM messages m
	   WHERE m.session_id = p.session_id AND m.ordinal <= p.o
	     AND m.ordinal > COALESCE((SELECT f.ordinal FROM messages f
	       WHERE f.session_id = p.session_id
	         AND f.ordinal > p.lo AND f.ordinal < p.o
	         AND %[1]s
	       ORDER BY f.ordinal DESC LIMIT 1), p.lo)
	     AND m.role = 'assistant' AND m.is_system = 0
	     AND m.is_sidechain = p.sc
	   ORDER BY m.ordinal ASC LIMIT 1),
	  (SELECT m.ordinal FROM messages m
	   WHERE m.session_id = p.session_id AND m.ordinal >= p.o
	     AND m.ordinal < COALESCE((SELECT f.ordinal FROM messages f
	       WHERE f.session_id = p.session_id
	         AND f.ordinal > p.o AND f.ordinal < p.hi
	         AND %[1]s
	       ORDER BY f.ordinal ASC LIMIT 1), p.hi)
	     AND m.role = 'assistant' AND m.is_system = 0
	     AND m.is_sidechain = p.sc
	   ORDER BY m.ordinal DESC LIMIT 1)
	FROM probes p`, runStopSQL())
}

// runStopSQL is the stop-row predicate under alias f, correlated on p.sc: an
// opposite-sidechain embeddable assistant row (flip) or an embeddable user
// row (unit boundary).
func runStopSQL() string {
	return "((f.role = 'assistant' AND f.is_system = 0 AND f.is_sidechain <> p.sc)" +
		" OR (" + embeddableUserSQL("f") + "))"
}

// lookupRunExtentChunk runs the one batched statement for a chunk of distinct
// extent probes: a VALUES CTE with the correlated point lookups of
// runExtentSelectSQL.
func (db *DB) lookupRunExtentChunk(
	ctx context.Context, probes []ExtentProbe, out [][2]int,
) error {
	values := make([]string, len(probes))
	args := make([]any, 0, len(probes)*6)
	for i, p := range probes {
		values[i] = "(?, ?, ?, ?, ?, ?)"
		args = append(args, i, p.SessionID, p.Ordinal, p.Lo, p.Hi, p.Sidechain)
	}
	query := "WITH probes(idx, session_id, o, lo, hi, sc) AS (VALUES " +
		strings.Join(values, ", ") + ")" + runExtentSelectSQL()

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying run extents: %w", err)
	}
	defer rows.Close()
	return ScanRunExtentRows(rows, probes, out)
}
