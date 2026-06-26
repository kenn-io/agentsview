package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// RecentEdits returns files ordered by most-recent edit across all sessions,
// grouped by (project, file_path), with up to MaxEditsPerFile recent edits
// inlined per file. Trashed sessions are excluded.
func (s *Store) RecentEdits(
	ctx context.Context, p db.RecentEditsParams,
) (db.RecentEditsResult, error) {
	p = db.NormalizeRecentEditsParams(p)
	q := `
WITH ranked AS (
  SELECT s.project AS project, tc.file_path AS file_path,
         tc.session_id AS session_id, tc.tool_name AS tool_name,
         tc.category AS category, tc.tool_use_id AS tool_use_id,
         tc.call_index AS call_index, m.ordinal AS ordinal,
         m.timestamp AS timestamp,
         ROW_NUMBER() OVER (
           PARTITION BY s.project, tc.file_path
           ORDER BY m.timestamp DESC NULLS LAST, tc.session_id DESC,
                    m.ordinal DESC, tc.call_index DESC) AS rn,
         COUNT(*) OVER (PARTITION BY s.project, tc.file_path) AS edit_count
  FROM tool_calls tc
  JOIN messages m ON m.session_id = tc.session_id
                 AND m.ordinal = tc.message_ordinal
  JOIN sessions s ON s.id = tc.session_id
  WHERE tc.category IN ('Edit','Write')
    AND tc.file_path IS NOT NULL AND tc.file_path <> ''
    AND s.deleted_at IS NULL
    %s
    %s
),
file_page AS (
  SELECT project, file_path, edit_count,
         timestamp AS last_edited_at, session_id AS last_session_id,
         ordinal AS last_ordinal, call_index AS last_call_index
  FROM ranked WHERE rn = 1
  ORDER BY last_edited_at DESC NULLS LAST, last_session_id DESC,
           last_ordinal DESC, last_call_index DESC, file_path DESC
  LIMIT %s OFFSET %s
)
SELECT fp.project, fp.file_path, fp.edit_count, fp.last_edited_at,
       fp.last_session_id, r.session_id, r.ordinal, r.tool_use_id,
       r.call_index, r.tool_name, r.category, r.timestamp
FROM file_page fp
JOIN ranked r ON r.project = fp.project AND r.file_path = fp.file_path
WHERE r.rn <= %s
ORDER BY fp.last_edited_at DESC NULLS LAST, fp.last_session_id DESC,
         fp.last_ordinal DESC, fp.last_call_index DESC, fp.file_path DESC,
         r.rn`

	// Build $-placeholders in order: [project?], [search?], limit+1, offset, K.
	args := []any{}
	n := 0
	next := func() string { n++; return fmt.Sprintf("$%d", n) }
	proj := ""
	if p.Project != "" {
		proj = "AND s.project = " + next()
		args = append(args, p.Project)
	}
	search := ""
	if p.Search != "" {
		search = `AND tc.file_path ILIKE ` + next() + ` ESCAPE E'\\'`
		args = append(args, "%"+db.EscapeLikePattern(p.Search)+"%")
	}
	limPH, offPH, kPH := next(), next(), next()
	args = append(args, p.Limit+1, p.Offset, p.MaxEditsPerFile)
	q = fmt.Sprintf(q, proj, search, limPH, offPH, kPH)
	rows, err := s.pg.QueryContext(ctx, q, args...)
	if err != nil {
		return db.RecentEditsResult{}, fmt.Errorf("pg recent edits: %w", err)
	}
	defer rows.Close()
	return db.ScanRecentEdits(rows, p)
}
