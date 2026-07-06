package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type CursorAttribution struct {
	ScoredCommits        int64                     `json:"scored_commits"`
	LinesAdded           int64                     `json:"lines_added"`
	LinesDeleted         int64                     `json:"lines_deleted"`
	TabLinesAdded        int64                     `json:"tab_lines_added"`
	TabLinesDeleted      int64                     `json:"tab_lines_deleted"`
	ComposerLinesAdded   int64                     `json:"composer_lines_added"`
	ComposerLinesDeleted int64                     `json:"composer_lines_deleted"`
	HumanLinesAdded      int64                     `json:"human_lines_added"`
	HumanLinesDeleted    int64                     `json:"human_lines_deleted"`
	BlankLinesAdded      int64                     `json:"blank_lines_added"`
	BlankLinesDeleted    int64                     `json:"blank_lines_deleted"`
	AIAuthoredPct        float64                   `json:"ai_authored_pct"`
	ConversationCounts   []CursorConversationCount `json:"conversation_counts,omitempty"`
}

type CursorAttributionStatus string

const (
	CursorAttributionAvailable   CursorAttributionStatus = "available"
	CursorAttributionEmpty       CursorAttributionStatus = "empty"
	CursorAttributionUnavailable CursorAttributionStatus = "unavailable"
)

type CursorConversationCount struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
	Count int64  `json:"count"`
}

func LoadCursorAttribution(
	from, to time.Time,
) (*CursorAttribution, CursorAttributionStatus, error) {
	dbPath := cursorAttributionDBPath()
	if dbPath == "" {
		return nil, CursorAttributionUnavailable, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, CursorAttributionUnavailable, nil
		}
		return nil, "", fmt.Errorf("stat cursor attribution db: %w", err)
	}

	conn, err := openCursorAttributionDB(dbPath)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close()

	attr := &CursorAttribution{}
	if err := conn.QueryRow(
		`SELECT
			COUNT(*),
			COALESCE(SUM(linesAdded), 0),
			COALESCE(SUM(linesDeleted), 0),
			COALESCE(SUM(tabLinesAdded), 0),
			COALESCE(SUM(tabLinesDeleted), 0),
			COALESCE(SUM(composerLinesAdded), 0),
			COALESCE(SUM(composerLinesDeleted), 0),
			COALESCE(SUM(humanLinesAdded), 0),
			COALESCE(SUM(humanLinesDeleted), 0),
			COALESCE(SUM(blankLinesAdded), 0),
			COALESCE(SUM(blankLinesDeleted), 0)
		FROM scored_commits
		WHERE scoredAt >= ? AND scoredAt < ?`,
		timeToMillis(from), timeToMillis(to),
	).Scan(
		&attr.ScoredCommits,
		&attr.LinesAdded,
		&attr.LinesDeleted,
		&attr.TabLinesAdded,
		&attr.TabLinesDeleted,
		&attr.ComposerLinesAdded,
		&attr.ComposerLinesDeleted,
		&attr.HumanLinesAdded,
		&attr.HumanLinesDeleted,
		&attr.BlankLinesAdded,
		&attr.BlankLinesDeleted,
	); err != nil {
		return nil, "", fmt.Errorf("querying scored_commits: %w", err)
	}

	rows, err := conn.Query(
		`SELECT
			COALESCE(model, ''),
			COALESCE(mode, ''),
			COUNT(*)
		FROM conversation_summaries
		WHERE updatedAt >= ? AND updatedAt < ?
		GROUP BY COALESCE(model, ''), COALESCE(mode, '')
		ORDER BY COUNT(*) DESC, COALESCE(model, ''), COALESCE(mode, '')`,
		timeToMillis(from), timeToMillis(to),
	)
	if err != nil {
		return nil, "", fmt.Errorf(
			"querying conversation_summaries: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var entry CursorConversationCount
		if err := rows.Scan(
			&entry.Model, &entry.Mode, &entry.Count,
		); err != nil {
			return nil, "", fmt.Errorf(
				"scanning conversation_summaries: %w", err,
			)
		}
		attr.ConversationCounts = append(
			attr.ConversationCounts, entry,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf(
			"iterating conversation_summaries: %w", err,
		)
	}
	sortCursorConversationCounts(attr.ConversationCounts)

	if attr.ScoredCommits == 0 &&
		attr.LinesAdded == 0 &&
		attr.LinesDeleted == 0 &&
		len(attr.ConversationCounts) == 0 {
		return nil, CursorAttributionEmpty, nil
	}

	aiLines := attr.TabLinesAdded + attr.ComposerLinesAdded
	denom := attr.LinesAdded
	if denom > 0 {
		attr.AIAuthoredPct = float64(aiLines) / float64(denom)
	}

	return attr, CursorAttributionAvailable, nil
}

func cursorAttributionDBPath() string {
	if path := os.Getenv("AGENTSVIEW_CURSOR_ATTRIBUTION_DB"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cursor", "ai-tracking", "ai-code-tracking.db")
}

func openCursorAttributionDB(path string) (*sql.DB, error) {
	// The file: prefix is required for go-sqlite3 to honor mode=ro;
	// without it the query string is dropped and the live Cursor db
	// would be opened read-write.
	conn, err := sql.Open(
		"sqlite3",
		"file:"+sqliteURIPath(path)+"?mode=ro&_busy_timeout=3000",
	)
	if err != nil {
		return nil, fmt.Errorf("opening cursor attribution db: %w", err)
	}
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("opening cursor attribution db: %w", err)
	}
	return conn, nil
}

func timeToMillis(t time.Time) int64 {
	return t.UTC().UnixNano() / int64(time.Millisecond)
}

func sortCursorConversationCounts(
	counts []CursorConversationCount,
) {
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}
		if counts[i].Model != counts[j].Model {
			return counts[i].Model < counts[j].Model
		}
		return counts[i].Mode < counts[j].Mode
	})
}
