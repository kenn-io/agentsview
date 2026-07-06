package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// PostgreSQL implementation of the conversation-unit seam, mirroring the
// SQLite span-scan implementation in internal/db/unit_range.go: probes are
// grouped into per-session (NearestUserBoundaries) or per-interval
// (RunExtents) spans, each pgUnitSpanChunk-sized chunk of spans costs one
// statement, and per-probe results resolve from the scanned rows in Go.
var _ db.UnitBoundsQuerier = (*Store)(nil)

// pgUnitSpanChunk caps spans per statement, matching SQLite's unitSpanChunk
// semantics: a span binds 4 variables (idx, session_id, lo, hi).
const pgUnitSpanChunk = maxPGVars / 4

// pgUnitOrdinalMax is the Hi sentinel for a missing upper boundary, the
// value documented on db.UnitBounds (ordinals are PG INTEGER, int32-safe).
const pgUnitOrdinalMax = 1<<31 - 1

// pgEmbeddableUserSQL is the PostgreSQL predicate matching an embeddable
// user row under the messages alias: user role, is_system = FALSE, and the
// PG dialect SystemPrefixSQL check — the PG form of internal/db's
// embeddableUserSQL. (The assistant-side member predicate skips the prefix
// check: SystemPrefixSQL constrains user rows only.)
func pgEmbeddableUserSQL(alias string) string {
	return fmt.Sprintf("%[1]s.role = 'user' AND %[1]s.is_system = FALSE AND %[2]s",
		alias, db.PostgresSystemPrefixSQL(alias+".content", alias+".role"))
}

// pgUserBoundarySpan accumulates one session's boundary data: the embeddable
// user ordinals inside [lo, hi] (the session's probe-ordinal span), plus the
// nearest embeddable user ordinal below lo and above hi (COALESCEd to the
// -1 / pgUnitOrdinalMax sentinels in SQL).
type pgUserBoundarySpan struct {
	sessionID    string
	lo, hi       int
	inner        []int
	below, above int
	belowSeen    bool
	aboveSeen    bool
}

