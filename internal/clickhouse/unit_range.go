package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.UnitBoundsQuerier = (*Store)(nil)

const (
	unitSessionChunk    = 200
	unitExtentChunk     = 80
	unitAnchorMetaChunk = 200
)

func clickhouseEmbeddableUserSQL(alias string) string {
	return fmt.Sprintf("%[1]s.role = 'user' AND %[1]s.is_system = false AND %[2]s",
		alias, db.ClickHouseSystemPrefixSQL(alias+".content", alias+".role"))
}

func (s *Store) NearestUserBoundaries(
	ctx context.Context, probes []db.UnitProbe,
) ([]db.UnitBounds, error) {
	return db.ResolveUserBoundaries(ctx, probes, unitSessionChunk,
		s.scanUserBoundaryOrdinals)
}

func (s *Store) scanUserBoundaryOrdinals(
	ctx context.Context, sessions []string, out [][]int,
) error {
	parts := make([]string, len(sessions))
	args := make([]any, 0, len(sessions)*2)
	for i, sessionID := range sessions {
		parts[i] = "SELECT toInt64(?) AS idx, ? AS session_id"
		args = append(args, i, sessionID)
	}
	query := `
		WITH spans AS (` + strings.Join(parts, " UNION ALL ") + `)
		SELECT sp.idx, m.ordinal
		FROM spans sp JOIN messages m ON m.session_id = sp.session_id
		WHERE ` + clickhouseEmbeddableUserSQL("m")
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying clickhouse nearest user boundaries: %w", err)
	}
	defer rows.Close()
	return db.ScanUserBoundaryRows(rows, out)
}

func (s *Store) RunExtents(
	ctx context.Context, probes []db.ExtentProbe,
) ([][2]int, error) {
	return db.ResolveRunExtents(ctx, probes, unitExtentChunk, s.lookupRunExtentChunk)
}

type extentMsg struct {
	ordinal    int
	role       string
	isSystem   bool
	sidechain  bool
	embeddable bool
}

func (s *Store) lookupRunExtentChunk(
	ctx context.Context, probes []db.ExtentProbe, out [][2]int,
) error {
	seen := make(map[string]struct{}, len(probes))
	ids := make([]string, 0, len(probes))
	for _, p := range probes {
		if _, ok := seen[p.SessionID]; ok {
			continue
		}
		seen[p.SessionID] = struct{}{}
		ids = append(ids, p.SessionID)
	}
	bySession, err := s.loadExtentMessages(ctx, ids)
	if err != nil {
		return err
	}
	for i, p := range probes {
		out[i] = computeRunExtent(bySession[p.SessionID], p)
	}
	return nil
}

func (s *Store) loadExtentMessages(
	ctx context.Context, sessionIDs []string,
) (map[string][]extentMsg, error) {
	if len(sessionIDs) == 0 {
		return map[string][]extentMsg{}, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.queryContext(ctx, `
		SELECT session_id, ordinal, role, is_system, is_sidechain, content
		FROM messages
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY session_id, ordinal`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse run-extent messages: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]extentMsg, len(sessionIDs))
	for rows.Next() {
		var sessionID, role, content string
		var m extentMsg
		if err := rows.Scan(&sessionID, &m.ordinal, &role, &m.isSystem,
			&m.sidechain, &content); err != nil {
			return nil, fmt.Errorf("scanning clickhouse run-extent message: %w", err)
		}
		m.role = role
		m.embeddable = role == "user" && !m.isSystem && !db.IsSystemPrefixed(content, role)
		out[sessionID] = append(out[sessionID], m)
	}
	return out, rows.Err()
}

