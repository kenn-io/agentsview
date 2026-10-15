package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// fullPushRunStateKey holds the record of a full push that has not finished.
const fullPushRunStateKey = "pg_full_push_run_v1"

// fullPushRunCheckpointInterval bounds how often a full push rewrites its run
// record. The record lists every finished session, so writing it after every
// batch would cost time quadratic in the archive size; a kill loses at most
// one interval of work instead.
const fullPushRunCheckpointInterval = 15 * time.Second

// fullPushRun records a full push that has not finished. A full push of a
// large archive takes long enough that a restart can interrupt it; the record
// lets the next process skip the sessions the interrupted one committed.
type fullPushRun struct {
	// Reason is why the run was a full push.
	Reason string `json:"reason"`
	// TargetFingerprint, ArchiveID, DatabaseGeneration, MarkerID,
	// DataVersion, and Backfills describe what the run was publishing.
	// Sessions a run committed under different values may be stale, so a
	// mismatch starts a new run instead of resuming. Backfills lists the
	// backfill markers the run was rewriting sessions to satisfy.
	TargetFingerprint  string   `json:"target_fingerprint"`
	ArchiveID          string   `json:"archive_id"`
	DatabaseGeneration string   `json:"database_generation"`
	MarkerID           string   `json:"marker_id"`
	DataVersion        int      `json:"data_version"`
	Backfills          []string `json:"backfills"`
	// Sessions maps each session the run committed to a hash of the push
	// fingerprint it committed.
	Sessions map[string]string `json:"sessions"`
	// VectorGeneration and Vectors record the vector phase's progress: each
	// session whose vectors the run pushed for that PG generation, mapped to
	// the aggregate document hash it pushed.
	VectorGeneration int64             `json:"vector_generation,omitempty"`
	Vectors          map[string]string `json:"vectors,omitempty"`
	// Errored reports that the run reached the end with session errors.
	// Such a run resumes only while something else still forces a full
	// push; otherwise the incremental push handles the failed sessions.
	Errored bool `json:"errored,omitempty"`
}

// resumableAs reports whether the run's committed sessions are current for a
// push described by current: both publish the same archive generation to the
// same target with the same formats, and the run was already doing every
// backfill current still needs. The run's own push records its backfills done
// once its session phase ends, so a run killed after that needs fewer.
func (r fullPushRun) resumableAs(current fullPushRun) bool {
	return r.TargetFingerprint == current.TargetFingerprint &&
		r.ArchiveID == current.ArchiveID &&
		r.DatabaseGeneration == current.DatabaseGeneration &&
		r.MarkerID == current.MarkerID &&
		r.DataVersion == current.DataVersion &&
		!slices.ContainsFunc(current.Backfills, func(key string) bool {
			return !slices.Contains(r.Backfills, key)
		})
}

// fullPushRunTracker records a running full push's progress in local sync
// state. A nil tracker records nothing, so call sites need no full-push check.
type fullPushRunTracker struct {
	state     syncStateStore
	run       fullPushRun
	interval  time.Duration
	lastSaved time.Time
}

// newFullPushRun describes a full push this process would run now.
func (s *Sync) newFullPushRun(reason, markerID string, backfills []string) fullPushRun {
	return fullPushRun{
		Reason:             reason,
		TargetFingerprint:  s.targetFingerprint,
		ArchiveID:          s.archiveID,
		DatabaseGeneration: s.databaseGeneration,
		MarkerID:           markerID,
		DataVersion:        db.CurrentDataVersion(),
		Backfills:          backfills,
		Sessions:           map[string]string{},
	}
}

// pendingFullPushRun reads the stored run record. pending reports that an
// earlier full push did not finish, so this push must be full too; resume
// reports that the run published what this process would publish now, so it
// can continue. run.Reason is the interrupted push's reason either way.
func (s *Sync) pendingFullPushRun(
	ctx context.Context, state syncStateStore, current fullPushRun,
) (run fullPushRun, pending, resume bool, err error) {
	stored, err := state.GetSyncState(ctx, fullPushRunStateKey)
	if err != nil {
		return fullPushRun{}, false, false, fmt.Errorf("reading %s: %w", fullPushRunStateKey, err)
	}
	if stored == "" {
		return fullPushRun{}, false, false, nil
	}
	if err := json.Unmarshal([]byte(stored), &run); err != nil {
		log.Printf("pgsync: starting over instead of resuming an unfinished full push whose record is unreadable: %v", err)
		return fullPushRun{Reason: "unfinished full push"}, true, false, nil
	}
	if !run.resumableAs(current) {
		log.Printf("pgsync: starting over instead of resuming the unfinished full push (%s) because the target, archive, or format changed since it began",
			run.Reason)
		return run, true, false, nil
	}
	return run, true, true, nil
}

