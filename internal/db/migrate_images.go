package db

import (
	"context"
	"encoding/json"
	"fmt"
)

// PreviewMigrateToolImages reports the inline image payloads that would be
// moved without writing any file or changing any row.
func (db *DB) PreviewMigrateToolImages(
	ctx context.Context, filter StripImagesFilter,
) (StripImagesReport, error) {
	return db.scanToolImages(ctx, filter, countMigratable, nil)
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
	return db.scanToolImages(ctx, filter, countMigratable, func(
		ctx context.Context, session stripImageSession,
	) (bool, error) {
		changed, err := db.migrateStoredToolResultRows(ctx, session.id, put)
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
	ctx context.Context, sessionID string, put imagePutFunc,
) (bool, error) {
	return db.rewriteStoredToolResultRows(ctx, sessionID, func(content string) (string, error) {
		return migrateToolResultImages(content, put)
	})
}

// countMigratable scans content for migratable inline image blocks and
// accumulates their byte counts into stats. It handles both array and
// labeled/anonymous summary forms.
func countMigratable(content string) ToolImageStats {
	var stats ToolImageStats
	// Try direct JSON array.
	var blocks []json.RawMessage
	if err := json.Unmarshal([]byte(content), &blocks); err == nil && blocks != nil {
		countMigratableBlocks(blocks, &stats)
		return stats
	}
	// Labeled/anonymous summary: count through the same scanner the rewrite
	// uses, so the preview cannot see a different set of sections.
	scanSummarySections(content, func(_, _ int, raw json.RawMessage) {
		var sectionBlocks []json.RawMessage
		if err := json.Unmarshal(raw, &sectionBlocks); err == nil {
			countMigratableBlocks(sectionBlocks, &stats)
		}
	})
	return stats
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
		_, decoded, stored, ok := decodeInlineImageURL(block.ImageURL)
		if !ok {
			continue
		}
		stats.Payloads++
		stats.StoredBytes += stored
		stats.DecodedBytes += decoded
	}
}