// NearestUserBoundaries returns, per probe, the nearest embeddable user
// ordinals strictly before and after the probe ordinal, with the -1 /
// pgUnitOrdinalMax sentinels standing in for missing boundaries — the exact
// semantics of the SQLite seam method (see internal/db/unit_range.go).
func (s *Store) NearestUserBoundaries(
	ctx context.Context, probes []db.UnitProbe,
) ([]db.UnitBounds, error) {
	out := make([]db.UnitBounds, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	spanIdx := make(map[string]int)
	spans := make([]pgUserBoundarySpan, 0, len(probes))
	for _, p := range probes {
		i, ok := spanIdx[p.SessionID]
		if !ok {
			spanIdx[p.SessionID] = len(spans)
			spans = append(spans, pgUserBoundarySpan{
				sessionID: p.SessionID, lo: p.Ordinal, hi: p.Ordinal,
			})
			continue
		}
		spans[i].lo = min(spans[i].lo, p.Ordinal)
		spans[i].hi = max(spans[i].hi, p.Ordinal)
	}
	for start := 0; start < len(spans); start += pgUnitSpanChunk {
		chunk := spans[start:min(start+pgUnitSpanChunk, len(spans))]
		if err := s.scanPGUserBoundarySpans(ctx, chunk); err != nil {
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
func (sp *pgUserBoundarySpan) boundsAround(ordinal int) db.UnitBounds {
	b := db.UnitBounds{Prev: sp.below, Next: sp.above}
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

// scanPGUserBoundarySpans runs the one batched statement for a chunk of
// spans: a VALUES CTE joined against messages for the in-span embeddable
// user rows (third column NULL), plus one sentinel row per span carrying the
// correlated point lookups below lo and above hi.
func (s *Store) scanPGUserBoundarySpans(
	ctx context.Context, spans []pgUserBoundarySpan,
) error {
	pb := &paramBuilder{}
	values := make([]string, len(spans))
	for i := range spans {
		values[i] = fmt.Sprintf("(%s::int, %s::text, %s::int, %s::int)",
			pb.add(i), pb.add(spans[i].sessionID),
			pb.add(spans[i].lo), pb.add(spans[i].hi))
	}
	user := pgEmbeddableUserSQL("m")
	query := fmt.Sprintf(`
		WITH spans(idx, session_id, lo, hi) AS (VALUES %s)
		SELECT sp.idx, m.ordinal, NULL::int
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
		strings.Join(values, ", "), user, user, user, pgUnitOrdinalMax)

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
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

// pgExtentSpanKey groups extent probes sharing one exclusive interval, so
// the interval's member rows are fetched once no matter how many anchors
// fall inside it.
type pgExtentSpanKey struct {
	sessionID string
	lo, hi    int
}

// pgRunMemberRow is one embeddable assistant row inside an extent interval.
type pgRunMemberRow struct {
	ordinal   int
	sidechain bool
}

// RunExtents returns, per probe, the first and last member ordinals of the
// anchor's same-sidechain run inside the exclusive (Lo, Hi) interval, bounded
// by the nearest opposite-sidechain embeddable assistant row (the reducer's
// flip rule) — the exact semantics of the SQLite seam method.
func (s *Store) RunExtents(
	ctx context.Context, probes []db.ExtentProbe,
) ([][2]int, error) {
	out := make([][2]int, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	spanIdx := make(map[pgExtentSpanKey]int)
	spans := make([]pgExtentSpanKey, 0, len(probes))
	for _, p := range probes {
		k := pgExtentSpanKey{sessionID: p.SessionID, lo: p.Lo, hi: p.Hi}
		if _, ok := spanIdx[k]; ok {
			continue
		}
		spanIdx[k] = len(spans)
		spans = append(spans, k)
	}
	members := make([][]pgRunMemberRow, len(spans))
	for start := 0; start < len(spans); start += pgUnitSpanChunk {
		chunk := spans[start:min(start+pgUnitSpanChunk, len(spans))]
		if err := s.scanPGRunMemberSpans(ctx, chunk, members[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for _, m := range members {
		sort.Slice(m, func(a, b int) bool { return m[a].ordinal < m[b].ordinal })
	}
	for i, p := range probes {
		ext, err := pgRunExtentWithin(members[spanIdx[pgExtentSpanKey{
			sessionID: p.SessionID, lo: p.Lo, hi: p.Hi,
		}]], p)
		if err != nil {
			return nil, err
		}
		out[i] = ext
	}
	return out, nil
}

// pgRunExtentWithin walks the interval's sorted member rows around the probe
// anchor: descending below the anchor and ascending above it, collecting the
// farthest same-sidechain member on each side and stopping at the first
// opposite-sidechain member (the flip that closes the run). No member at the
// anchor's side means the probe was not built for a rule-2 anchor.
func pgRunExtentWithin(members []pgRunMemberRow, p db.ExtentProbe) ([2]int, error) {
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

// scanPGRunMemberSpans runs the one batched statement for a chunk of extent
// intervals: a VALUES CTE joined against messages for each interval's
// embeddable assistant rows, exclusive of the (lo, hi) bounds. The member
// predicate is role + is_system only: SystemPrefixSQL constrains user rows
// exclusively, so it is identically TRUE for assistant rows and deliberately
// omitted here.
func (s *Store) scanPGRunMemberSpans(
	ctx context.Context, spans []pgExtentSpanKey, out [][]pgRunMemberRow,
) error {
	pb := &paramBuilder{}
	values := make([]string, len(spans))
	for i, sp := range spans {
		values[i] = fmt.Sprintf("(%s::int, %s::text, %s::int, %s::int)",
			pb.add(i), pb.add(sp.sessionID), pb.add(sp.lo), pb.add(sp.hi))
	}
	query := fmt.Sprintf(`
		WITH spans(idx, session_id, lo, hi) AS (VALUES %s)
		SELECT sp.idx, m.ordinal, m.is_sidechain
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		  AND m.ordinal > sp.lo AND m.ordinal < sp.hi
		WHERE m.role = 'assistant' AND m.is_system = FALSE`,
		strings.Join(values, ", "))

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return fmt.Errorf("querying run extents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var m pgRunMemberRow
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

// pgAnchorMetaChunk caps (session_id, ordinal) refs per anchor-meta lookup,
// matching internal/db's enrichHitsChunk semantics (2 binds per ref).
const pgAnchorMetaChunk = maxPGVars / 2

// pgAnchorKey identifies one (session_id, ordinal) anchor ref.
type pgAnchorKey struct {
	sessionID string
	ordinal   int
}

// pgAnchorMeta is one match's anchor metadata: session lineage plus the
// anchor message row's classification columns — the PG twin of internal/db's
// contentAnchorMeta.
type pgAnchorMeta struct {
	relationship    string
	parentSessionID string
	role            sql.NullString
	sidechain       sql.NullBool
	embeddable      sql.NullBool
	missing         bool
}

// deriveLexicalUnitsPG is the shared post-scan pass for the PG substring,
// regex, and fts-fallback modes, mirroring internal/db's deriveLexicalUnits:
// one batched anchor-meta lookup, one shared db.DeriveUnitRanges derivation
// (one batched statement per seam method for the whole page), then per-match
// assignment of OrdinalRange and the lineage fields. matches is the already
// truncated page, so the pass is O(page).
func (s *Store) deriveLexicalUnitsPG(
	ctx context.Context, matches []db.ContentMatch,
) error {
	if len(matches) == 0 {
		return nil
	}
	metas, err := s.fillAnchorMetaPG(ctx, matches)
	if err != nil {
		return err
	}
	anchors := make([]db.UnitAnchor, len(matches))
	for i, m := range matches {
		meta := metas[i]
		anchors[i] = db.UnitAnchor{
			SessionID:  m.SessionID,
			Ordinal:    m.Ordinal,
			Role:       meta.role.String,
			Sidechain:  meta.sidechain.Valid && meta.sidechain.Bool,
			Embeddable: meta.embeddable.Valid && meta.embeddable.Bool,
			Missing:    meta.missing,
		}
	}
	ranges, err := db.DeriveUnitRanges(ctx, s, anchors)
	if err != nil {
		return fmt.Errorf("deriving lexical units: %w", err)
	}
	for i := range matches {
		matches[i].OrdinalRange = ranges[i]
		matches[i].Relationship = metas[i].relationship
		matches[i].ParentSessionID = metas[i].parentSessionID
		matches[i].Sidechain = anchors[i].Sidechain
		matches[i].Subordinate = anchors[i].Sidechain ||
			db.SubordinateSession(metas[i].relationship, metas[i].parentSessionID)
	}
	return nil
}

// fillAnchorMetaPG fetches anchor classification and session lineage for
// every page row: one batched VALUES-CTE lookup per pgAnchorMetaChunk
// distinct (session_id, ordinal) refs, never a per-row query. Refs whose
// message row does not exist (tool_result_events orphans) are marked missing
// so derivation falls back to [o, o]; their session lineage still resolves
// via the sessions join. The result aligns 1:1 with matches.
func (s *Store) fillAnchorMetaPG(
	ctx context.Context, matches []db.ContentMatch,
) ([]pgAnchorMeta, error) {
	seen := make(map[pgAnchorKey]bool, len(matches))
	refs := make([]pgAnchorKey, 0, len(matches))
	for i := range matches {
		key := pgAnchorKey{matches[i].SessionID, matches[i].Ordinal}
		if !seen[key] {
			seen[key] = true
			refs = append(refs, key)
		}
	}
	found := make(map[pgAnchorKey]pgAnchorMeta, len(refs))
	for start := 0; start < len(refs); start += pgAnchorMetaChunk {
		chunk := refs[start:min(start+pgAnchorMetaChunk, len(refs))]
		if err := s.lookupAnchorMetaChunkPG(ctx, chunk, found); err != nil {
			return nil, err
		}
	}
	metas := make([]pgAnchorMeta, len(matches))
	for i := range matches {
		got, ok := found[pgAnchorKey{matches[i].SessionID, matches[i].Ordinal}]
		if !ok {
			metas[i].missing = true
			continue
		}
		got.missing = !got.role.Valid
		metas[i] = got
	}
	return metas, nil
}

// lookupAnchorMetaChunkPG resolves one chunk of (session_id, ordinal) refs to
// session lineage plus the anchor message row's classification columns:
// role, sidechain, and the embeddable flag (is_system = FALSE AND content not
// system-prefixed, exactly the embedding reducer's predicate). messages is
// LEFT JOINed so a ref whose message row is absent still resolves lineage;
// its classification columns come back NULL.
func (s *Store) lookupAnchorMetaChunkPG(
	ctx context.Context, refs []pgAnchorKey,
	out map[pgAnchorKey]pgAnchorMeta,
) error {
	pb := &paramBuilder{}
	values := make([]string, len(refs))
	for i, r := range refs {
		values[i] = fmt.Sprintf("(%s::text, %s::int)",
			pb.add(r.sessionID), pb.add(r.ordinal))
	}
	query := "WITH refs(session_id, ordinal) AS (VALUES " +
		strings.Join(values, ", ") + ") " +
		"SELECT r.session_id, r.ordinal, " +
		"COALESCE(s.relationship_type,''), COALESCE(s.parent_session_id,''), " +
		"m.role, m.is_sidechain, " +
		"CASE WHEN m.is_system = FALSE AND " +
		db.PostgresSystemPrefixSQL("m.content", "m.role") +
		" THEN TRUE ELSE FALSE END " +
		"FROM refs r " +
		"JOIN sessions s ON s.id = r.session_id " +
		"LEFT JOIN messages m ON m.session_id = r.session_id AND m.ordinal = r.ordinal"

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return fmt.Errorf("looking up match anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key pgAnchorKey
		var meta pgAnchorMeta
		if err := rows.Scan(&key.sessionID, &key.ordinal,
			&meta.relationship, &meta.parentSessionID,
			&meta.role, &meta.sidechain, &meta.embeddable); err != nil {
			return fmt.Errorf("scanning match anchor: %w", err)
		}
		out[key] = meta
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating match anchors: %w", err)
	}
	return nil
}
