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
// SQLite implementation in internal/db/unit_range.go: NearestUserBoundaries
// groups probes per session and fetches each session's embeddable user
// ordinals once, RunExtents dedups probes and resolves each with correlated
// point lookups, and each chunk of sessions or probes costs one statement.
var _ db.UnitBoundsQuerier = (*Store)(nil)

// pgUnitSessionChunk caps sessions per NearestUserBoundaries statement,
// matching SQLite's unitSessionChunk semantics: a session binds 2 variables
// (idx, session_id).
const pgUnitSessionChunk = maxPGVars / 2

// pgUnitExtentChunk caps extent probes per RunExtents statement, matching
// SQLite's unitExtentChunk semantics: a probe binds 6 variables (idx,
// session_id, o, lo, hi, sc).
const pgUnitExtentChunk = maxPGVars / 6

// pgUnitOrdinalMax is the Hi sentinel for a missing upper boundary, the
// value documented on db.UnitBounds (ordinals are PG INTEGER, int32-safe).
const pgUnitOrdinalMax = 1<<31 - 1

// pgEmbeddableUserSQL is the PostgreSQL predicate matching an embeddable
// user row under the given alias: user role, is_system = FALSE, and the
// PG dialect SystemPrefixSQL check — the PG form of internal/db's
// embeddableUserSQL. (The assistant-side member predicate skips the prefix
// check: SystemPrefixSQL constrains user rows only.)
func pgEmbeddableUserSQL(alias string) string {
	return fmt.Sprintf("%[1]s.role = 'user' AND %[1]s.is_system = FALSE AND %[2]s",
		alias, db.PostgresSystemPrefixSQL(alias+".content", alias+".role"))
}

