package rawarchive

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

var captureCountQueries = map[string]string{
	"stars":            "SELECT count(*) FROM starred_sessions",
	"pins":             "SELECT count(*) FROM pinned_messages",
	"renamed":          "SELECT count(*) FROM sessions WHERE coalesce(display_name,'')<>''",
	"trashed":          "SELECT count(*) FROM sessions WHERE deleted_at IS NOT NULL",
	"deleted":          "SELECT count(*) FROM excluded_sessions",
	"has_origin":       "SELECT count(*) FROM pg_sync_state WHERE key='artifact_origin_id'",
	"artifact_imports": "SELECT count(*) FROM artifact_imported_sessions",
	"qualified_ids":    "SELECT count(*) FROM sessions WHERE instr(id,'~')>0",
	"insights":         "SELECT count(*) FROM insights",
	"manual_projects":  "SELECT count(*) FROM session_project_assignments",
	"worktree_rules":   "SELECT count(*) FROM worktree_project_mappings",
}

func capturePreflight(ctx context.Context, path, device string) (CapturePreflight, error) {
	result := CapturePreflight{Counts: map[string]*int64{}, Unknown: map[string]string{}}
	file, err := inventoryFile(ctx, path, "application", "sessions.db", "sqlite-online-backup")
	if errors.Is(err, os.ErrNotExist) {
		for key := range captureCountQueries {
			result.Counts[key] = nil
			result.Unknown[key] = "database absent"
		}
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.DatabaseSHA256 = file.SHA256
	conn, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return result, err
	}
	defer conn.Close()
	for key, query := range captureCountQueries {
		var count int64
		if err := conn.QueryRowContext(ctx, query).Scan(&count); err != nil {
			result.Counts[key] = nil
			result.Unknown[key] = err.Error()
		} else {
			result.Counts[key] = new(count)
		}
	}
	if len(result.Unknown) == 0 {
		if err := result.readDeletions(ctx, conn, device); err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}

func (p *CapturePreflight) importError(seed bool) error {
	if p.DatabaseSHA256 == "" || len(p.Unknown) > 0 {
		return errors.New("capture preflight is unknown; inspect the captured database before importing")
	}
	counts := map[string]int64{}
	seen := map[string]bool{}
	for _, d := range p.Deletions {
		if d.ParserID == "" || strings.Contains(d.ParserID, "~") || seen[d.ParserID] || (d.Kind != "trashed" && d.Kind != "deleted") || (d.Kind == "trashed" && d.Provider == "") {
			return errors.New("invalid capture deletion evidence")
		}
		seen[d.ParserID] = true
		counts[d.Kind]++
	}
	for key := range captureCountQueries {
		count := p.Counts[key]
		if count == nil {
			return errors.New("capture preflight is incomplete")
		}
		switch key {
		case "has_origin", "artifact_imports", "qualified_ids":
			if *count != 0 {
				return errors.New("capture contains artifact evidence; artifact identity integration is required before import")
			}
		case "trashed", "deleted":
			if counts[key] != *count {
				return errors.New("capture deletion evidence is incomplete")
			}
		}
	}
	if !seed && p.DeletionAttributionError != "" {
		return errors.New(p.DeletionAttributionError)
	}

	return nil
}

func (p *CapturePreflight) readDeletions(ctx context.Context, conn *sql.DB, device string) error {
	rows, err := conn.QueryContext(ctx, `SELECT id,agent,'trashed',machine FROM sessions WHERE deleted_at IS NOT NULL
 UNION ALL SELECT id,'','deleted','' FROM excluded_sessions ORDER BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasPermanentDeletion := false
	for rows.Next() {
		var d db.RawArchiveSuppression
		var machine string
		if err := rows.Scan(&d.ParserID, &d.Provider, &d.Kind, &machine); err != nil {
			return err
		}
		p.Deletions = append(p.Deletions, d)
		hasPermanentDeletion = hasPermanentDeletion || d.Kind == "deleted"
		if d.Kind == "trashed" && machine != "" && machine != "local" && machine != device {
			var alias string
			err := conn.QueryRowContext(ctx, `SELECT value FROM pg_sync_state WHERE key=?`, db.MachineAliasKeyPrefix+machine).Scan(&alias)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if alias != device {
				p.DeletionAttributionError = "capture contains deletion evidence with unowned machine keys; explicitly adopt source ownership before collection"
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Permanent exclusions have no machine column. Their ownership is only
	// unambiguous when the source database contains no unexplained machines.
	if hasPermanentDeletion {
		var unowned int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sessions s WHERE machine NOT IN ('','local',?) AND NOT EXISTS(SELECT 1 FROM pg_sync_state p WHERE p.key=?||s.machine AND p.value=?)`, device, db.MachineAliasKeyPrefix, device).Scan(&unowned); err != nil {
			return err
		}
		if unowned > 0 {
			p.DeletionAttributionError = "capture contains permanent deletions with unowned machine keys; explicitly adopt source ownership before collection"
		}
	}
	return nil
}
