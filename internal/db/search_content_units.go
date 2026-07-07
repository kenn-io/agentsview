package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// contentAnchorMeta is one match's anchor metadata: session lineage plus the
// anchor message row's classification columns (role, sidechain, embeddable).
// fillAnchorMeta resolves all of it post-truncation with one batched lookup —
// deliberately NOT in the search SQL, where extra columns would be evaluated
// for every candidate row before the LIMIT and carried through the sort.
// Rows whose message row does not exist are marked missing.
type contentAnchorMeta struct {
	relationship    string
	parentSessionID string
	role            sql.NullString
	sidechain       sql.NullBool
	embeddable      sql.NullBool
	missing         bool
}

// deriveLexicalUnits is the shared post-scan pass for the substring, regex,
// and fts modes: it fetches each row's anchor classification and session
// lineage with one batched lookup, derives each match's conversation-unit
// range (DeriveUnitRanges issues one batched statement per seam method for
// the whole page, deduplicating duplicate probes), and assigns OrdinalRange
// plus the lineage fields. matches is the already-truncated page, so the
// pass is O(page).
func (db *DB) deriveLexicalUnits(
	ctx context.Context, matches []ContentMatch,
) error {
	if len(matches) == 0 {
		return nil
	}
	metas, err := db.fillAnchorMeta(ctx, matches)
	if err != nil {
		return err
	}
	anchors := buildContentUnitAnchors(matches, metas)
	ranges, err := DeriveUnitRanges(ctx, db, anchors)
	if err != nil {
		return fmt.Errorf("deriving lexical units: %w", err)
	}
	for i := range matches {
		matches[i].OrdinalRange = ranges[i]
		matches[i].Relationship = metas[i].relationship
		matches[i].ParentSessionID = metas[i].parentSessionID
		matches[i].Sidechain = anchors[i].Sidechain
		matches[i].Subordinate = anchors[i].Sidechain ||
			SubordinateSession(metas[i].relationship, metas[i].parentSessionID)
	}
	return nil
}

// buildContentUnitAnchors maps scanned sidecars to DeriveUnitRanges anchors.
// A missing anchor row keeps zero-valued classification fields, so it
// resolves to [o, o] with Sidechain false (session lineage still applies).
func buildContentUnitAnchors(
	matches []ContentMatch, metas []contentAnchorMeta,
) []UnitAnchor {
	anchors := make([]UnitAnchor, len(matches))
	for i, m := range matches {
		meta := metas[i]
		anchors[i] = UnitAnchor{
			SessionID:  m.SessionID,
			Ordinal:    m.Ordinal,
			Role:       meta.role.String,
			Sidechain:  meta.sidechain.Valid && meta.sidechain.Bool,
			Embeddable: meta.embeddable.Valid && meta.embeddable.Bool,
			Missing:    meta.missing,
		}
	}
	return anchors
}

// fillAnchorMeta fetches anchor classification and session lineage for every
// page row via lookupAnchorMeta. Refs whose message row does not exist
// (tool_result_events orphans) are marked missing so derivation falls back
// to [o, o]; their session lineage is still populated via the sessions join.
// The result aligns 1:1 with matches.
func (db *DB) fillAnchorMeta(
	ctx context.Context, matches []ContentMatch,
) ([]contentAnchorMeta, error) {
	refs := make([]semanticHitKey, len(matches))
	for i := range matches {
		refs[i] = semanticHitKey{matches[i].SessionID, matches[i].Ordinal}
	}
	found, err := db.lookupAnchorMeta(ctx, refs)
	if err != nil {
		return nil, err
	}
	metas := make([]contentAnchorMeta, len(matches))
	for i, ref := range refs {
		got, ok := found[ref]
		if !ok {
			metas[i].missing = true
			continue
		}
		got.missing = !got.role.Valid
		metas[i] = got
	}
	return metas, nil
}

