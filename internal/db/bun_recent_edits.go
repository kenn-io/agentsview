package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

type bunRecentEditProjection struct {
	Project         string              `bun:"project"`
	FilePath        string              `bun:"file_path"`
	EditCount       int                 `bun:"edit_count"`
	RawLastEditedAt any                 `bun:"last_edited_at"`
	LastEditedAt    *bunmodel.Timestamp `bun:"-"`
	LastSessionID   string              `bun:"last_session_id"`
	SessionID       string              `bun:"session_id"`
	Ordinal         int                 `bun:"ordinal"`
	ToolUseID       string              `bun:"tool_use_id"`
	CallIndex       int                 `bun:"call_index"`
	ToolName        string              `bun:"tool_name"`
	Category        string              `bun:"category"`
	RawTimestamp    any                 `bun:"timestamp"`
	Timestamp       *bunmodel.Timestamp `bun:"-"`
}

type bunRecentEditKey struct {
	Project  string `bun:"project"`
	FilePath string `bun:"file_path"`
}

func (s *BunStore) RecentEdits(
	ctx context.Context, params RecentEditsParams,
) (RecentEditsResult, error) {
	params = NormalizeRecentEditsParams(params)
	var result RecentEditsResult
	err := s.consistentView(ctx, func(store bun.IDB) error {
		attempt, err := s.recentEditsFrom(ctx, store, params)
		if err != nil {
			return err
		}
		result = attempt
		return nil
	})
	return result, err
}

func (s *BunStore) recentEditsFrom(
	ctx context.Context, store bun.IDB, params RecentEditsParams,
) (RecentEditsResult, error) {
	timestampExpr := "m.timestamp"
	sortExpr := s.backend.TimestampOrderExpr(timestampExpr)
	predicates := []string{
		"s.deleted_at IS NULL",
		"tc.category IN ('Edit', 'Write')",
		"tc.file_path IS NOT NULL",
		"TRIM(tc.file_path) != ''",
	}
	var args []any
	if params.Project != "" {
		predicates = append(predicates, "s.project = ?")
		args = append(args, params.Project)
	}
	queryOffset := params.Offset
	if params.Search != "" {
		if s.backend.Capabilities().SearchDialect.unicodeRecentEdits &&
			hasNonASCII(params.Search) {
			keys, err := s.unicodeRecentEditKeys(
				ctx, store, params, sortExpr,
			)
			if err != nil {
				return RecentEditsResult{}, err
			}
			if len(keys) == 0 {
				return RecentEditsResult{}, nil
			}
			keyPredicates := make([]string, len(keys))
			for i, key := range keys {
				keyPredicates[i] = "(s.project = ? AND tc.file_path = ?)"
				args = append(args, key.Project, key.FilePath)
			}
			predicates = append(predicates,
				"("+strings.Join(keyPredicates, " OR ")+")")
			queryOffset = 0
		} else {
			predicates = append(predicates,
				"LOWER(tc.file_path) LIKE ? ESCAPE '\\'")
			args = append(args,
				"%"+EscapeLikePattern(strings.ToLower(params.Search))+"%")
		}
	}
	query := fmt.Sprintf(`
		WITH edit_rows AS (
			SELECT s.project, tc.file_path,
				COUNT(*) OVER (
					PARTITION BY s.project, tc.file_path
				) AS edit_count,
				s.id AS session_id,
				tc.message_ordinal AS ordinal,
				tc.tool_use_id, tc.call_index, tc.tool_name, tc.category,
				%[1]s AS timestamp,
				%[2]s AS edit_sort,
				ROW_NUMBER() OVER (
					PARTITION BY s.project, tc.file_path
					ORDER BY %[2]s DESC NULLS LAST, s.id DESC,
						tc.message_ordinal DESC, tc.call_index DESC
				) AS edit_rank
			FROM tool_calls AS tc
			JOIN sessions AS s ON s.id = tc.session_id
			LEFT JOIN messages AS m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE %[3]s
		),
		paged_files AS (
			SELECT project, file_path, edit_count,
				timestamp AS last_edited_at,
				session_id AS last_session_id,
				ordinal AS last_ordinal,
				call_index AS last_call_index, edit_sort
			FROM edit_rows
			WHERE edit_rank = 1
			ORDER BY edit_sort DESC NULLS LAST, session_id DESC,
				ordinal DESC, call_index DESC, file_path DESC
			LIMIT ? OFFSET ?
		)
		SELECT page.project, page.file_path, page.edit_count,
			page.last_edited_at, page.last_session_id,
			edit.session_id, edit.ordinal, edit.tool_use_id,
			edit.call_index, edit.tool_name, edit.category, edit.timestamp
		FROM paged_files AS page
		JOIN edit_rows AS edit
			ON edit.project = page.project AND edit.file_path = page.file_path
		WHERE edit.edit_rank <= ?
		ORDER BY page.edit_sort DESC NULLS LAST, page.last_session_id DESC,
			page.last_ordinal DESC, page.last_call_index DESC,
			page.file_path DESC, edit.edit_rank ASC`,
		timestampExpr, sortExpr, strings.Join(predicates, " AND "),
	)
	args = append(args, params.Limit+1, queryOffset, params.MaxEditsPerFile)
	var rows []bunRecentEditProjection
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return RecentEditsResult{}, fmt.Errorf("querying Bun recent edits: %w", err)
	}
	for index := range rows {
		lastEditedAt, err := bunAvailableTimestamp(rows[index].RawLastEditedAt)
		if err != nil {
			return RecentEditsResult{}, fmt.Errorf(
				"scanning Bun recent-edit latest timestamp: %w", err,
			)
		}
		timestamp, err := bunAvailableTimestamp(rows[index].RawTimestamp)
		if err != nil {
			return RecentEditsResult{}, fmt.Errorf(
				"scanning Bun recent-edit timestamp: %w", err,
			)
		}
		rows[index].LastEditedAt = lastEditedAt
		rows[index].Timestamp = timestamp
	}
	return buildBunRecentEditPage(rows, params), nil
}

