package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// PreviewMigrateToolImages reports the inline image payloads that would be
// moved without writing any file or changing any row.
func (db *DB) PreviewMigrateToolImages(
	ctx context.Context, filter StripImagesFilter,
) (StripImagesReport, error) {
	return db.scanMigrateToolImages(ctx, filter, nil)
}

// MigrateToolImages moves retained inline tool-result image payloads into the
// asset store. put is called once per payload with the decoded bytes; it must
// write the file and return the asset:// reference. The caller owns the archive
// write lock before invoking this method.
func (db *DB) MigrateToolImages(
	ctx context.Context, filter StripImagesFilter, put imagePutFunc,
) (StripImagesReport, error) {
	if err := db.requireWritable(); err != nil {
		return StripImagesReport{}, err
	}
	return db.scanMigrateToolImages(ctx, filter, func(
		ctx context.Context, session stripImageSession,
	) (bool, error) {
		var stats ToolImageStats
		changed, err := db.migrateStoredToolResultRows(ctx, session.id, put, &stats)
		if err != nil {
			return false, fmt.Errorf(
				"migrating tool results for %s: %w", session.id, err,
			)
		}
		return changed, nil
	})
}

// migrateStoredToolResultRows rewrites one session's content columns through
// migrateToolResultImages, writing asset files before any UPDATE commits.
func (db *DB) migrateStoredToolResultRows(
	ctx context.Context, sessionID string, put imagePutFunc, stats *ToolImageStats,
) (bool, error) {
	return db.rewriteStoredToolResultRows(ctx, sessionID, func(content string) (string, error) {
		return migrateToolResultImages(content, put, stats)
	})
}

func (db *DB) scanMigrateToolImages(
	ctx context.Context,
	filter StripImagesFilter,
	apply func(context.Context, stripImageSession) (bool, error),
) (StripImagesReport, error) {
	sessions, err := db.stripImageSessions(ctx, filter)
	if err != nil {
		return StripImagesReport{}, err
	}
	report := StripImagesReport{
		Projects: make([]StripImagesProjectReport, 0),
	}
	byProject := make(map[string]*StripImagesProjectReport)
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		stats, err := db.migrateImageStats(ctx, session.id)
		if err != nil {
			return report, err
		}
		if stats.Payloads == 0 {
			continue
		}
		project := byProject[session.project]
		if project == nil {
			report.Projects = append(report.Projects, StripImagesProjectReport{
				Project: session.project,
			})
			project = &report.Projects[len(report.Projects)-1]
			byProject[session.project] = project
		}
		project.Sessions++
		report.Sessions++
		if apply != nil {
			changed, err := apply(ctx, session)
			if err != nil {
				// Undo the pre-increment for the failed session so the caller
				// receives only the work that committed successfully.
				project.Sessions--
				report.Sessions--
				if project.Sessions == 0 {
					// Sessions arrive grouped by project, so an emptied entry
					// is always the one just appended.
					report.Projects = report.Projects[:len(report.Projects)-1]
					delete(byProject, session.project)
				}
				return report, err
			}
			if changed {
				project.Changed++
				report.Changed++
			}
		} else if stats.Payloads > 0 {
			project.Changed++
			report.Changed++
		}
		project.Payloads += stats.Payloads
		project.StoredBytes += stats.StoredBytes
		project.DecodedBytes += stats.DecodedBytes
		report.Payloads += stats.Payloads
		report.StoredBytes += stats.StoredBytes
		report.DecodedBytes += stats.DecodedBytes
	}
	sort.Slice(report.Projects, func(i, j int) bool {
		return report.Projects[i].Project < report.Projects[j].Project
	})
	return report, nil
}

func (db *DB) migrateImageStats(
	ctx context.Context, sessionID string,
) (ToolImageStats, error) {
	var stats ToolImageStats
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT COALESCE(result_content, '')
		FROM tool_calls
		WHERE session_id = ?
		UNION ALL
		SELECT COALESCE(content, '')
		FROM tool_result_events
		WHERE session_id = ?`, sessionID, sessionID)
	if err != nil {
		return ToolImageStats{}, fmt.Errorf("reading tool result bytes for migrate stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return ToolImageStats{}, fmt.Errorf("scanning tool result bytes for migrate stats: %w", err)
		}
		countMigratable(content, &stats)
	}
	if err := rows.Err(); err != nil {
		return ToolImageStats{}, fmt.Errorf("iterating tool result bytes for migrate stats: %w", err)
	}
	return stats, nil
}

// countMigratable scans content for migratable inline image blocks and
// accumulates their byte counts into stats. It handles both array and
// labeled/anonymous summary forms.
func countMigratable(content string, stats *ToolImageStats) {
	// Try direct JSON array.
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(content), &blocks); err == nil && blocks != nil {
		countMigratableBlocks(blocks, stats)
		return
	}
	// Labeled/anonymous summary: count through the same scanner the rewrite
	// uses, so the preview cannot see a different set of sections.
	scanSummarySections(content, func(_, _ int, raw json.RawMessage) {
		var sectionBlocks []json.RawMessage
		if err := json.Unmarshal(raw, &sectionBlocks); err == nil {
			countMigratableBlocks(sectionBlocks, stats)
		}
	})
}

func countMigratableBlocks(blocks []json.RawMessage, stats *ToolImageStats) {
	for _, raw := range blocks {
		if !isMigratableToolImageBlock(raw) {
			continue
		}
		var block toolImageBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		_, decoded, stored, ok := decodeInlineImage(block.ImageURL)
		if !ok {
			continue
		}
		stats.Payloads++
		stats.StoredBytes += stored
		stats.DecodedBytes += int64(len(decoded))
	}
}
