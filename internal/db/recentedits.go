package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// RecentEditsParams controls the cross-session recent-edits query.
type RecentEditsParams struct {
	Project         string // optional project filter ("" = all)
	Search          string // optional case-insensitive file_path substring ("" = all)
	Limit           int    // max file groups per page
	Offset          int    // file groups to skip
	MaxEditsPerFile int    // K: inline cap of recent edits per file
}

// RecentEdit is one Edit/Write tool call inlined under its file.
type RecentEdit struct {
	SessionID string `json:"session_id"`
	Ordinal   int    `json:"ordinal"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	CallIndex int    `json:"call_index"`
	ToolName  string `json:"tool_name"`
	Category  string `json:"category"`
	Timestamp string `json:"timestamp,omitempty"`
}

// RecentEditFile is one (project, file_path) group, newest edit first.
type RecentEditFile struct {
	Project        string       `json:"project"`
	FilePath       string       `json:"file_path"`
	EditCount      int          `json:"edit_count"`
	LastEditedAt   string       `json:"last_edited_at,omitempty"`
	LastSessionID  string       `json:"last_session_id"`
	Edits          []RecentEdit `json:"edits"`
	EditsTruncated bool         `json:"edits_truncated"`
}

// RecentEditsResult is one page of the recent-edits feed.
type RecentEditsResult struct {
	Files   []RecentEditFile `json:"files"`
	HasMore bool             `json:"has_more"`
}

// NormalizeRecentEditsParams clamps paging params; exported so PostgreSQL and
// DuckDB share identical clamping.
func NormalizeRecentEditsParams(p RecentEditsParams) RecentEditsParams {
	p.Search = strings.TrimSpace(p.Search)
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if p.MaxEditsPerFile <= 0 {
		p.MaxEditsPerFile = 20
	}
	return p
}

// RecentEdits returns files ordered by most-recent edit across all sessions,
// grouped by (project, file_path), with up to MaxEditsPerFile recent edits
// inlined per file. Trashed sessions are excluded.

// Placeholders bind in text order: project (CTE), search (CTE), LIMIT,
// OFFSET, then K.

// ScanRecentEdits groups the flat (file × edit) result rows into files,
// preserving row order, and applies has_more and per-file truncation. Shared
// by all three backends. Rows must be selected in this exact order: project,
// file_path, edit_count, last_edited_at, last_session_id, session_id, ordinal,
// tool_use_id, call_index, tool_name, category, timestamp.
func ScanRecentEdits(
	rows *sql.Rows, p RecentEditsParams,
) (RecentEditsResult, error) {
	p = NormalizeRecentEditsParams(p)
	type fileKey struct{ project, filePath string }
	// Non-nil from the start: the empty feed serializes as [] not null, and it
	// gives the analyzer a non-nil source for the files[i]/files[:Limit] ops.
	files := []RecentEditFile{}
	idx := map[fileKey]int{}
	for rows.Next() {
		var (
			project, filePath, lastSessionID string
			editCount, ordinal, callIndex    int
			sessionID, toolName, cat         string
			toolUseID                        sql.NullString
			lastEditedAt, timestamp          sql.NullString
		)
		if err := rows.Scan(&project, &filePath, &editCount, &lastEditedAt,
			&lastSessionID, &sessionID, &ordinal, &toolUseID, &callIndex,
			&toolName, &cat, &timestamp); err != nil {
			return RecentEditsResult{}, fmt.Errorf("scanning recent edit: %w", err)
		}
		key := fileKey{project, filePath}
		i, ok := idx[key]
		if !ok {
			files = append(files, RecentEditFile{
				Project:        project,
				FilePath:       filePath,
				EditCount:      editCount,
				LastEditedAt:   lastEditedAt.String,
				LastSessionID:  lastSessionID,
				Edits:          []RecentEdit{},
				EditsTruncated: editCount > p.MaxEditsPerFile,
			})
			i = len(files) - 1
			idx[key] = i
		}
		files[i].Edits = append(files[i].Edits, RecentEdit{
			SessionID: sessionID,
			Ordinal:   ordinal,
			ToolUseID: toolUseID.String,
			CallIndex: callIndex,
			ToolName:  toolName,
			Category:  cat,
			Timestamp: timestamp.String,
		})
	}
	if err := rows.Err(); err != nil {
		return RecentEditsResult{}, fmt.Errorf("iterating recent edits: %w", err)
	}
	// Files are appended in page order; the has_more probe file (limit+1) is
	// last, so truncating to Limit drops it and its inlined edits.
	hasMore := len(files) > p.Limit
	if hasMore {
		files = files[:p.Limit]
	}
	return RecentEditsResult{Files: files, HasMore: hasMore}, nil
}