func computeRunExtent(msgs []extentMsg, p db.ExtentProbe) [2]int {
	isStop := func(m extentMsg) bool {
		if m.role == "assistant" && !m.isSystem && m.sidechain != p.Sidechain {
			return true
		}
		return m.embeddable
	}
	isMember := func(m extentMsg) bool {
		return m.role == "assistant" && !m.isSystem && m.sidechain == p.Sidechain
	}
	leftStop := p.Lo
	rightStop := p.Hi
	for _, m := range msgs {
		if m.ordinal > p.Lo && m.ordinal < p.Ordinal && isStop(m) {
			leftStop = m.ordinal
		}
	}
	for _, m := range msgs {
		if m.ordinal > p.Ordinal && m.ordinal < p.Hi && isStop(m) {
			rightStop = m.ordinal
			break
		}
	}
	first, last := p.Ordinal, p.Ordinal
	found := false
	for _, m := range msgs {
		if m.ordinal <= leftStop || m.ordinal > p.Ordinal {
			continue
		}
		if m.ordinal >= rightStop {
			break
		}
		if !isMember(m) {
			continue
		}
		if !found || m.ordinal < first {
			first = m.ordinal
		}
		if !found || m.ordinal > last {
			last = m.ordinal
		}
		found = true
	}
	for _, m := range msgs {
		if m.ordinal < p.Ordinal || m.ordinal >= rightStop {
			continue
		}
		if !isMember(m) {
			continue
		}
		if !found || m.ordinal < first {
			first = m.ordinal
		}
		if !found || m.ordinal > last {
			last = m.ordinal
		}
		found = true
	}
	return [2]int{first, last}
}

type anchorKey struct {
	sessionID string
	ordinal   int
}

type anchorMeta struct {
	relationship    string
	parentSessionID string
	role            sql.NullString
	sidechain       sql.NullBool
	embeddable      sql.NullBool
	missing         bool
}

func (s *Store) deriveLexicalUnits(
	ctx context.Context, matches []db.ContentMatch,
) error {
	if len(matches) == 0 {
		return nil
	}
	metas, err := s.fillAnchorMeta(ctx, matches)
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
		return fmt.Errorf("deriving clickhouse lexical units: %w", err)
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

func (s *Store) fillAnchorMeta(
	ctx context.Context, matches []db.ContentMatch,
) ([]anchorMeta, error) {
	seen := make(map[anchorKey]bool, len(matches))
	refs := make([]anchorKey, 0, len(matches))
	for i := range matches {
		key := anchorKey{matches[i].SessionID, matches[i].Ordinal}
		if !seen[key] {
			seen[key] = true
			refs = append(refs, key)
		}
	}
	found := make(map[anchorKey]anchorMeta, len(refs))
	for start := 0; start < len(refs); start += unitAnchorMetaChunk {
		chunk := refs[start:min(start+unitAnchorMetaChunk, len(refs))]
		if err := s.lookupAnchorMetaChunk(ctx, chunk, found); err != nil {
			return nil, err
		}
	}
	metas := make([]anchorMeta, len(matches))
	for i := range matches {
		got, ok := found[anchorKey{matches[i].SessionID, matches[i].Ordinal}]
		if !ok {
			metas[i].missing = true
			continue
		}
		got.missing = !got.role.Valid
		metas[i] = got
	}
	return metas, nil
}

func (s *Store) lookupAnchorMetaChunk(
	ctx context.Context, refs []anchorKey, out map[anchorKey]anchorMeta,
) error {
	parts := make([]string, len(refs))
	args := make([]any, 0, len(refs)*2)
	for i, r := range refs {
		parts[i] = "SELECT ? AS session_id, toInt64(?) AS ordinal"
		args = append(args, r.sessionID, r.ordinal)
	}
	query := `
		WITH refs AS (` + strings.Join(parts, " UNION ALL ") + `)
		SELECT r.session_id, r.ordinal,
			COALESCE(s.relationship_type, ''), COALESCE(s.parent_session_id, ''),
			m.role, m.is_sidechain,
			if(m.is_system = false AND ` +
		db.ClickHouseSystemPrefixSQL("m.content", "m.role") +
		`, true, false)
		FROM refs r
		JOIN sessions s ON s.id = r.session_id
		LEFT JOIN messages m ON m.session_id = r.session_id AND m.ordinal = r.ordinal`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("looking up clickhouse match anchors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key anchorKey
		var meta anchorMeta
		if err := rows.Scan(&key.sessionID, &key.ordinal,
			&meta.relationship, &meta.parentSessionID,
			&meta.role, &meta.sidechain, &meta.embeddable); err != nil {
			return fmt.Errorf("scanning clickhouse match anchor: %w", err)
		}
		out[key] = meta
	}
	return rows.Err()
}
