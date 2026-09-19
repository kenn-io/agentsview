package storage

import (
	"encoding/json/jsontext"
	"errors"
	"time"

	"go.kenn.io/agentsview/internal/jsonutil"
)

// PushOptions controls a single replica push. The zero value is an
// incremental push with generation-wide vector reconciliation.
type PushOptions struct {
	// Full bypasses unchanged-fingerprint and unchanged-hash skips so
	// every session is resent.
	Full bool
	// ScopeVectorsToChangedSessions limits the vector phase's local
	// hash read and replica state read to this push's changed relational
	// sessions, instead of reconciling the whole generation. Ignored
	// when the push runs (or is internally promoted to run) full, so
	// reset recovery and backfills keep generation-wide reconciliation.
	ScopeVectorsToChangedSessions bool
	// LastReconciledVectorGeneration is the replica generation id the
	// caller last reconciled generation-wide. When a scoped push resolves
	// a different active generation id, the vector phase promotes itself
	// to a generation-wide read so a newly active or recreated generation
	// is never left partially populated. Zero on the first push, which
	// the reconcile bit already forces generation-wide.
	LastReconciledVectorGeneration int64
}

// PushProgress is reported after each batch during a replica push.
type PushProgress struct {
	// Phase is "preparing" while per-session push fingerprints are computed
	// (SessionsDone/SessionsTotal count candidate sessions fingerprinted; on
	// a full push this covers every local session and can run for minutes),
	// "" during the session/message push, and "vectors" during the vector
	// phase, whose progress is carried by the Vector* fields.
	Phase            string
	SessionsDone     int
	SessionsTotal    int
	MessagesDone     int
	SkippedConflicts int
	Errors           int
	// VectorSessionsDone counts local sessions examined by the vector
	// phase's delta scan (most are unchanged and skipped cheaply);
	// VectorSessionsTotal is the local candidate count and
	// VectorChunksPushed the embedding chunks written so far.
	VectorSessionsDone  int
	VectorSessionsTotal int
	VectorChunksPushed  int
}

// PushResult summarizes a replica push.
//
//nolint:recvcheck // Value encoding and pointer decoding intentionally implement distinct interfaces.
type PushResult struct {
	SessionsPushed   int
	MessagesPushed   int
	SkippedConflicts int
	// SkippedUnchanged counts in-scope sessions whose fingerprint already
	// matched the replica. Backends that skip silently leave it zero.
	SkippedUnchanged int
	// DeletedStale counts replica sessions removed because they no longer
	// exist locally or left the push scope.
	DeletedStale int
	Errors       int
	Duration     time.Duration
	// Full reports whether the push rewrote every in-scope session, and
	// FullReason says why when the caller did not ask for it.
	Full       bool
	FullReason string
	// Vectors summarizes the vector phase. A backend without one reports
	// Skipped with an empty reason.
	Vectors VectorPushResult
}

type pushResultJSON PushResult

func (r PushResult) MarshalJSONTo(out *jsontext.Encoder) error {
	return jsonutil.MarshalDurationFields(out, pushResultJSON(r))
}

func (r *PushResult) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	var decoded pushResultJSON
	if err := jsonutil.UnmarshalDurationFields(in, &decoded); err != nil {
		return err
	}
	*r = PushResult(decoded)
	return nil
}

// VectorPushResult summarizes the vector push phase. Skipped is set (with a
// human reason) when the phase cannot run: no source, no active generation,
// or no vector extension on the replica. The counters describe what changed
// on the replica.
//
// DocsDeleted counts vector document rows removed: on eviction of a whole
// session and when a doc vanished from a re-pushed session (its shared row is
// removed only when no other generation still embeds it). Conflicts counts
// sessions this pusher left untouched because the replica owner marker names
// a different machine, on the push path (a locally changed session the
// replica says another machine owns) and on the evict path (an owned-elsewhere
// session absent from local, kept rather than evicted).
type VectorPushResult struct {
	Skipped           bool
	SkippedReason     string
	SessionsPushed    int
	SessionsUnchanged int
	// SessionsDeferred counts sessions whose vector reconciliation was
	// withheld this run: a failed session-phase push, an export hash
	// that diverged mid-push, or an eviction abandoned because the local
	// generation changed. The delta state is untouched, so the next
	// generation-wide reconciliation sends them.
	SessionsDeferred int
	DocsPushed       int
	ChunksPushed     int
	DocsDeleted      int
	SessionsEvicted  int
	Conflicts        int
	// GenerationID is the replica id of the generation this phase
	// reconciled, zero when the phase was skipped or found no active
	// generation. The watch orchestrator records it after a clean
	// generation-wide pass so a later push against a different generation
	// id (a re-embed, or a reset/drop that recreated the row under any
	// machine) promotes the next scoped push to a generation-wide
	// reconciliation instead of writing only the changed sessions' chunks.
	GenerationID int64
}

// ValidateProjectFilters rejects ambiguous include/exclude project filters.
func ValidateProjectFilters(projects, excludeProjects []string) error {
	if len(projects) > 0 && len(excludeProjects) > 0 {
		return errors.New("projects and exclude_projects are mutually exclusive")
	}
	return nil
}
