package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/stringutil"
	"go.kenn.io/kit/atomicfile"
)

// replicaWatchErrorLimit bounds the push error kept in the health record.
const replicaWatchErrorLimit = 500

// replicaWatchRecord is what a replica watcher last did, persisted so
// `<backend> status --max-stale` can tell a stalled watcher from an idle one.
// It records the outcome of each push the watcher ran instead of predicting
// what a push would send, so any cause of failure counts.
type replicaWatchRecord struct {
	// Target is the configured target name; empty for an unnamed default.
	Target string `json:"target"`
	// Destination fingerprints the target's URL and schema, so a record
	// written for an earlier destination of the same target is not reused.
	Destination string `json:"destination"`
	// PushStartedAt is set while a push runs and cleared when it finishes.
	PushStartedAt time.Time `json:"push_started_at,omitzero"`
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`
	// LastSuccessStartedAt is when the last successful push started.
	// Sessions changed from then on may not have reached the replica.
	LastSuccessStartedAt time.Time `json:"last_success_started_at,omitzero"`
	// FailingSince is the first failure after the last success. It is zero
	// while pushes succeed.
	FailingSince  time.Time `json:"failing_since,omitzero"`
	LastFailureAt time.Time `json:"last_failure_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
}

// replicaWatchHealth writes the health record for the watcher that holds the
// backend's watch lock. A nil *replicaWatchHealth records nothing.
type replicaWatchHealth struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	rec  replicaWatchRecord
}

func replicaWatchHealthPath(dataDir, backend string) string {
	return filepath.Join(dataDir, backend+"-watch-health.json")
}

// replicaWatchDestination fingerprints where a target pushes. It hashes the
// URL so credentials in it are not written to the record.
func replicaWatchDestination(target storage.ReplicaTarget) string {
	sum := sha256.Sum256([]byte(target.URL + "\x00" + target.Schema))
	return hex.EncodeToString(sum[:8])
}

// matches reports whether rec was written for target.
func (rec replicaWatchRecord) matches(target storage.ConfiguredReplica) bool {
	return rec.Target == target.Name &&
		rec.Destination == replicaWatchDestination(target.Target)
}

// newReplicaWatchHealth keeps the last success and failure streak of an
// earlier watcher for the same target and destination, so a restart does not
// hide a stall. A push still marked as running belonged to a watcher that
// exited mid-push; it becomes a failure so a crash loop builds a failure
// streak instead of looking like a fresh push after every restart.
func newReplicaWatchHealth(
	dataDir, backend string, target storage.ConfiguredReplica,
	now func() time.Time,
) *replicaWatchHealth {
	h := &replicaWatchHealth{
		path: replicaWatchHealthPath(dataDir, backend),
		now:  now,
		rec: replicaWatchRecord{
			Target:      target.Name,
			Destination: replicaWatchDestination(target.Target),
		},
	}
	prev, err := loadReplicaWatchRecord(h.path)
	switch {
	case err == nil && prev.matches(target):
		h.rec = prev
		if started := prev.PushStartedAt; !started.IsZero() {
			h.rec.PushStartedAt = time.Time{}
			if h.rec.FailingSince.IsZero() {
				h.rec.FailingSince = started
			}
			h.rec.LastFailureAt = started
			h.rec.LastError = "the watcher exited during a push"
			h.writeLocked()
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		log.Printf("warning: reading %s: %v", h.path, err)
	}
	return h
}

func (h *replicaWatchHealth) start() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rec.PushStartedAt = h.now().UTC()
	h.writeLocked()
}

func (h *replicaWatchHealth) finish(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now().UTC()
	started := h.rec.PushStartedAt
	h.rec.PushStartedAt = time.Time{}
	if err == nil {
		h.rec.LastSuccessAt = now
		h.rec.LastSuccessStartedAt = started
		h.rec.FailingSince = time.Time{}
		h.rec.LastError = ""
	} else {
		if h.rec.FailingSince.IsZero() {
			h.rec.FailingSince = now
		}
		h.rec.LastFailureAt = now
		h.rec.LastError = stringutil.SafeTruncate(err.Error(), replicaWatchErrorLimit)
	}
	h.writeLocked()
}

