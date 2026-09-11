package postgres

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Effective pin selection uses the same precedence as selectRawEffectivePins:
// visible eligible cohort members, explicit branch values before group defaults,
// any true member pins the cohort, first lexical explicit member supplies note.
// False overrides suppress their own inherited value before page selection.
const rawEffectivePinCandidatesSQL = `WITH eligible AS (
 SELECT b.branch_id,b.group_id,b.session_id,
 COALESCE((SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id=b.branch_id AND c.field='trashed'),(SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id='' AND c.field='trashed'),false) trashed
 FROM raw_session_branches b JOIN sessions s ON s.id=b.session_id
 WHERE b.active AND ($1='' OR s.id=$1) AND ($1<>'' OR s.deleted_at IS NULL) AND ($2='' OR s.project=$2)
 AND NOT COALESCE((SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id=b.branch_id AND c.field='excluded'),(SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id='' AND c.field='excluded'),false)
), members AS (
 SELECT b.* FROM eligible b WHERE NOT trashed OR NOT EXISTS(SELECT 1 FROM eligible v WHERE v.session_id=b.session_id AND NOT v.trashed)
), effective AS (
 SELECT DISTINCT ON(b.session_id,p.message_key) b.session_id,p.message_key,p.ordinal,p.note,p.created_at
 FROM members b JOIN raw_pins p ON p.group_id=b.group_id AND (p.branch_id=b.branch_id OR p.branch_id='')
 WHERE p.pinned AND (p.branch_id<>'' OR NOT EXISTS(SELECT 1 FROM raw_pins x WHERE x.group_id=b.group_id AND x.branch_id=b.branch_id AND x.message_key=p.message_key))
 ORDER BY b.session_id,p.message_key,(p.branch_id<>'') DESC,b.branch_id
)`

// pinReferencePage limits effective references before fetching any transcript.
// Legacy and raw references share the same deterministic page, so unresolved
// pins compete by durable creation time, not by transient materialization IDs.
func (s *RawProjectionStore) pinReferencePage(ctx context.Context, id, project string) ([]db.PinnedMessage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, rawEffectivePinCandidatesSQL+`, candidates AS (
 SELECT session_id,message_key,ordinal,note,created_at,0::bigint legacy_id FROM effective
 UNION ALL
 SELECT p.session_id,''::text,p.ordinal,p.note,p.created_at,p.id FROM pinned_messages p JOIN sessions s ON s.id=p.session_id
 WHERE s.provenance_kind='legacy' AND ($1='' OR s.id=$1) AND ($1<>'' OR s.deleted_at IS NULL) AND ($2='' OR s.project=$2)
 ), page AS MATERIALIZED (
 SELECT * FROM candidates ORDER BY created_at DESC,session_id,message_key,legacy_id DESC LIMIT CASE WHEN $1='' THEN 500 ELSE NULL END
 )
 SELECT p.session_id,p.message_key,p.ordinal,p.note,p.created_at,COALESCE(pm.id,0),s.project,s.agent,COALESCE(s.display_name,s.session_name),s.first_message,m.content,m.role
 FROM page p JOIN sessions s ON s.id=p.session_id
 LEFT JOIN pinned_messages pm ON pm.session_id=p.session_id AND pm.ordinal=p.ordinal
 LEFT JOIN messages m ON m.session_id=p.session_id AND m.ordinal=p.ordinal
 ORDER BY p.created_at DESC,p.session_id,p.message_key,p.legacy_id DESC`, id, project)
	if err != nil {
		return nil, err
	}
	var pins []db.PinnedMessage
	var rawIDs []string
	for rows.Next() {
		var p db.PinnedMessage
		var created time.Time
		if err = rows.Scan(&p.SessionID, &p.MessageKey, &p.Ordinal, &p.Note, &created, &p.ID, &p.SessionProject, &p.SessionAgent, &p.SessionDisplayName, &p.SessionFirstMessage, &p.Content, &p.Role); err != nil {
			rows.Close()
			return nil, err
		}
		p.CreatedAt = FormatISO8601(created)
		p.MessageID = int64(p.Ordinal)
		if p.MessageKey != "" {
			rawIDs = append(rawIDs, p.SessionID)
			p.Unresolved = true
		}
		pins = append(pins, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	slices.Sort(rawIDs)
	rawIDs = slices.Compact(rawIDs)
	if len(rawIDs) > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT session_id,payload FROM raw_content_revisions WHERE session_id=ANY($1)`, rawIDs)
		if err != nil {
			return nil, err
		}
		current := map[string]map[int]string{}
		wanted := map[string]map[int]bool{}
		for _, p := range pins {
			if p.MessageKey != "" {
				if wanted[p.SessionID] == nil {
					wanted[p.SessionID] = map[int]bool{}
				}
				wanted[p.SessionID][p.Ordinal] = true
			}
		}
		for rows.Next() {
			var session string
			var data []byte
			if err = rows.Scan(&session, &data); err != nil {
				rows.Close()
				return nil, err
			}
			payload, e := decodeRawPayload(data)
			if e != nil {
				rows.Close()
				return nil, e
			}
			keys := map[int]string{}
			for _, m := range payload.Messages {
				if !wanted[session][m.Ordinal] {
					continue
				}
				key, e := rawMessageKey(m)
				if e != nil {
					rows.Close()
					return nil, e
				}
				keys[m.Ordinal] = key
			}
			current[session] = keys
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for i := range pins {
			p := &pins[i]
			if p.MessageKey != "" {
				p.Unresolved = current[p.SessionID][p.Ordinal] != p.MessageKey
				if p.Unresolved {
					p.ID = 0
					p.MessageID = 0
					p.Content = nil
					p.Role = nil
				}
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return pins, nil
}