// lookupAnchorMeta resolves anchor classification and session lineage for
// refs: one batched VALUES-CTE lookup per enrichHitsChunk distinct
// (session_id, ordinal) refs, never a per-row query. Refs whose session row
// is absent are omitted from the result map.
func (db *DB) lookupAnchorMeta(
	ctx context.Context, refs []semanticHitKey,
) (map[semanticHitKey]contentAnchorMeta, error) {
	seen := make(map[semanticHitKey]bool, len(refs))
	distinct := make([]semanticHitKey, 0, len(refs))
	for _, ref := range refs {
		if !seen[ref] {
			seen[ref] = true
			distinct = append(distinct, ref)
		}
	}
	found := make(map[semanticHitKey]contentAnchorMeta, len(distinct))
	for start := 0; start < len(distinct); start += enrichHitsChunk {
		chunk := distinct[start:min(start+enrichHitsChunk, len(distinct))]
		if err := db.lookupAnchorMetaChunk(ctx, chunk, found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// lookupAnchorMetaChunk resolves one chunk of (session_id, ordinal) refs to
// session lineage plus the anchor message row's classification columns:
// role, sidechain, and the embeddable flag (is_system = 0 AND content not
// system-prefixed — SystemPrefixSQL constrains only user rows, exactly like
// the embedding reducer's predicate). messages is LEFT JOINed so a ref whose
// message row is absent (tool_result_events orphan) still resolves lineage;
// its classification columns come back NULL.
func (db *DB) lookupAnchorMetaChunk(
	ctx context.Context, refs []semanticHitKey,
	out map[semanticHitKey]contentAnchorMeta,
) error {
	values := make([]string, len(refs))
	args := make([]any, 0, len(refs)*2)
	for i, r := range refs {
		values[i] = "(?, ?)"
		args = append(args, r.sessionID, r.ordinal)
	}
	query := "WITH refs(session_id, ordinal) AS (VALUES " +
		strings.Join(values, ", ") + ") " +
		"SELECT r.session_id, r.ordinal, " +
		"COALESCE(s.relationship_type,''), COALESCE(s.parent_session_id,''), " +
		"m.role, m.is_sidechain, " +
		"CASE WHEN m.is_system = 0 AND " +
		SystemPrefixSQL("m.content", "m.role") + " THEN 1 ELSE 0 END " +
		"FROM refs r " +
		"JOIN sessions s ON s.id = r.session_id " +
		"LEFT JOIN messages m ON m.session_id = r.session_id AND m.ordinal = r.ordinal"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("looking up match anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key semanticHitKey
		var meta contentAnchorMeta
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

// classifyUnitlessHybridHits assigns each unit-less hybrid FTS hit (the
// resolver returned no mirror unit; hits[i] for i in idxs) its structurally
// derived conversation-unit range and subordinate flag BEFORE scope filtering
// and fusion, so a unit-less sidechain (or subagent/fork-session) hit is
// excluded, included, and penalized exactly like lexical mode classifies the
// same anchor. One batched anchor lookup plus one DeriveUnitRanges pass
// covers all idxs — unit-less refs are rare (system rows or mirror lag), so
// the pre-merge leg stays cheap; mirror-unit rows keep their embedded span
// and resolver-provided subordinate flag untouched.
func (db *DB) classifyUnitlessHybridHits(
	ctx context.Context, hits []hybridDisplay, idxs []int,
) error {
	if len(idxs) == 0 {
		return nil
	}
	refs := make([]semanticHitKey, len(idxs))
	for k, i := range idxs {
		refs[k] = semanticHitKey{hits[i].sessionID, hits[i].ordinal}
	}
	metas, err := db.lookupAnchorMeta(ctx, refs)
	if err != nil {
		return err
	}
	anchors := make([]UnitAnchor, len(idxs))
	for k, ref := range refs {
		meta := metas[ref]
		anchors[k] = UnitAnchor{
			SessionID:  ref.sessionID,
			Ordinal:    ref.ordinal,
			Role:       meta.role.String,
			Sidechain:  meta.sidechain.Valid && meta.sidechain.Bool,
			Embeddable: meta.embeddable.Valid && meta.embeddable.Bool,
			Missing:    !meta.role.Valid,
		}
	}
	ranges, err := DeriveUnitRanges(ctx, db, anchors)
	if err != nil {
		return fmt.Errorf("deriving hybrid unit-less ranges: %w", err)
	}
	for k, i := range idxs {
		hits[i].ordinalStart, hits[i].ordinalEnd = ranges[k][0], ranges[k][1]
		meta := metas[refs[k]]
		hits[i].subordinate = anchors[k].Sidechain ||
			SubordinateSession(meta.relationship, meta.parentSessionID)
	}
	return nil
}