// writeLocked replaces the record through a rename so a reader never sees a
// partial file. A failed write is logged; the watcher keeps pushing.
func (h *replicaWatchHealth) writeLocked() {
	data, err := json.Marshal(h.rec)
	if err == nil {
		err = atomicfile.WriteFile(h.path, data)
	}
	if err != nil {
		log.Printf("warning: writing %s: %v", h.path, err)
	}
}

func loadReplicaWatchRecord(path string) (replicaWatchRecord, error) {
	var rec replicaWatchRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("parsing %s: %w", path, err)
	}
	return rec, nil
}

// replicaWatchStall explains why the watcher has stalled, or returns "" when
// it is healthy or idle. A watcher stalls when a push has run for longer than
// limit, when pushes have failed for longer than limit, or when a session
// changed after the last successful push started and has waited longer than
// limit without a push. A running push or a failure streak younger than limit
// is given that time to finish or recover. changedBetween counts archive
// sessions changed from one time through another; a zero start counts from
// the beginning.
func replicaWatchStall(
	rec replicaWatchRecord, now time.Time, limit time.Duration,
	changedBetween func(from, until time.Time) (int, error),
) (string, error) {
	if !rec.PushStartedAt.IsZero() && now.Sub(rec.PushStartedAt) > limit {
		return fmt.Sprintf(
			"the push that started %s has not finished; it is stuck or the watcher stopped mid-push",
			agoText(rec.PushStartedAt, now),
		), nil
	}
	if !rec.FailingSince.IsZero() && now.Sub(rec.FailingSince) > limit {
		return fmt.Sprintf(
			"pushes have failed since %s: %s",
			agoText(rec.FailingSince, now), rec.LastError,
		), nil
	}
	if !rec.PushStartedAt.IsZero() || !rec.FailingSince.IsZero() {
		return "", nil
	}
	waitedUntil := now.Add(-limit)
	from := rec.LastSuccessStartedAt
	if !from.IsZero() && from.After(waitedUntil) {
		return "", nil
	}
	changed, err := changedBetween(from, waitedUntil)
	if err != nil {
		return "", err
	}
	if changed == 0 {
		return "", nil
	}
	if from.IsZero() {
		return fmt.Sprintf(
			"%d session(s) are waiting and no push has succeeded; check that the watcher is running",
			changed,
		), nil
	}
	return fmt.Sprintf(
		"%d session(s) changed after the last successful push started %s and nothing has pushed them; check that the watcher is running",
		changed, agoText(from, now),
	), nil
}

// replicaWatchRows describes the health record for `<backend> status`.
func replicaWatchRows(rec replicaWatchRecord, now time.Time) []storage.StatusRow {
	success := "never"
	if !rec.LastSuccessAt.IsZero() {
		success = agoText(rec.LastSuccessAt, now)
	}
	rows := []storage.StatusRow{{Label: "Watcher last success:", Value: success}}
	if !rec.PushStartedAt.IsZero() {
		rows = append(rows, storage.StatusRow{
			Label: "Watcher push running:", Value: "since " + agoText(rec.PushStartedAt, now),
		})
	}
	if !rec.FailingSince.IsZero() {
		rows = append(rows,
			storage.StatusRow{Label: "Watcher failing since:", Value: agoText(rec.FailingSince, now)},
			storage.StatusRow{Label: "Watcher last error:", Value: rec.LastError},
		)
	}
	return rows
}

// agoText renders t with its age, for example
// "2026-10-26T16:16:55Z (5h12m ago)".
func agoText(t, now time.Time) string {
	age := max(now.Sub(t), 0).Round(time.Second)
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format(time.RFC3339), age)
}
