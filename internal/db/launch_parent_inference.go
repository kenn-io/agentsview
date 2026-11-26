package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"path"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

func launchFolder(s string) string {
	s = normalizedMappingPath(s)
	windows := strings.HasPrefix(s, "//") || len(s) >= 2 && s[1] == ':'
	if windows {
		s = strings.ToLower(s)
	}
	return s
}

func sameLaunchFolder(a, b string) bool {
	convert := func(path, reference string) string {
		path = normalizedMappingPath(path)
		reference = normalizedMappingPath(reference)
		if len(reference) >= 3 && reference[1] == ':' && reference[2] == '/' && len(path) >= 3 && path[0] == '/' && path[2] == '/' && (path[1] >= 'a' && path[1] <= 'z' || path[1] >= 'A' && path[1] <= 'Z') {
			return string(path[1]) + ":" + path[2:]
		}
		return path
	}
	return launchFolder(convert(a, b)) == launchFolder(convert(b, a))
}

type launchChild struct {
	id, agent, cwd, machine, started string
	qualified                        bool
}

// The activity index is dropped only while a disposable archive is bulk-loaded.
// Its final backfill links the runs after the index has been rebuilt.
func launchInferenceEnabled(ctx context.Context, tx *sql.Tx) (bool, error) {
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_messages_activity_timestamp')`).Scan(&enabled)
	return enabled, err
}