func hasNonASCII(value string) bool {
	for _, char := range value {
		if char > 127 {
			return true
		}
	}
	return false
}

// unicodeRecentEditKeys preserves the original Unicode-aware path search on
// SQLite, whose built-in LOWER only folds ASCII. It streams narrow, ordered
// keys, discards matches before Offset, and stops after the requested page plus
// lookahead; the main query still hydrates a bounded number of edit rows.
func (s *BunStore) unicodeRecentEditKeys(
	ctx context.Context, store bun.IDB, params RecentEditsParams,
	sortExpr string,
) ([]bunRecentEditKey, error) {
	predicates := []string{
		"s.deleted_at IS NULL",
		"tc.category IN ('Edit', 'Write')",
		"tc.file_path IS NOT NULL",
		"TRIM(tc.file_path) != ''",
	}
	var args []any
	if params.Project != "" {
		predicates = append(predicates, "s.project = ?")
		args = append(args, params.Project)
	}
	query := fmt.Sprintf(`
		WITH edit_keys AS (
			SELECT s.project, tc.file_path,
				%[1]s AS edit_sort,
				s.id AS session_id, tc.message_ordinal AS ordinal,
				tc.call_index,
				ROW_NUMBER() OVER (
					PARTITION BY s.project, tc.file_path
					ORDER BY %[2]s DESC NULLS LAST, s.id DESC,
						tc.message_ordinal DESC, tc.call_index DESC
				) AS edit_rank
			FROM tool_calls AS tc
			JOIN sessions AS s ON s.id = tc.session_id
			LEFT JOIN messages AS m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE %[3]s
		)
		SELECT project, file_path
		FROM edit_keys
		WHERE edit_rank = 1
		ORDER BY edit_sort DESC NULLS LAST, session_id DESC,
			ordinal DESC, call_index DESC, file_path DESC`,
		sortExpr, sortExpr, strings.Join(predicates, " AND "),
	)
	formatted := store.NewRaw(query, args...).String()
	rows, err := store.QueryContext(ctx, formatted)
	if err != nil {
		return nil, fmt.Errorf("querying Unicode recent-edit keys: %w", err)
	}
	defer rows.Close()
	needle := strings.ToLower(params.Search)
	matched := make([]bunRecentEditKey, 0, params.Limit+1)
	skipped := 0
	for rows.Next() && len(matched) < params.Limit+1 {
		var key bunRecentEditKey
		if err := rows.Scan(&key.Project, &key.FilePath); err != nil {
			return nil, fmt.Errorf("scanning Unicode recent-edit key: %w", err)
		}
		if !strings.Contains(strings.ToLower(key.FilePath), needle) {
			continue
		}
		if skipped < params.Offset {
			skipped++
			continue
		}
		matched = append(matched, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating Unicode recent-edit keys: %w", err)
	}
	return matched, nil
}

func buildBunRecentEditPage(
	rows []bunRecentEditProjection, params RecentEditsParams,
) RecentEditsResult {
	files := []RecentEditFile{}
	indices := make(map[string]int)
	for _, row := range rows {
		key := row.Project + "\x00" + row.FilePath
		index, ok := indices[key]
		if !ok {
			files = append(files, RecentEditFile{
				Project: row.Project, FilePath: row.FilePath,
				EditCount:     row.EditCount,
				LastEditedAt:  bunAnalyticsTimeString(row.LastEditedAt),
				LastSessionID: row.LastSessionID, Edits: []RecentEdit{},
				EditsTruncated: row.EditCount > params.MaxEditsPerFile,
			})
			index = len(files) - 1
			indices[key] = index
		}
		files[index].Edits = append(files[index].Edits, RecentEdit{
			SessionID: row.SessionID, Ordinal: row.Ordinal,
			ToolUseID: row.ToolUseID, CallIndex: row.CallIndex,
			ToolName: row.ToolName, Category: row.Category,
			Timestamp: bunAnalyticsTimeString(row.Timestamp),
		})
	}
	hasMore := len(files) > params.Limit
	if hasMore {
		files = files[:params.Limit]
	}
	return RecentEditsResult{Files: files, HasMore: hasMore}
}
