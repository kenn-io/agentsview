package db

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// Stats holds database-wide statistics.
type Stats struct {
	SessionCount    int     `json:"session_count"`
	MessageCount    int     `json:"message_count"`
	ProjectCount    int     `json:"project_count"`
	MachineCount    int     `json:"machine_count"`
	EarliestSession *string `json:"earliest_session"`
}

// rootSessionFilter is the WHERE clause shared by session list
// and stats to exclude sub-agent, fork, and trashed sessions.
const rootSessionFilter = `message_count > 0
	AND relationship_type NOT IN ('subagent', 'fork')
	AND deleted_at IS NULL`

func nonSourceBackedAgentPlaceholders() string {
	agents := nonSourceBackedAgents()
	placeholders := make([]string, len(agents))
	for i := range agents {
		placeholders[i] = "?"
	}
	return strings.Join(placeholders, ", ")
}

func nonSourceBackedAgentArgs() []any {
	agents := nonSourceBackedAgents()
	args := make([]any, len(agents))
	for i, a := range agents {
		args[i] = string(a)
	}
	return args
}

func nonSourceBackedAgents() []parser.AgentType {
	var agents []parser.AgentType
	for _, def := range parser.Registry {
		if def.FileBased || def.Type == parser.AgentDevin {
			continue
		}
		agents = append(agents, def.Type)
	}
	return agents
}

// FileBackedSessionCount returns the number of root sessions protected by local
// resync discovery. This includes literal file-backed sessions plus Devin's
// provider-backed local CLI archive sessions.
func (db *DB) FileBackedSessionCount(
	ctx context.Context,
) (int, error) {
	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		 WHERE agent NOT IN (`+nonSourceBackedAgentPlaceholders()+`)
		 AND `+rootSessionFilter,
		nonSourceBackedAgentArgs()...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf(
			"counting file-backed sessions: %w", err,
		)
	}
	return count, nil
}

// GetStats returns database statistics, counting only root
// sessions with messages (matching the session list filter).
func (db *DB) GetStats(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) (Stats, error) {
	filter := rootSessionFilter
	if excludeOneShot {
		if !excludeAutomated {
			filter += " AND (user_message_count > 1 OR is_automated = 1)"
		} else {
			filter += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		filter += " AND is_automated = 0"
	}
	query := fmt.Sprintf(`
		SELECT
			(SELECT COUNT(*) FROM sessions
			 WHERE %s),
			(SELECT COALESCE(SUM(message_count), 0)
			 FROM sessions WHERE %s),
			(SELECT COUNT(DISTINCT project) FROM sessions
			 WHERE %s),
			(SELECT COUNT(DISTINCT machine) FROM sessions
			 WHERE %s),
			(SELECT MIN(COALESCE(
				NULLIF(started_at, ''), created_at
			 )) FROM sessions
			 WHERE %s)`,
		filter, filter, filter, filter, filter)

	var s Stats
	err := db.getReader().QueryRowContext(ctx, query).Scan(
		&s.SessionCount,
		&s.MessageCount,
		&s.ProjectCount,
		&s.MachineCount,
		&s.EarliestSession,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("fetching stats: %w", err)
	}
	return s, nil
}