func inferLaunchParentsTx(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	enabled, err := launchInferenceEnabled(ctx, tx)
	if err != nil || !enabled {
		return 0, err
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
	slices.SortFunc(children, func(a, b launchChild) int {
		left, leftErr := time.Parse(time.RFC3339Nano, a.started)
		right, rightErr := time.Parse(time.RFC3339Nano, b.started)
		if leftErr == nil && rightErr == nil {
			if order := left.Compare(right); order != 0 {
				return order
			}
		}
		return strings.Compare(a.id, b.id)
	})
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
const launchCandidateSQL = `SELECT s.id, s.machine, m.timestamp, tc.tool_name, tc.input_json, s.deleted_at IS NOT NULL, COALESCE(tc.working_directory,'') FROM messages m INDEXED BY idx_messages_activity_timestamp
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
	candidates, nearbyRuns, err := launchContext(ctx, tx, child, start)
	if err != nil {
		return 0, err
	}
	var existingParent string
	err = tx.QueryRowContext(ctx, `SELECT parent_session_id FROM session_external_parents WHERE session_id=? AND inferred=1`, child.id).Scan(&existingParent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	matches := map[string]bool{}
	known := map[string]int{}
	competing := map[string]map[string]bool{}
	uncertain := map[string]bool{}
	for _, launch := range launchWindow(candidates, time.Time{}, start) {
		matches[launch.parent] = true
		if !launch.certain && (!launch.retained || launch.parent != existingParent) {
			uncertain[launch.parent] = true
			continue
		}
		runs := launchRuns(start, launch.at, candidates, nearbyRuns)
		known[launch.parent]++
		if competing[launch.parent] == nil {
			competing[launch.parent] = map[string]bool{}
		}
		for _, run := range runs {
			competing[launch.parent][run] = true
		}
	}
	for id, count := range known {
		if !uncertain[id] && count < len(competing[id]) {
			delete(known, id)
		}
	}
	if len(matches) == 1 && len(known) == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET parser_relationship_type = relationship_type WHERE id = ? AND parser_relationship_type IS NULL`, child.id); err != nil {
			return 0, err
		}
		for id := range matches {
			_, err = tx.ExecContext(ctx, `INSERT INTO session_external_parents(session_id,parent_session_id,inferred) VALUES(?,?,1) ON CONFLICT(session_id) DO UPDATE SET parent_session_id=excluded.parent_session_id, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE inferred=1 AND parent_session_id != excluded.parent_session_id`, child.id, id)
			if err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	return retractInferredLaunchParent(ctx, tx, child.id)
}

type nearbyLaunchRun struct {
	id      string
	started time.Time
}

func launchContext(ctx context.Context, tx *sql.Tx, child launchChild, start time.Time) ([]possibleLaunch, []nearbyLaunchRun, error) {
	candidates, err := possibleLaunches(ctx, tx, child, start.Add(-60*time.Second-time.Nanosecond), start.Add(60*time.Second))
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 {
		return candidates, nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,cwd,started_at FROM sessions INDEXED BY idx_sessions_started WHERE started_at>=? AND started_at<=? AND machine=? AND agent=? AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, start.Add(-62*time.Second).UTC().Format(time.RFC3339Nano), start.Add(62*time.Second).UTC().Format(time.RFC3339Nano), child.machine, child.agent)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var runs []nearbyLaunchRun
	for rows.Next() {
		var id, cwd, stamp string
		if err := rows.Scan(&id, &cwd, &stamp); err != nil {
			return nil, nil, err
		}
		started, err := time.Parse(time.RFC3339Nano, stamp)
		if err == nil && absoluteLaunchFolder(cwd) && sameLaunchFolder(cwd, child.cwd) {
			runs = append(runs, nearbyLaunchRun{id, started})
		}
	}
	return candidates, runs, rows.Err()
}

// launchRuns finds the runs that could have used an invocation.
func launchRuns(childStart, at time.Time, candidates []possibleLaunch, nearby []nearbyLaunchRun) []string {
	var runs []string
	var later []nearbyLaunchRun
	for _, run := range nearby {
		if !run.started.Before(at) && run.started.Sub(at) <= 60*time.Second {
			if run.started.After(childStart) {
				later = append(later, run)
			} else {
				runs = append(runs, run.id)
			}
		}
	}
	if len(later) == 0 {
		return runs
	}
	var launches []time.Time
	for _, candidate := range launchWindow(candidates, childStart, at.Add(60*time.Second)) {
		if candidate.certain {
			launches = append(launches, candidate.at)
		}
	}
	slices.SortFunc(later, func(a, b nearbyLaunchRun) int { return a.started.Compare(b.started) })
	for _, run := range later {
		matched := false
		for i, launch := range launches {
			if !launch.After(run.started) && run.started.Sub(launch) <= 60*time.Second {
				launches = slices.Delete(launches, i, i+1)
				matched = true
				break
			}
		}
		if !matched {
			runs = append(runs, run.id)
		}
	}
	return runs
}

type possibleLaunch struct {
	parent   string
	at       time.Time
	certain  bool
	retained bool
}

func launchWindow(candidates []possibleLaunch, from, until time.Time) []possibleLaunch {
	var result []possibleLaunch
	for _, candidate := range candidates {
		if candidate.at.After(from) && !candidate.at.After(until) {
			result = append(result, candidate)
		}
	}
	return result
}

func possibleLaunches(ctx context.Context, tx *sql.Tx, child launchChild, from, until time.Time) ([]possibleLaunch, error) {
	rows, err := tx.QueryContext(ctx, launchCandidateSQL, from.Add(-2*time.Second).UTC().Format(time.RFC3339Nano), until.Add(2*time.Second).UTC().Format(time.RFC3339Nano), child.machine, child.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var launches []possibleLaunch
	for rows.Next() {
		var id, machine, stamp, tool, callCwd string
		var input sql.NullString
		var trashed bool
		if err := rows.Scan(&id, &machine, &stamp, &tool, &input, &trashed, &callCwd); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !at.After(from) || at.After(until) || pollTool(tool) {
			continue
		}
		if !input.Valid || strings.TrimSpace(input.String) == "" {
			launches = append(launches, possibleLaunch{parent: id, at: at})
			continue
		}
		calls, undecodable := shellCalls(input.String)
		if undecodable {
			launches = append(launches, possibleLaunch{parent: id, at: at})
			continue
		}
		for _, call := range calls {
			if call.unknown {
				launches = append(launches, possibleLaunch{parent: id, at: at})
				continue
			}
			dir := callCwd
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
				matched := slices.ContainsFunc(launch.Dirs, func(dir string) bool {
					return absoluteLaunchFolder(dir) && sameLaunchFolder(dir, child.cwd)
				})
				if launch.Unknown || matched {
					certain := machine == child.machine && matched && !launch.Unknown && len(launch.Dirs) == 1
					launches = append(launches, possibleLaunch{parent: id, at: at, certain: certain && !trashed, retained: certain && trashed})
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(launches, func(a, b possibleLaunch) int { return a.at.Compare(b.at) })
	return launches, nil
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
	changes := make(map[string]int, len(ids))
	for _, id := range ids {
		changes[id] = 0
	}
	return launchSeedsChanged(ctx, tx, ids, changes)
}

func launchSeedsChanged(ctx context.Context, tx *sql.Tx, ids []string, changes map[string]int) ([]string, error) {
	enabled, err := launchInferenceEnabled(ctx, tx)
	if err != nil || !enabled {
		return ids, err
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	type call struct {
		machine, stamp, tool string
		input                sql.NullString
	}
	var calls []call
	if len(changes) > 0 {
		changedData, err := json.Marshal(changes)
		if err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.machine, m.timestamp, tc.tool_name, tc.input_json FROM json_each(?) changed JOIN sessions s ON s.id=changed.key CROSS JOIN messages m INDEXED BY idx_messages_session_ordinal ON m.session_id=s.id CROSS JOIN tool_calls tc INDEXED BY idx_tool_calls_message ON tc.message_id=m.id WHERE s.agent IN ('claude','codex') AND m.ordinal>=CAST(changed.value AS INTEGER) AND tc.category='Bash' AND m.timestamp IS NOT NULL`, string(changedData))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
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
	}
	var anchors []call
	for _, c := range calls {
		if !pollTool(c.tool) && launchInputMayMatter(c.input) {
			anchors = append(anchors, c)
		}
	}
	childRows, err := tx.QueryContext(ctx, `SELECT machine,started_at FROM sessions WHERE id IN (SELECT value FROM json_each(?)) AND started_at IS NOT NULL AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, string(data))
	if err != nil {
		return nil, err
	}
	defer childRows.Close()
	for childRows.Next() {
		var c call
		if err := childRows.Scan(&c.machine, &c.stamp); err != nil {
			return nil, err
		}
		anchors = append(anchors, c)
	}
	if err := childRows.Err(); err != nil {
		return nil, err
	}
	childRows.Close()
	seeds := append([]string{}, ids...)
	for _, c := range anchors {
		at, err := time.Parse(time.RFC3339Nano, c.stamp)
		if err != nil {
			continue
		}
		r, err := tx.QueryContext(ctx, `SELECT id FROM sessions INDEXED BY idx_sessions_started WHERE (?='' OR machine=?) AND started_at>=? AND started_at<=? AND deleted_at IS NULL AND ((agent='codex' AND session_kind='non-interactive') OR (agent='claude' AND entrypoint='sdk-cli'))`, strings.TrimSpace(c.machine), c.machine, at.Add(-62*time.Second).UTC().Format(time.RFC3339Nano), at.Add(62*time.Second).UTC().Format(time.RFC3339Nano))
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
		err = r.Err()
		r.Close()
		if err != nil {
			return nil, err
		}
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
			return true
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
	if state != "" {
		if err := json.Unmarshal([]byte(state), &cursor); err != nil {
			return 0, err
		}
	}
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
	updated, err := inferLaunchParentsTx(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	applied, err := applySessionExternalParentsFor(ctx, tx.ExecContext, func(ctx context.Context, query string, args ...any) rowScanner {
		return tx.QueryRowContext(ctx, query, args...)
	}, ids)
	if err != nil {
		return 0, err
	}
	state = "done"
	if len(ids) == 1000 {
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return 0, err
		}
		state = string(encoded)
	}
	if err = SetSyncStateTx(ctx, tx, launchBackfillKey, state); err != nil {
		return 0, err
	}
	return updated + applied, nil
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
