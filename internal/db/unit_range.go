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

// DeriveUnitRanges applies the spec's rules 1-3 + missing-anchor fallback,
// memoized per session. Result aligns 1:1 with anchors.
//
// Rule-1 (embeddable user), rule-3 (system rows, other roles, non-embeddable
// rows), and missing anchors resolve to [o, o] with no queries. Embeddable
// assistant anchors resolve in rounds: each round batches one
// NearestUserBoundaries call and one RunExtents call for the lowest-ordinal
// unresolved anchor per session, then reuses the derived range (memoized per
// session) for every other anchor whose ordinal falls inside it with a
// matching sidechain. A page whose anchors sit in one run therefore costs a
// single one-probe round; extra rounds happen only when one session's anchors
// span multiple runs.
func DeriveUnitRanges(
	ctx context.Context, q UnitBoundsQuerier, anchors []UnitAnchor,
) ([][2]int, error) {
	out := make([][2]int, len(anchors))
	pending := classifyUnitAnchors(anchors, out)
	memo := make(map[string][]unitMemoRange)
	for len(pending) > 0 {
		pending = resolveFromUnitMemo(anchors, pending, memo, out)
		if len(pending) == 0 {
			break
		}
		if err := deriveRepresentativeRanges(ctx, q, anchors, pending, memo); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unitMemoRange is one derived run range, memoized per session so other
// anchors inside it reuse it instead of re-querying.
type unitMemoRange struct {
	lo, hi    int
	sidechain bool
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

// lookupUnitMemo finds a memoized range containing the anchor's ordinal with
// the anchor's sidechain value. Run spans never contain an embeddable
// assistant row of the opposite sidechain (a flip would have closed the
// run), so a sidechain-matched containment hit is always the anchor's run.
func lookupUnitMemo(
	memo map[string][]unitMemoRange, a UnitAnchor,
) ([2]int, bool) {
	for _, r := range memo[a.SessionID] {
		if r.sidechain == a.Sidechain && r.lo <= a.Ordinal && a.Ordinal <= r.hi {
			return [2]int{r.lo, r.hi}, true
		}
	}
	return [2]int{}, false
}

// resolveFromUnitMemo resolves every pending anchor covered by a memoized
// range and returns the still-unresolved indexes.
func resolveFromUnitMemo(
	anchors []UnitAnchor, pending []int,
	memo map[string][]unitMemoRange, out [][2]int,
) []int {
	remaining := pending[:0]
	for _, i := range pending {
		if r, ok := lookupUnitMemo(memo, anchors[i]); ok {
			out[i] = r
			continue
		}
		remaining = append(remaining, i)
	}
	return remaining
}

// unitSessionRepresentatives picks one pending anchor per session (the
// lowest ordinal) to probe this round; the derived range then resolves every
// same-run anchor via the memo.
func unitSessionRepresentatives(anchors []UnitAnchor, pending []int) []int {
	best := make(map[string]int)
	for _, i := range pending {
		j, ok := best[anchors[i].SessionID]
		if !ok || anchors[i].Ordinal < anchors[j].Ordinal {
			best[anchors[i].SessionID] = i
		}
	}
	reps := make([]int, 0, len(best))
	for _, i := range best {
		reps = append(reps, i)
	}
	sort.Ints(reps)
	return reps
}

// deriveRepresentativeRanges runs one batched NearestUserBoundaries call and
// one batched RunExtents call for this round's session representatives,
// recording each derived range in the memo. Every derived range must cover
// its own anchor (the anchor row qualifies for its run by construction), so
// the caller's memo pass is guaranteed to make progress.
func deriveRepresentativeRanges(
	ctx context.Context, q UnitBoundsQuerier, anchors []UnitAnchor,
	pending []int, memo map[string][]unitMemoRange,
) error {
	reps := unitSessionRepresentatives(anchors, pending)
	probes := make([]UnitProbe, len(reps))
	for k, i := range reps {
		probes[k] = UnitProbe{
			SessionID: anchors[i].SessionID, Ordinal: anchors[i].Ordinal,
		}
	}
	bounds, err := q.NearestUserBoundaries(ctx, probes)
	if err != nil {
		return err
	}
	if len(bounds) != len(reps) {
		return fmt.Errorf(
			"deriving unit ranges: NearestUserBoundaries returned %d results for %d probes",
			len(bounds), len(reps))
	}
	extentProbes := make([]ExtentProbe, len(reps))
	for k, i := range reps {
		extentProbes[k] = ExtentProbe{
			SessionID: anchors[i].SessionID, Ordinal: anchors[i].Ordinal,
			Lo: bounds[k].Prev, Hi: bounds[k].Next,
			Sidechain: anchors[i].Sidechain,
		}
	}
	extents, err := q.RunExtents(ctx, extentProbes)
	if err != nil {
		return err
	}
	if len(extents) != len(reps) {
		return fmt.Errorf(
			"deriving unit ranges: RunExtents returned %d results for %d probes",
			len(extents), len(reps))
	}
	for k, i := range reps {
		a := anchors[i]
		e := extents[k]
		if e[0] > a.Ordinal || e[1] < a.Ordinal {
			return fmt.Errorf(
				"deriving unit ranges: run extent [%d, %d] does not cover anchor %s#%d",
				e[0], e[1], a.SessionID, a.Ordinal)
		}
		memo[a.SessionID] = append(memo[a.SessionID],
			unitMemoRange{lo: e[0], hi: e[1], sidechain: a.Sidechain})
	}
	return nil
}

// SQLite seam implementation.
var _ UnitBoundsQuerier = (*DB)(nil)

// unitBoundsProbeChunk and unitExtentProbeChunk cap probes per statement so
// the VALUES CTE stays inside SQLite's bind-variable limit (see maxSQLVars):
// a bounds probe binds 3 variables (idx, session_id, ordinal), an extent
// probe binds 6 (idx, session_id, ordinal, lo, hi, sidechain).
const (
	unitBoundsProbeChunk = maxSQLVars / 3
	unitExtentProbeChunk = maxSQLVars / 6
)

// embeddableRoleSQL is the SQL predicate matching an embeddable row of the
// given role under the messages alias: role match, is_system = 0, and the
// SQLite dialect SystemPrefixSQL check (which by its own logic only
// constrains user rows, exactly as ScanEmbeddableUnits' scan predicate).
func embeddableRoleSQL(alias, role string) string {
	return fmt.Sprintf("%[1]s.role = '%[2]s' AND %[1]s.is_system = 0 AND %[3]s",
		alias, role, SystemPrefixSQL(alias+".content", alias+".role"))
}

// NearestUserBoundaries returns, per probe, the nearest embeddable user
// ordinals strictly before and after the probe ordinal (sidechain is
// irrelevant: the reducer closes runs on any embeddable user row regardless
// of its is_sidechain). Sentinels -1 / unitOrdinalMax stand in for missing
// boundaries. Probes are batched through a VALUES CTE, chunked at
// unitBoundsProbeChunk.
func (db *DB) NearestUserBoundaries(
	ctx context.Context, probes []UnitProbe,
) ([]UnitBounds, error) {
	out := make([]UnitBounds, len(probes))
	for start := 0; start < len(probes); start += unitBoundsProbeChunk {
		chunk := probes[start:min(start+unitBoundsProbeChunk, len(probes))]
		if err := db.nearestUserBoundariesChunk(ctx, chunk, out[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (db *DB) nearestUserBoundariesChunk(
	ctx context.Context, probes []UnitProbe, out []UnitBounds,
) error {
	values := make([]string, len(probes))
	args := make([]any, 0, len(probes)*3)
	for i, p := range probes {
		values[i] = "(?, ?, ?)"
		args = append(args, i, p.SessionID, p.Ordinal)
	}
	user := embeddableRoleSQL("m", "user")
	query := fmt.Sprintf(`
		WITH probes(idx, session_id, ordinal) AS (VALUES %s)
		SELECT p.idx,
		  COALESCE((SELECT MAX(m.ordinal) FROM messages m
		    WHERE m.session_id = p.session_id AND m.ordinal < p.ordinal
		      AND %s), -1),
		  COALESCE((SELECT MIN(m.ordinal) FROM messages m
		    WHERE m.session_id = p.session_id AND m.ordinal > p.ordinal
		      AND %s), %d)
		FROM probes p`,
		strings.Join(values, ", "), user, user, unitOrdinalMax)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying nearest user boundaries: %w", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var idx int
		var b UnitBounds
		if err := rows.Scan(&idx, &b.Prev, &b.Next); err != nil {
			return fmt.Errorf("scanning nearest user boundaries: %w", err)
		}
		if idx < 0 || idx >= len(out) {
			return fmt.Errorf(
				"nearest user boundaries: probe index %d out of range [0, %d)",
				idx, len(out))
		}
		out[idx] = b
		seen++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating nearest user boundaries: %w", err)
	}
	if seen != len(probes) {
		return fmt.Errorf(
			"nearest user boundaries: got %d rows for %d probes", seen, len(probes))
	}
	return nil
}

// RunExtents returns, per probe, the first and last member ordinals of the
// anchor's same-sidechain run: the nearest embeddable assistant rows of the
// anchor's sidechain around the anchor, bounded exclusively by (Lo, Hi) and
// by the nearest embeddable assistant row of the opposite sidechain inside
// that interval (the reducer's flip rule; flips only matter among embeddable
// assistant rows). The anchor row itself always qualifies, so a NULL
// endpoint means the probe's anchor is not an embeddable assistant row —
// an internal invariant violation reported as an error. Probes are batched
// through a VALUES CTE, chunked at unitExtentProbeChunk.
func (db *DB) RunExtents(
	ctx context.Context, probes []ExtentProbe,
) ([][2]int, error) {
	out := make([][2]int, len(probes))
	for start := 0; start < len(probes); start += unitExtentProbeChunk {
		chunk := probes[start:min(start+unitExtentProbeChunk, len(probes))]
		if err := db.runExtentsChunk(ctx, chunk, out[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (db *DB) runExtentsChunk(
	ctx context.Context, probes []ExtentProbe, out [][2]int,
) error {
	values := make([]string, len(probes))
	args := make([]any, 0, len(probes)*6)
	for i, p := range probes {
		values[i] = "(?, ?, ?, ?, ?, ?)"
		args = append(args, i, p.SessionID, p.Ordinal, p.Lo, p.Hi, p.Sidechain)
	}
	member := embeddableRoleSQL("m", "assistant")
	flip := embeddableRoleSQL("f", "assistant")
	query := fmt.Sprintf(`
		WITH probes(idx, session_id, ordinal, lo, hi, sidechain) AS (VALUES %s)
		SELECT p.idx,
		  (SELECT MIN(m.ordinal) FROM messages m
		    WHERE m.session_id = p.session_id AND m.ordinal <= p.ordinal
		      AND m.ordinal > COALESCE((SELECT MAX(f.ordinal) FROM messages f
		            WHERE f.session_id = p.session_id AND f.ordinal > p.lo
		              AND f.ordinal < p.ordinal
		              AND %[2]s
		              AND f.is_sidechain <> p.sidechain), p.lo)
		      AND %[3]s
		      AND m.is_sidechain = p.sidechain),
		  (SELECT MAX(m.ordinal) FROM messages m
		    WHERE m.session_id = p.session_id AND m.ordinal >= p.ordinal
		      AND m.ordinal < COALESCE((SELECT MIN(f.ordinal) FROM messages f
		            WHERE f.session_id = p.session_id AND f.ordinal < p.hi
		              AND f.ordinal > p.ordinal
		              AND %[2]s
		              AND f.is_sidechain <> p.sidechain), p.hi)
		      AND %[3]s
		      AND m.is_sidechain = p.sidechain)
		FROM probes p`,
		strings.Join(values, ", "), flip, member)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying run extents: %w", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		if err := scanRunExtentRow(rows, probes, out); err != nil {
			return err
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating run extents: %w", err)
	}
	if seen != len(probes) {
		return fmt.Errorf("run extents: got %d rows for %d probes", seen, len(probes))
	}
	return nil
}

// scanRunExtentRow scans one run-extents result row into out, failing fast
// on a NULL endpoint: the anchor row always qualifies for its own run, so
// NULL means the probe was built for a row that is not an embeddable
// assistant row (or does not exist).
func scanRunExtentRow(
	rows *sql.Rows, probes []ExtentProbe, out [][2]int,
) error {
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
		p := probes[idx]
		return fmt.Errorf(
			"run extents: anchor %s#%d is not an embeddable assistant row "+
				"(probe must only be built for rule-2 anchors)",
			p.SessionID, p.Ordinal)
	}
	out[idx] = [2]int{int(first.Int64), int(last.Int64)}
	return nil
}
