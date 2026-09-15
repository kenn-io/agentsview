package db

import (
	"context"
	"fmt"
	"strings"
)

// ListStarredSessionIDsForScope returns starred session IDs restricted to
// the given project scope (see the canonical Bun session filter's project/exclude
// semantics), sorted for deterministic output. Cost is bounded by the
// number of starred rows (one join lookup each), not archive size: callers
// needing a scope-filtered curation fingerprint should use this instead of
// filtering ListStarredSessionIDs's unscoped result against a
// separately-loaded session list.
func (db *DB) ListStarredSessionIDsForScope(
	ctx context.Context, projects, excludeProjects []string,
) ([]string, error) {
	where, args := curationScopeWhere("s", projects, excludeProjects)
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT ss.session_id FROM starred_sessions ss
		 JOIN sessions s ON s.id = ss.session_id`+where+
			` ORDER BY ss.session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing scoped starred sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning scoped starred session: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// curationScopeWhere builds a " WHERE ..." clause (with a leading space, ""
// when unfiltered) restricting alias.project to projects/excludeProjects.
// Shared by ListStarredSessionIDsForScope and ListPinnedSessionIDsForScope,
// which both join a curation table to sessions under the same alias
// convention to compute a project-scoped curation fingerprint.
func curationScopeWhere(alias string, projects, excludeProjects []string) (string, []any) {
	var args []any
	var where []string
	if len(projects) > 0 {
		placeholders := make([]string, len(projects))
		for i, p := range projects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, alias+".project IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(excludeProjects) > 0 {
		placeholders := make([]string, len(excludeProjects))
		for i, p := range excludeProjects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, alias+".project NOT IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}