// dropMissingRunSessions forgets run sessions that PG no longer holds under
// this pusher's marker. A PG reset while the push was down would otherwise
// leave the resumed run skipping sessions that are gone.
func (s *Sync) dropMissingRunSessions(ctx context.Context, run *fullPushRun) error {
	ids := slices.Collect(maps.Keys(run.Sessions))
	present := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += pushComparisonBatchSize {
		chunk := ids[start:min(start+pushComparisonBatchSize, len(ids))]
		if err := s.markPresentRunSessions(ctx, run.MarkerID, chunk, present); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if !present[id] {
			delete(run.Sessions, id)
		}
	}
	return nil
}

// markPresentRunSessions sets present[id] for each of ids that PG holds
// under markerID.
func (s *Sync) markPresentRunSessions(
	ctx context.Context, markerID string, ids []string, present map[string]bool,
) error {
	rows, err := s.pg.QueryContext(ctx,
		`SELECT id FROM sessions WHERE owner_marker = $1 AND id = ANY($2)`,
		markerID, ids)
	if err != nil {
		return fmt.Errorf("reading pg sessions the interrupted full push wrote: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning pg session the interrupted full push wrote: %w", err)
		}
		present[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading pg sessions the interrupted full push wrote: %w", err)
	}
	return nil
}

// startFullPushRun begins tracking run and saves it, so an interruption
// before the first checkpoint still leaves the next push full.
func (s *Sync) startFullPushRun(
	ctx context.Context, state syncStateStore, run fullPushRun,
) (*fullPushRunTracker, error) {
	interval := s.fullRunCheckpointInterval
	if interval == 0 {
		interval = fullPushRunCheckpointInterval
	}
	t := &fullPushRunTracker{state: state, run: run, interval: interval}
	if err := t.save(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func runFingerprintHash(fingerprint string) string {
	// Half a SHA-256 still tells one session's fingerprints apart, and the
	// record, rewritten every checkpoint, holds one hash per session.
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:16])
}

// sessionDone reports whether the run committed the session with this push
// fingerprint.
func (t *fullPushRunTracker) sessionDone(id, fingerprint string) bool {
	if t == nil {
		return false
	}
	h, ok := t.run.Sessions[id]
	return ok && h == runFingerprintHash(fingerprint)
}

// recordSessions notes committed sessions and checkpoints when due.
func (t *fullPushRunTracker) recordSessions(
	ctx context.Context, sessions []db.Session, fingerprints map[string]string,
) error {
	if t == nil || len(sessions) == 0 {
		return nil
	}
	for _, sess := range sessions {
		t.run.Sessions[sess.ID] = runFingerprintHash(fingerprints[sess.ID])
	}
	return t.checkpoint(ctx)
}

// vectorDone reports whether the run pushed the session's vectors with this
// aggregate hash to generation.
func (t *fullPushRunTracker) vectorDone(generation int64, id, aggHash string) bool {
	if t == nil || t.run.VectorGeneration != generation {
		return false
	}
	h, ok := t.run.Vectors[id]
	return ok && h == aggHash
}

// recordVector notes a pushed vector session and checkpoints when due.
func (t *fullPushRunTracker) recordVector(
	ctx context.Context, generation int64, id, aggHash string,
) error {
	if t == nil {
		return nil
	}
	if t.run.VectorGeneration != generation || t.run.Vectors == nil {
		t.run.VectorGeneration = generation
		t.run.Vectors = map[string]string{}
	}
	t.run.Vectors[id] = aggHash
	return t.checkpoint(ctx)
}

func (t *fullPushRunTracker) checkpoint(ctx context.Context) error {
	if time.Since(t.lastSaved) < t.interval {
		return nil
	}
	return t.save(ctx)
}

// save writes the run record. A nil tracker saves nothing.
func (t *fullPushRunTracker) save(ctx context.Context) error {
	if t == nil {
		return nil
	}
	data, err := json.Marshal(t.run)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", fullPushRunStateKey, err)
	}
	if err := t.state.SetSyncState(ctx, fullPushRunStateKey, string(data)); err != nil {
		return fmt.Errorf("writing %s: %w", fullPushRunStateKey, err)
	}
	t.lastSaved = time.Now()
	return nil
}

// complete ends the run. A run with session errors stays pending with its
// progress saved, so the next push resumes and retries the failed sessions;
// otherwise the run record is cleared.
func (t *fullPushRunTracker) complete(ctx context.Context, sessionErrors int) error {
	if t == nil {
		return nil
	}
	if sessionErrors > 0 {
		t.run.Errored = true
		return t.save(ctx)
	}
	if err := t.state.SetSyncState(ctx, fullPushRunStateKey, ""); err != nil {
		return fmt.Errorf("clearing %s: %w", fullPushRunStateKey, err)
	}
	return nil
}
