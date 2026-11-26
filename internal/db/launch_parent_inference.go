package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

var launchAgentWords = map[string]*regexp.Regexp{
	"codex":  regexp.MustCompile(`(?i)(^|[^a-z0-9_])codex([^a-z0-9_]|$)`),
	"claude": regexp.MustCompile(`(?i)(^|[^a-z0-9_])claude([^a-z0-9_]|$)`),
}

func launchFolder(s string) string {
	s = strings.ReplaceAll(s, `\`, "/")
	windows := strings.HasPrefix(s, "//") || len(s) >= 2 && s[1] == ':'
	s = path.Clean(s)
	if windows {
		s = strings.ToLower(s)
	}
	return s
}

type launchChild struct {
	id, agent, cwd, machine, started string
	qualified                        bool
}

func inferLaunchParentsTx(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, agent, cwd, machine, COALESCE(started_at, ''),
 deleted_at IS NULL AND ((agent = 'codex' AND session_kind = 'non-interactive') OR (agent = 'claude' AND entrypoint = 'sdk-cli')) FROM sessions s
 WHERE id IN (SELECT value FROM json_each(?)) AND (EXISTS (SELECT 1 FROM session_external_parents ep WHERE ep.session_id=s.id AND ep.inferred=1) OR (deleted_at IS NULL AND ((agent = 'codex' AND session_kind = 'non-interactive') OR (agent = 'claude' AND entrypoint = 'sdk-cli'))))`, string(encoded))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var children []launchChild
	for rows.Next() {
		var c launchChild
		if err := rows.Scan(&c.id, &c.agent, &c.cwd, &c.machine, &c.started, &c.qualified); err != nil {
			rows.Close()
			return 0, err
		}
		children = append(children, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, child := range children {
		n, err := inferLaunchChild(ctx, tx, child)
		if err != nil {
			return 0, err
		}
		changed += n
	}
	return changed, nil
}

// Starting with the bounded timestamp range prevents a scan of every session
// on the machine for each candidate child.
const launchCandidateSQL = `SELECT s.id, s.cwd, s.machine, m.timestamp, tc.tool_name, tc.input_json, s.deleted_at IS NOT NULL FROM messages m INDEXED BY idx_messages_activity_timestamp
 CROSS JOIN tool_calls tc ON tc.message_id=m.id CROSS JOIN sessions s ON s.id=m.session_id
 WHERE m.role='assistant' AND m.model != '<synthetic>' AND m.timestamp >= ? AND m.timestamp <= ? AND (s.machine=? OR TRIM(s.machine)='') AND s.id != ? AND s.agent IN ('claude','codex') AND tc.category='Bash'`

func inferLaunchChild(ctx context.Context, tx *sql.Tx, child launchChild) (int, error) {
	start, err := time.Parse(time.RFC3339Nano, child.started)
	if err != nil || !child.qualified || !absoluteLaunchFolder(child.cwd) || strings.TrimSpace(child.machine) == "" {
		return retractInferredLaunchParent(ctx, tx, child.id)
	}
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(parser_parent_session_id,'') = '' AND NOT `+spawnEdgeExistsSQL("s")+` AND NOT EXISTS (SELECT 1 FROM session_parent_inference_blocks b WHERE b.session_id=s.id) AND NOT EXISTS (SELECT 1 FROM session_external_parents e WHERE e.session_id=s.id AND e.inferred=0) FROM sessions s WHERE id=?`, child.id).Scan(&eligible)
	if err != nil {
		return 0, err
	}
	if !eligible {
		return retractInferredLaunchParent(ctx, tx, child.id)
	}
	rows, err := tx.QueryContext(ctx, launchCandidateSQL, start.Add(-62*time.Second).UTC().Format(time.RFC3339Nano), start.Add(2*time.Second).UTC().Format(time.RFC3339Nano), child.machine, child.id)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	matches := map[string]bool{}
	known := map[string]int{}
	competing := map[string]map[string]bool{}
	for rows.Next() {
		var id, cwd, machine, stamp, tool string
		var input sql.NullString
		var trashed bool
		if err := rows.Scan(&id, &cwd, &machine, &stamp, &tool, &input, &trashed); err != nil {
			rows.Close()
			return 0, err
		}
		if pollTool(tool) {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, stamp)
		if e != nil || at.After(start) || start.Sub(at) > 60*time.Second {
			continue
		}
		if !input.Valid || strings.TrimSpace(input.String) == "" {
			matches[id] = true
			continue
		}
		calls, undecodable := shellCalls(input.String)
		if undecodable {
			matches[id] = true
			continue
		}
		for _, call := range calls {
			if call.unknown {
				if re := launchAgentWords[child.agent]; re != nil && re.MatchString(call.command) {
					matches[id] = true
				}
				continue
			}
			dir := cwd
			if call.unknownWorkdir {
				dir = ""
			} else if call.workdir != "" {
				dir = parser.ShellWorkdir(dir, call.workdir)
			}
			if !absoluteLaunchFolder(dir) {
				dir = ""
			}
			for _, launch := range parser.LaunchesFromCommand(call.command, dir) {
				if launch.Agent != child.agent {
					continue
				}
				matched := false
				for _, folder := range launch.Dirs {
					if absoluteLaunchFolder(folder) && launchFolder(folder) == launchFolder(child.cwd) {
						matched = true
						break
					}
				}
				if !launch.Unknown && !matched {
					continue
				}
				matches[id] = true
				if !trashed && matched && !launch.Unknown && !launch.Possible && !call.possible && len(launch.Dirs) == 1 && strings.TrimSpace(machine) != "" {
					runs, err := launchRuns(ctx, tx, child, at)
					if err != nil {
						rows.Close()
						return 0, err
					}
					known[id]++
					if competing[id] == nil {
						competing[id] = map[string]bool{}
					}
					for _, run := range runs {
						competing[id][run] = true
					}
				}
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(matches) == 1 && len(known) == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET parser_relationship_type = relationship_type WHERE id = ? AND parser_relationship_type IS NULL`, child.id); err != nil {
			return 0, err
		}
		for id := range matches {
			if known[id] < len(competing[id]) {
				return retractInferredLaunchParent(ctx, tx, child.id)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO session_external_parents(session_id,parent_session_id,inferred) VALUES(?,?,1) ON CONFLICT(session_id) DO UPDATE SET parent_session_id=excluded.parent_session_id, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE inferred=1 AND parent_session_id != excluded.parent_session_id`, child.id, id)
			if err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	return retractInferredLaunchParent(ctx, tx, child.id)
}

// launchRuns finds the runs that could have used an invocation.
func launchRuns(ctx context.Context, tx *sql.Tx, child launchChild, at time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,cwd,started_at FROM sessions INDEXED BY idx_sessions_started WHERE started_at>=? AND started_at<=? AND machine=? AND agent=? AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, at.Add(-2*time.Second).UTC().Format(time.RFC3339Nano), at.Add(62*time.Second).UTC().Format(time.RFC3339Nano), child.machine, child.agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var id, cwd, stamp string
		if err := rows.Scan(&id, &cwd, &stamp); err != nil {
			return nil, err
		}
		started, err := time.Parse(time.RFC3339Nano, stamp)
		if err == nil && !started.Before(at) && started.Sub(at) <= 60*time.Second && absoluteLaunchFolder(cwd) && launchFolder(cwd) == launchFolder(child.cwd) {
			runs = append(runs, id)
		}
	}
	return runs, rows.Err()
}

func absoluteLaunchFolder(dir string) bool {
	dir = strings.ReplaceAll(dir, `\`, "/")
	return path.IsAbs(dir) || len(dir) >= 3 && dir[1] == ':' && dir[2] == '/'
}

func retractInferredLaunchParent(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	var old string
	err := tx.QueryRowContext(ctx, `SELECT parent_session_id FROM session_external_parents WHERE session_id=? AND inferred=1`, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM session_external_parents WHERE session_id=? AND inferred=1`, id); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET parent_session_id=NULL, relationship_type=`+parserRelationshipSQL+`, friction_rules_version='', sync_marker=MAX(COALESCE(sync_marker,''),strftime('%Y-%m-%dT%H:%M:%fZ','now')) WHERE id=? AND parent_session_id=? AND relationship_type='subagent' AND COALESCE(parser_parent_session_id,'')='' AND NOT `+spawnEdgeExistsSQL("sessions"), id, old)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// launchSeeds adds runs near calls in changed launchers, so late evidence can
// add or retract a match.
func launchSeeds(ctx context.Context, tx *sql.Tx, ids []string) ([]string, error) {
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.machine, m.timestamp, tc.tool_name, tc.input_json FROM sessions s JOIN tool_calls tc ON tc.session_id=s.id JOIN messages m ON m.id=tc.message_id WHERE s.id IN (SELECT value FROM json_each(?)) AND s.agent IN ('claude','codex') AND tc.category='Bash' AND m.timestamp IS NOT NULL`, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type call struct {
		machine, stamp, tool string
		input                sql.NullString
	}
	var calls []call
	for rows.Next() {
		var c call
		if err := rows.Scan(&c.machine, &c.stamp, &c.tool, &c.input); err != nil {
			rows.Close()
			return nil, err
		}
		calls = append(calls, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	seeds := append([]string{}, ids...)
	for _, c := range calls {
		if pollTool(c.tool) || !launchInputMayMatter(c.input) {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, c.stamp)
		if e != nil {
			continue
		}
		r, e := tx.QueryContext(ctx, `SELECT id FROM sessions INDEXED BY idx_sessions_started WHERE (?='' OR machine=?) AND started_at>=? AND started_at<=? AND deleted_at IS NULL AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, strings.TrimSpace(c.machine), c.machine, at.Add(-2*time.Second).UTC().Format(time.RFC3339Nano), at.Add(62*time.Second).UTC().Format(time.RFC3339Nano))
		if e != nil {
			return nil, e
		}
		defer r.Close()
		for r.Next() {
			var id string
			if e = r.Scan(&id); e != nil {
				r.Close()
				return nil, e
			}
			seeds = append(seeds, id)
		}
		e = r.Err()
		r.Close()
		if e != nil {
			return nil, e
		}
	}
	childRows, err := tx.QueryContext(ctx, `SELECT machine,started_at FROM sessions WHERE id IN (SELECT value FROM json_each(?)) AND started_at IS NOT NULL AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, string(data))
	if err != nil {
		return nil, err
	}
	defer childRows.Close()
	var children []call
	for childRows.Next() {
		var c call
		if err := childRows.Scan(&c.machine, &c.stamp); err != nil {
			return nil, err
		}
		children = append(children, c)
	}
	if err := childRows.Err(); err != nil {
		return nil, err
	}
	childRows.Close()
	for _, c := range children {
		at, err := time.Parse(time.RFC3339Nano, c.stamp)
		if err != nil {
			continue
		}
		r, err := tx.QueryContext(ctx, `SELECT id FROM sessions INDEXED BY idx_sessions_started WHERE started_at>=? AND started_at<=? AND machine=? AND deleted_at IS NULL AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, at.Add(-60*time.Second).UTC().Format(time.RFC3339Nano), at.Add(60*time.Second).UTC().Format(time.RFC3339Nano), c.machine)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		for r.Next() {
			var id string
			if err := r.Scan(&id); err != nil {
				r.Close()
				return nil, err
			}
			seeds = append(seeds, id)
		}
		if err := r.Err(); err != nil {
			r.Close()
			return nil, err
		}
		r.Close()
	}
	return seeds, nil
}

func launchInputMayMatter(input sql.NullString) bool {
	if !input.Valid || strings.TrimSpace(input.String) == "" {
		return true
	}
	calls, undecodable := shellCalls(input.String)
	if undecodable {
		return true
	}
	for _, call := range calls {
		if call.unknown {
			for _, re := range launchAgentWords {
				if re.MatchString(call.command) {
					return true
				}
			}
			continue
		}
		if len(parser.LaunchesFromCommand(call.command, "")) != 0 {
			return true
		}
	}
	return false
}

const launchBackfillKey = "launch_parent_backfill_v1"

func backfillLaunchParentsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	state, err := GetSyncStateTx(ctx, tx, launchBackfillKey)
	if err != nil {
		return 0, err
	}
	if state == "done" {
		return 0, nil
	}
	var cursor struct{ Started, ID string }
	updated := 0
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id, started_at FROM sessions WHERE started_at IS NOT NULL AND (started_at > ? OR (started_at = ? AND id > ?)) AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli')) ORDER BY started_at,id LIMIT 1000`, cursor.Started, cursor.Started, cursor.ID)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			if err = rows.Scan(&cursor.ID, &cursor.Started); err != nil {
				rows.Close()
				return 0, err
			}
			ids = append(ids, cursor.ID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		n, err := inferLaunchParentsTx(ctx, tx, ids)
		if err != nil {
			return 0, err
		}
		updated += n
		if len(ids) < 1000 {
			break
		}
	}
	if err = SetSyncStateTx(ctx, tx, launchBackfillKey, "done"); err != nil {
		return 0, err
	}
	return updated, nil
}

// restoreInferenceBlocks removes links inferred before rebuild metadata arrived.
func restoreInferenceBlocks(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET parent_session_id=NULL, relationship_type=`+parserRelationshipSQL+`, friction_rules_version='', sync_marker=MAX(COALESCE(sync_marker,''),strftime('%Y-%m-%dT%H:%M:%fZ','now'))
 WHERE COALESCE(parser_parent_session_id,'')='' AND NOT `+spawnEdgeExistsSQL("sessions")+` AND relationship_type='subagent' AND EXISTS (SELECT 1 FROM session_external_parents ep JOIN session_parent_inference_blocks b ON b.session_id=ep.session_id WHERE ep.session_id=sessions.id AND ep.inferred=1 AND ep.parent_session_id=sessions.parent_session_id)`)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM session_external_parents WHERE inferred=1 AND session_id IN (SELECT session_id FROM session_parent_inference_blocks)`)
	return err
}
