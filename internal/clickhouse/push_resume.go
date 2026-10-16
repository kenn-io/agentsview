package clickhouse

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/db"
)

// fullPushRun records a full push that has not finished. A full push takes
// long enough on a large archive that a restart can interrupt it; the record
// lets the next process keep the sessions the interrupted one finished.
//
// The record itself holds no per-session progress. A session counts as done
// when this archive's mirror session row carries a push version from the run
// or later and a fingerprint equal to the local one. The session row is the
// last row a batch writes for a session, so its presence means the session's
// dependents and usage snapshot landed too.
type fullPushRun struct {
	// Version is the push version of the process that started the run.
	// Rows written by the run carry it or a later version.
	Version uint64 `json:"version"`
	// DatabaseID, Scope, SchemaVersion, and DataVersion describe what the run
	// was publishing. Rows a run wrote under different values may be stale,
	// so a mismatch starts a new run instead of resuming.
	DatabaseID    string `json:"database_id"`
	Scope         string `json:"scope"`
	SchemaVersion int    `json:"schema_version"`
	DataVersion   int    `json:"data_version"`
	// Reason is why the run was a full push.
	Reason string `json:"reason"`
	// Errored reports that the run reached the end with session errors.
	// Such a run resumes only while something else still forces a full
	// push; otherwise the incremental push handles the failed sessions.
	Errored bool `json:"errored,omitempty"`
}

func (r fullPushRun) encode() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encoding full push run: %w", err)
	}
	return string(b), nil
}

func parseFullPushRun(value string) (fullPushRun, error) {
	var run fullPushRun
	if err := json.Unmarshal([]byte(value), &run); err != nil {
		return fullPushRun{}, fmt.Errorf("decoding full push run %q: %w", value, err)
	}
	return run, nil
}

// newFullPushRun describes a full push this process starts.
func (s *Sync) newFullPushRun(version uint64, databaseID, reason string) fullPushRun {
	return fullPushRun{
		Version:       version,
		DatabaseID:    databaseID,
		Scope:         s.scopeString(),
		SchemaVersion: SchemaVersion,
		DataVersion:   db.CurrentDataVersion(),
		Reason:        reason,
	}
}

// pendingFullPushRun interprets the stored run record. pending reports that
// an earlier full push did not finish, so this push must be full too; reason
// is that push's reason. resume reports that the run can continue: it
// published the same archive generation, scope, and formats this process
// would.
func (s *Sync) pendingFullPushRun(stored, databaseID string) (run fullPushRun, pending, resume bool) {
	if stored == "" {
		return fullPushRun{}, false, false
	}
	run, err := parseFullPushRun(stored)
	if err != nil {
		log.Printf("clickhouse push: starting over instead of resuming an unfinished full push whose record is unreadable: %v", err)
		return fullPushRun{Reason: "unfinished full push"}, true, false
	}
	current := s.newFullPushRun(run.Version, databaseID, run.Reason)
	current.Errored = run.Errored
	if run.Version == 0 || run != current {
		log.Printf("clickhouse push: starting over instead of resuming the unfinished full push (%s) because the archive, scope, or format changed since it began",
			run.Reason)
		return run, true, false
	}
	return run, true, true
}

// readRunFingerprints returns the fingerprints of this archive's mirror
// sessions written at version since or later.
func (s *Sync) readRunFingerprints(ctx context.Context, since uint64) (map[string]string, error) {
	rows, err := s.conn.QueryContext(ctx,
		"SELECT id, agentsview_push_fingerprint FROM sessions WHERE source_archive_id = ? AND push_version >= ?",
		s.archiveID, since)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse sessions the interrupted full push wrote: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, fmt.Errorf("scanning clickhouse session the interrupted full push wrote: %w", err)
		}
		out[id] = fp
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading clickhouse sessions the interrupted full push wrote: %w", err)
	}
	return out, nil
}
