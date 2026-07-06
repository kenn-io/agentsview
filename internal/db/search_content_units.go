package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// contentAnchorMeta is the per-row sidecar the lexical scan reads alongside a
// ContentMatch: session lineage plus the anchor message row's classification
// columns. The three anchor columns are NULL for tool_result_events rows
// (those branches have no messages join); fillEventAnchorMeta backfills them
// post-scan, marking rows whose message row does not exist as missing.
type contentAnchorMeta struct {
	relationship    string
	parentSessionID string
	role            sql.NullString
	sidechain       sql.NullBool
	embeddable      sql.NullBool
	missing         bool
}

// lexicalAnchorColumns returns the five extra columns every lexical UNION
// branch selects for post-scan unit derivation: session lineage from the
// already-joined sessions alias s, plus the anchor message row's role,
// sidechain flag, and embeddable classification (is_system = 0 AND content
// not system-prefixed — SystemPrefixSQL constrains only user rows, exactly
// like the embedding reducer's predicate). msgAlias "" emits typed NULLs for
// the three anchor columns (tool_result_events branches, whose row
// cardinality must not change by joining messages).
func lexicalAnchorColumns(msgAlias string) string {
	lineage := "COALESCE(s.relationship_type,'') AS relationship, " +
		"COALESCE(s.parent_session_id,'') AS parent_session_id, "
	if msgAlias == "" {
		return lineage + "CAST(NULL AS TEXT) AS anchor_role, " +
			"CAST(NULL AS INTEGER) AS anchor_sidechain, " +
			"CAST(NULL AS INTEGER) AS anchor_embeddable"
	}
	return lineage + fmt.Sprintf(
		"%[1]s.role AS anchor_role, %[1]s.is_sidechain AS anchor_sidechain, "+
			"CASE WHEN %[1]s.is_system = 0 AND %[2]s THEN 1 ELSE 0 END AS anchor_embeddable",
		msgAlias, SystemPrefixSQL(msgAlias+".content", msgAlias+".role"))
}

// deriveLexicalUnits is the shared post-scan pass for the substring, regex,
// and fts modes: it backfills event-row anchors with one batched lookup,
// derives each match's conversation-unit range (DeriveUnitRanges memoizes per
// session, so a page of hits in one run costs a single probe round), and
// assigns OrdinalRange plus the lineage fields. metas aligns 1:1 with
// matches; both are the already-truncated page, so the pass is O(page).
func (db *DB) deriveLexicalUnits(
	ctx context.Context, matches []ContentMatch, metas []contentAnchorMeta,
) error {
	if len(matches) != len(metas) {
		return fmt.Errorf("deriving lexical units: %d matches, %d metas",
			len(matches), len(metas))
	}
	if len(matches) == 0 {
		return nil
	}
	if err := db.fillEventAnchorMeta(ctx, matches, metas); err != nil {
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

// fillEventAnchorMeta backfills anchor classification for rows scanned with
// NULL anchor columns (the tool_result_events branches): one batched
// VALUES-CTE lookup per enrichHitsChunk distinct (session_id, ordinal) refs,
// never a per-row query. Refs whose message row does not exist are marked
// missing so derivation falls back to [o, o].
func (db *DB) fillEventAnchorMeta(
	ctx context.Context, matches []ContentMatch, metas []contentAnchorMeta,
) error {
	var need []int
	seen := make(map[semanticHitKey]bool)
	var refs []semanticHitKey
	for i := range metas {
		if metas[i].role.Valid {
			continue
		}
		need = append(need, i)
		key := semanticHitKey{matches[i].SessionID, matches[i].Ordinal}
		if !seen[key] {
			seen[key] = true
			refs = append(refs, key)
		}
	}
	if len(need) == 0 {
		return nil
	}
	found := make(map[semanticHitKey]contentAnchorMeta, len(refs))
	for start := 0; start < len(refs); start += enrichHitsChunk {
		chunk := refs[start:min(start+enrichHitsChunk, len(refs))]
		if err := db.lookupAnchorMetaChunk(ctx, chunk, found); err != nil {
			return err
		}
	}
	for _, i := range need {
		got, ok := found[semanticHitKey{matches[i].SessionID, matches[i].Ordinal}]
		if !ok {
			metas[i].missing = true
			continue
		}
		metas[i].role = got.role
		metas[i].sidechain = got.sidechain
		metas[i].embeddable = got.embeddable
	}
	return nil
}

// lookupAnchorMetaChunk resolves one chunk of (session_id, ordinal) refs to
// their message rows' anchor-classification columns, using the same
// embeddable predicate lexicalAnchorColumns computes in the search SQL.
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
		"SELECT m.session_id, m.ordinal, m.role, m.is_sidechain, " +
		"CASE WHEN m.is_system = 0 AND " +
		SystemPrefixSQL("m.content", "m.role") + " THEN 1 ELSE 0 END " +
		"FROM refs r " +
		"JOIN messages m ON m.session_id = r.session_id AND m.ordinal = r.ordinal"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("looking up event anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key semanticHitKey
		var meta contentAnchorMeta
		if err := rows.Scan(&key.sessionID, &key.ordinal,
			&meta.role, &meta.sidechain, &meta.embeddable); err != nil {
			return fmt.Errorf("scanning event anchor: %w", err)
		}
		out[key] = meta
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating event anchors: %w", err)
	}
	return nil
}

// deriveUnitlessHybridRanges replaces the self-range on unit-less hybrid FTS
// rows (the resolver returned no mirror unit) with the structurally derived
// conversation-unit range, so lexical hits outside the mirror still cite
// their run. It reuses the classification enrichSemanticHits already fetched
// (role, sidechain, embeddable) — no extra per-row queries; mirror-unit rows
// keep their embedded span untouched, as do the fusion keys and the
// subordinate penalty applied at merge time.
func (db *DB) deriveUnitlessHybridRanges(
	ctx context.Context, displays []hybridDisplay,
	meta map[semanticHitKey]semanticHitInfo,
) error {
	var idxs []int
	var anchors []UnitAnchor
	for i, d := range displays {
		if !d.unitless {
			continue
		}
		a := UnitAnchor{SessionID: d.sessionID, Ordinal: d.ordinal, Missing: true}
		if info, ok := meta[semanticHitKey{d.sessionID, d.ordinal}]; ok {
			a.Role, a.Sidechain, a.Embeddable = info.role, info.isSidechain, info.embeddable
			a.Missing = false
		}
		idxs = append(idxs, i)
		anchors = append(anchors, a)
	}
	if len(anchors) == 0 {
		return nil
	}
	ranges, err := DeriveUnitRanges(ctx, db, anchors)
	if err != nil {
		return fmt.Errorf("deriving hybrid unit-less ranges: %w", err)
	}
	for k, i := range idxs {
		displays[i].ordinalStart, displays[i].ordinalEnd = ranges[k][0], ranges[k][1]
	}
	return nil
}