// NearestUserBoundaries returns, per probe, the nearest embeddable user
// ordinals strictly before and after the probe ordinal, with the -1 /
// pgUnitOrdinalMax sentinels standing in for missing boundaries — the exact
// semantics of the SQLite seam method (see internal/db/unit_range.go).
//
// Probes are grouped per session: one statement per pgUnitSessionChunk
// distinct sessions fetches each session's embeddable user ordinals ONCE,
// and every probe's boundaries resolve from the sorted ordinals in Go.
func (s *Store) NearestUserBoundaries(
	ctx context.Context, probes []db.UnitProbe,
) ([]db.UnitBounds, error) {
	out := make([]db.UnitBounds, len(probes))
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
	for start := 0; start < len(sessions); start += pgUnitSessionChunk {
		chunk := sessions[start:min(start+pgUnitSessionChunk, len(sessions))]
		if err := s.scanPGUserBoundaryOrdinals(ctx, chunk, ordinals[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for _, o := range ordinals {
		sort.Ints(o)
	}
	for i, p := range probes {
		out[i] = pgBoundsAround(ordinals[sessionIdx[p.SessionID]], p.Ordinal)
	}
	return out, nil
}

// pgBoundsAround resolves one probe's exclusive boundaries from a session's
// sorted embeddable user ordinals: the strict MAX(< ordinal) / MIN(> ordinal)
// neighbors, with the -1 / pgUnitOrdinalMax sentinels when absent.
func pgBoundsAround(userOrdinals []int, ordinal int) db.UnitBounds {
	b := db.UnitBounds{Prev: -1, Next: pgUnitOrdinalMax}
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

// scanPGUserBoundaryOrdinals runs the one batched statement for a chunk of
// distinct sessions: a VALUES CTE joined against messages for every
// embeddable user ordinal of each session. out aligns 1:1 with sessions.
func (s *Store) scanPGUserBoundaryOrdinals(
	ctx context.Context, sessions []string, out [][]int,
) error {
	pb := &paramBuilder{}
	values := make([]string, len(sessions))
	for i, sessionID := range sessions {
		values[i] = fmt.Sprintf("(%s::int, %s::text)", pb.add(i), pb.add(sessionID))
	}
	query := fmt.Sprintf(`
		WITH spans(idx, session_id) AS (VALUES %s)
		SELECT sp.idx, m.ordinal
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		WHERE %s`,
		strings.Join(values, ", "), pgEmbeddableUserSQL("m"))

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return fmt.Errorf("querying nearest user boundaries: %w", err)
	}
	defer rows.Close()
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

// RunExtents returns, per probe, the first and last member ordinals of the
// anchor's same-sidechain run, bounded exclusively by (Lo, Hi) and by the
// nearest STOP row inside that interval — an embeddable user row or an
// opposite-sidechain embeddable assistant row — the exact semantics of the
// SQLite seam method (see internal/db/unit_range.go). Probing with the -1 /
// pgUnitOrdinalMax sentinels therefore derives the full rule-2 extent on its
// own.
//
// Duplicate probes share one slot, and one statement per pgUnitExtentChunk
// distinct probes resolves every probe with correlated point lookups
// (nearest stop row on each side, then the farthest same-sidechain member
// inside the stop-narrowed interval), moving exactly one result row per
// probe instead of each interval's member rows.
func (s *Store) RunExtents(
	ctx context.Context, probes []db.ExtentProbe,
) ([][2]int, error) {
	out := make([][2]int, len(probes))
	if len(probes) == 0 {
		return out, nil
	}
	keyIdx := make(map[db.ExtentProbe]int, len(probes))
	keys := make([]db.ExtentProbe, 0, len(probes))
	for _, p := range probes {
		if _, ok := keyIdx[p]; ok {
			continue
		}
		keyIdx[p] = len(keys)
		keys = append(keys, p)
	}
	extents := make([][2]int, len(keys))
	for start := 0; start < len(keys); start += pgUnitExtentChunk {
		chunk := keys[start:min(start+pgUnitExtentChunk, len(keys))]
		if err := s.lookupPGRunExtentChunk(ctx, chunk, extents[start:start+len(chunk)]); err != nil {
			return nil, err
		}
	}
	for i, p := range probes {
		out[i] = extents[keyIdx[p]]
	}
	return out, nil
}

// pgRunExtentSelectSQL builds the correlated point-lookup SELECT under a
// probes CTE with columns (idx, session_id, o, lo, hi, sc) — the PG form of
// internal/db's runExtentSelectSQL. Per probe and per side: the inner
// subquery seeks the nearest stop row between the anchor and the interval
// bound, the outer subquery seeks the farthest same-sidechain member inside
// the stop-narrowed interval. The member predicate is role + is_system only:
// SystemPrefixSQL constrains user rows exclusively, so it is identically
// TRUE for assistant rows and deliberately omitted there.
func pgRunExtentSelectSQL() string {
	stop := "((f.role = 'assistant' AND f.is_system = FALSE AND f.is_sidechain <> p.sc)" +
		" OR (" + pgEmbeddableUserSQL("f") + "))"
	return fmt.Sprintf(`
	SELECT p.idx,
	  (SELECT m.ordinal FROM messages m
	   WHERE m.session_id = p.session_id AND m.ordinal <= p.o
	     AND m.ordinal > COALESCE((SELECT f.ordinal FROM messages f
	       WHERE f.session_id = p.session_id
	         AND f.ordinal > p.lo AND f.ordinal < p.o
	         AND %[1]s
	       ORDER BY f.ordinal DESC LIMIT 1), p.lo)
	     AND m.role = 'assistant' AND m.is_system = FALSE
	     AND m.is_sidechain = p.sc
	   ORDER BY m.ordinal ASC LIMIT 1),
	  (SELECT m.ordinal FROM messages m
	   WHERE m.session_id = p.session_id AND m.ordinal >= p.o
	     AND m.ordinal < COALESCE((SELECT f.ordinal FROM messages f
	       WHERE f.session_id = p.session_id
	         AND f.ordinal > p.o AND f.ordinal < p.hi
	         AND %[1]s
	       ORDER BY f.ordinal ASC LIMIT 1), p.hi)
	     AND m.role = 'assistant' AND m.is_system = FALSE
	     AND m.is_sidechain = p.sc
	   ORDER BY m.ordinal DESC LIMIT 1)
	FROM probes p`, stop)
}

// lookupPGRunExtentChunk runs the one batched statement for a chunk of
// distinct extent probes: a VALUES CTE with the correlated point lookups of
// pgRunExtentSelectSQL. A NULL extent side means no same-sidechain member
// exists at the anchor — the probe was not built for a rule-2 anchor.
func (s *Store) lookupPGRunExtentChunk(
	ctx context.Context, probes []db.ExtentProbe, out [][2]int,
) error {
	pb := &paramBuilder{}
	values := make([]string, len(probes))
	for i, p := range probes {
		values[i] = fmt.Sprintf(
			"(%s::int, %s::text, %s::int, %s::int, %s::int, %s::boolean)",
			pb.add(i), pb.add(p.SessionID), pb.add(p.Ordinal),
			pb.add(p.Lo), pb.add(p.Hi), pb.add(p.Sidechain))
	}
	query := "WITH probes(idx, session_id, o, lo, hi, sc) AS (VALUES " +
		strings.Join(values, ", ") + ")" + pgRunExtentSelectSQL()

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return fmt.Errorf("querying run extents: %w", err)
	}
	defer rows.Close()
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
// (constant batched statement count for the whole page), then per-match
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
