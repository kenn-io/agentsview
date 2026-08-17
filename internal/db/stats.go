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
	return db.fileBackedSessionCount(ctx, "", "", false)
}

// FileBackedSessionCountForMachine returns the protected root-session count
// for one sync source machine. Multi-source rebuilds use it so one healthy
// source cannot satisfy the empty-discovery guard for another source.
func (db *DB) FileBackedSessionCountForMachine(
	ctx context.Context, machine string,
) (int, error) {
	return db.fileBackedSessionCount(ctx, machine, "", true)
}

// FileBackedSessionCountForSource returns the protected root-session count
// for one namespaced rebuild contributor. ID prefixes distinguish a remote
// contributor from local sessions when both machines have the same hostname.
func (db *DB) FileBackedSessionCountForSource(
	ctx context.Context, machine, idPrefix string,
) (int, error) {
	return db.fileBackedSessionCount(ctx, machine, idPrefix, true)
}

// FileBackedSessionCountForRebuildOwner returns the protected root-session
// count owned by the local rebuild phase. Current, empty, and legacy local
// machine values cover archives created before source baselines; exact
// baseline rows preserve ownership after a structured root is relabeled.
// Contributor namespaces and agents whose source format can legitimately move
// between storage backends are excluded by the coordinator.
func (db *DB) FileBackedSessionCountForRebuildOwner(
	ctx context.Context,
	localMachine string,
	excludedIDPrefixes []string,
	excludedAgents []string,
) (int, error) {
	conditions := []string{`(
		machine = ? OR machine = '' OR machine = 'local' OR EXISTS (
			SELECT 1
			FROM local_session_source_baselines AS b
			WHERE b.session_id = sessions.id
			  AND b.machine = sessions.machine
			  AND b.agent = sessions.agent
			  AND b.file_path = sessions.file_path
		)
	)`}
	args := nonSourceBackedAgentArgs()
	args = append(args, localMachine)
	seenPrefixes := make(map[string]struct{}, len(excludedIDPrefixes))
	for _, prefix := range excludedIDPrefixes {
		if prefix == "" {
			continue
		}
		if _, seen := seenPrefixes[prefix]; seen {
			continue
		}
		seenPrefixes[prefix] = struct{}{}
		conditions = append(conditions, `substr(id, 1, length(?)) <> ?`)
		args = append(args, prefix, prefix)
	}
	seenAgents := make(map[string]struct{}, len(excludedAgents))
	for _, agent := range excludedAgents {
		if agent == "" {
			continue
		}
		if _, seen := seenAgents[agent]; seen {
			continue
		}
		seenAgents[agent] = struct{}{}
		conditions = append(conditions, `agent <> ?`)
		args = append(args, agent)
	}

	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		 WHERE agent NOT IN (`+nonSourceBackedAgentPlaceholders()+`)
		 AND `+rootSessionFilter+`
		 AND `+strings.Join(conditions, "\n AND "),
		args...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf(
			"counting local rebuild-owned file sessions: %w", err,
		)
	}
	return count, nil
}

func (db *DB) fileBackedSessionCount(
	ctx context.Context, machine, idPrefix string, scoped bool,
) (int, error) {
	machinePredicate := ""
	args := nonSourceBackedAgentArgs()
	if scoped {
		machinePredicate = " AND machine = ?"
		args = append(args, machine)
	}
	if idPrefix != "" {
		machinePredicate += " AND substr(id, 1, length(?)) = ?"
		args = append(args, idPrefix, idPrefix)
	}
	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		 WHERE agent NOT IN (`+nonSourceBackedAgentPlaceholders()+`)
		 AND `+rootSessionFilter+machinePredicate,
		args...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf(
			"counting file-backed sessions: %w", err,
		)
	}
	return count, nil
}
