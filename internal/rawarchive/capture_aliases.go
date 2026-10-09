package rawarchive

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// recordRootAliases adds every other spelling of a provider root that the
// captured database uses for its transcripts. A spelling counts only when the
// spelled root directory itself resolves to the same canonical root while the
// source files still exist, such as a path through a symlinked ancestor.
func recordRootAliases(ctx context.Context, databasePath string, roots []RootSpec, canonical map[string]string) error {
	conn, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: databasePath}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, `SELECT DISTINCT agent, file_path FROM sessions
		WHERE agent IN ('claude','codex') AND coalesce(file_path,'') <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, stored string
		if err := rows.Scan(&agent, &stored); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(stored)
		if err != nil {
			continue
		}
		for i := range roots {
			root := &roots[i]
			base, ok := canonical[root.ID]
			if root.Provider != agent || !ok {
				continue
			}
			rel, err := containedPath(base, resolved)
			if err != nil {
				continue
			}
			alias, found := strings.CutSuffix(stored, string(filepath.Separator)+rel)
			if !found || alias == root.OriginalPath || slices.Contains(root.Aliases, alias) {
				continue
			}
			// One linked transcript does not make its directory equivalent;
			// the alias itself must resolve to the root.
			if resolvedAlias, err := filepath.EvalSymlinks(alias); err == nil && resolvedAlias == base {
				root.Aliases = append(root.Aliases, alias)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range roots {
		slices.Sort(roots[i].Aliases)
	}
	return nil
}
