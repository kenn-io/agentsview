package nanoclaw

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "github.com/mattn/go-sqlite3" // "sqlite3" driver for LoadAgentMap
)

// Agent is one NanoClaw agent group as the cell's v2.db describes it
// (nanoclaw.rs:67-77). Persona is agent_groups.name, Folder is
// agent_groups.folder, and Channel joins the names of the messaging groups
// the agent serves with ", " in query order; it is "" when there are none.
type Agent struct{ ID, Persona, Folder, Channel string }

// AgentMap is keyed by agent id, the directory name under v2-sessions.
type AgentMap map[string]Agent

// agentMapSQL is nanoclaw.rs:123-127 verbatim.
const agentMapSQL = `SELECT ag.id, ag.name, ag.folder, mg.name
FROM agent_groups ag
LEFT JOIN messaging_group_agents mga ON mga.agent_group_id = ag.id
LEFT JOIN messaging_groups mg ON mg.id = mga.messaging_group_id
ORDER BY ag.id, mga.priority DESC, mga.created_at`

// LoadAgentMap reads the agent map from a NanoClaw routing database opened
// read-only (port of query_agent_map, nanoclaw.rs:117-154). Any error,
// including a torn copy or schema drift, means "no map"; the Resolver
// decides whether that fails open or closed.
func LoadAgentMap(ctx context.Context, dbPath string) (AgentMap, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("nanoclaw agent map: %w", err)
	}
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(dbPath)+"?mode=ro&_busy_timeout=3000")
	if err != nil {
		return nil, fmt.Errorf("nanoclaw agent map: opening %s: %w", dbPath, err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, agentMapSQL)
	if err != nil {
		return nil, fmt.Errorf("nanoclaw agent map: querying %s: %w", dbPath, err)
	}
	defer rows.Close()

	agents := AgentMap{}
	groups := map[string][]string{}
	for rows.Next() {
		var id, persona, folder string
		var group sql.NullString
		if err := rows.Scan(&id, &persona, &folder, &group); err != nil {
			return nil, fmt.Errorf("nanoclaw agent map: scanning %s: %w", dbPath, err)
		}
		if _, seen := agents[id]; !seen {
			agents[id] = Agent{ID: id, Persona: persona, Folder: folder}
		}
		if group.Valid {
			groups[id] = append(groups[id], group.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nanoclaw agent map: reading %s: %w", dbPath, err)
	}
	for id, names := range groups {
		a := agents[id]
		a.Channel = strings.Join(names, ", ")
		agents[id] = a
	}
	return agents, nil
}

// sqliteURIPath escapes a filesystem path for a SQLite file: URI, as
// internal/parser/sqlite_dsn.go does for provider databases.
func sqliteURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

// Filter is the cell's trust tier: include and exclude lists matched
// against an agent's id, persona and folder (nanoclaw.rs:156-167).
type Filter struct{ Include, Exclude []string }

// Configured reports whether any list is set; then unmapped agents fail
// closed (nanoclaw.rs:183, :193-203).
func (f Filter) Configured() bool { return len(f.Include) > 0 || len(f.Exclude) > 0 }

// Allowed applies agent_allowed: exclude always wins, and a non-empty
// include admits only matching agents.
func (f Filter) Allowed(a Agent) bool {
	matches := func(needle string) bool {
		return needle == a.ID || needle == a.Persona || needle == a.Folder
	}
	if slices.ContainsFunc(f.Exclude, matches) {
		return false
	}
	if len(f.Include) > 0 && !slices.ContainsFunc(f.Include, matches) {
		return false
	}
	return true
}

// AgentIDFromPath returns the agent id when filePath is a cell transcript,
// <dataDir>/v2-sessions/<agent-id>/.claude-shared/projects/<…>/<file>, the
// layout jilog's discover globs (nanoclaw.rs:216-218). Anything else,
// including relative or remote-rewritten paths, is not a cell session.
func AgentIDFromPath(dataDir, filePath string) (string, bool) {
	if dataDir == "" || !filepath.IsAbs(filePath) {
		return "", false
	}
	root := filepath.Join(filepath.Clean(dataDir), "v2-sessions")
	rel, err := filepath.Rel(root, filepath.Clean(filePath))
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] != ".claude-shared" || parts[2] != "projects" {
		return "", false
	}
	return parts[0], true
}
