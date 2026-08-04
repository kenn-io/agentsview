package db

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func (s *BunStore) RecentEdits(
	ctx context.Context, p RecentEditsParams,
) (RecentEditsResult, error) {
	p = NormalizeRecentEditsParams(p)
	var result RecentEditsResult
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var sessions []bunmodel.Session
		query := store.NewSelect().Model(&sessions).Where("deleted_at IS NULL")
		if p.Project != "" {
			query = query.Where("project = ?", p.Project)
		}
		if err := query.Scan(ctx); err != nil {
			return fmt.Errorf("querying Bun recent-edit sessions: %w", err)
		}
		ids := bunAnalyticsSessionIDs(sessions)
		messages, err := bunAnalyticsMessagesFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		tools, err := bunAnalyticsToolsFrom(ctx, store, ids)
		if err != nil {
			return err
		}
		result = buildBunRecentEdits(sessions, messages, tools, p)
		return nil
	})
	return result, err
}

type bunRecentEditRow struct {
	project  string
	filePath string
	edit     RecentEdit
}

func buildBunRecentEdits(
	sessions []bunmodel.Session,
	messages []bunmodel.Message,
	tools []bunmodel.ToolCall,
	p RecentEditsParams,
) RecentEditsResult {
	projects := make(map[string]string, len(sessions))
	messageTimes := map[string]string{}
	for _, session := range sessions {
		projects[session.ID] = session.Project
	}
	for _, message := range messages {
		messageTimes[fmt.Sprintf("%s\x00%d", message.SessionID, message.Ordinal)] =
			bunAnalyticsTimeString(message.Timestamp)
	}
	search := strings.ToLower(p.Search)
	groups := map[string][]bunRecentEditRow{}
	for _, tool := range tools {
		if tool.Category != "Edit" && tool.Category != "Write" {
			continue
		}
		if tool.FilePath == nil || strings.TrimSpace(*tool.FilePath) == "" {
			continue
		}
		project, ok := projects[tool.SessionID]
		if !ok || (search != "" &&
			!strings.Contains(strings.ToLower(*tool.FilePath), search)) {
			continue
		}
		row := bunRecentEditRow{
			project: project, filePath: *tool.FilePath,
			edit: RecentEdit{
				SessionID: tool.SessionID, Ordinal: tool.MessageOrdinal,
				ToolUseID: tool.ToolUseID, CallIndex: tool.CallIndex,
				ToolName: tool.ToolName, Category: tool.Category,
				Timestamp: messageTimes[fmt.Sprintf("%s\x00%d", tool.SessionID, tool.MessageOrdinal)],
			},
		}
		key := project + "\x00" + *tool.FilePath
		groups[key] = append(groups[key], row)
	}
	var files []RecentEditFile
	for _, rows := range groups {
		sort.SliceStable(rows, func(i, j int) bool {
			return bunRecentEditLess(rows[i].edit, rows[j].edit)
		})
		first := rows[0]
		file := RecentEditFile{
			Project: first.project, FilePath: first.filePath,
			EditCount: len(rows), LastEditedAt: first.edit.Timestamp,
			LastSessionID: first.edit.SessionID, Edits: []RecentEdit{},
			EditsTruncated: len(rows) > p.MaxEditsPerFile,
		}
		for i := 0; i < min(len(rows), p.MaxEditsPerFile); i++ {
			file.Edits = append(file.Edits, rows[i].edit)
		}
		files = append(files, file)
	}
	sort.SliceStable(files, func(i, j int) bool {
		left := RecentEdit{Timestamp: files[i].LastEditedAt,
			SessionID: files[i].LastSessionID,
			Ordinal:   files[i].Edits[0].Ordinal,
			CallIndex: files[i].Edits[0].CallIndex}
		right := RecentEdit{Timestamp: files[j].LastEditedAt,
			SessionID: files[j].LastSessionID,
			Ordinal:   files[j].Edits[0].Ordinal,
			CallIndex: files[j].Edits[0].CallIndex}
		if bunRecentEditLess(left, right) {
			return true
		}
		if bunRecentEditLess(right, left) {
			return false
		}
		return files[i].FilePath > files[j].FilePath
	})
	if p.Offset >= len(files) {
		return RecentEditsResult{Files: []RecentEditFile{}}
	}
	files = files[p.Offset:]
	hasMore := len(files) > p.Limit
	if hasMore {
		files = files[:p.Limit]
	}
	return RecentEditsResult{Files: files, HasMore: hasMore}
}

func bunRecentEditLess(left, right RecentEdit) bool {
	if left.Timestamp != right.Timestamp {
		return timestampAfter(left.Timestamp, right.Timestamp)
	}
	if left.SessionID != right.SessionID {
		return left.SessionID > right.SessionID
	}
	if left.Ordinal != right.Ordinal {
		return left.Ordinal > right.Ordinal
	}
	return left.CallIndex > right.CallIndex
}
