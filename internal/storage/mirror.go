package storage

import (
	"context"
	"encoding/json/jsontext"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/jsonutil"
)

// Mirror is a disposable local derived file rebuilt from the archive (DuckDB
// today). It is not a replica: it has no remote push, no ownership markers,
// and no vector phase. A remote backend implements Replica instead.
type Mirror interface {
	// Name is the CLI verb and the daemon push route segment, e.g. "duckdb".
	Name() string
	// DisplayName is the product name shown to operators.
	DisplayName() string
	// ValidatePushTarget rejects a config that cannot be written locally.
	ValidatePushTarget(cfg config.DuckDBConfig) error
	// Push rebuilds or incrementally updates the mirror from local.
	Push(
		ctx context.Context, cfg config.DuckDBConfig, local *db.DB,
		opts MirrorPushOptions, full bool,
		onProgress func(MirrorPushProgress),
	) (MirrorPushResult, error)
}

// MirrorPushOptions holds mirror push-scope filters and push-mode behavior.
type MirrorPushOptions struct {
	Projects        []string
	ExcludeProjects []string
	// Automatic marks a watch-mode or daemon-driven push. It skips
	// archive-scale diagnostics and defers on rebuild-lock contention.
	Automatic bool
}

// MirrorPushResult summarizes a mirror push.
//
//nolint:recvcheck // Value encoding and pointer decoding intentionally implement distinct interfaces.
type MirrorPushResult struct {
	SessionsPushed int
	MessagesPushed int
	Errors         int
	Duration       time.Duration
	Diagnostics    MirrorPushDiagnostics
	// ArchiveRebuilt reports that a daemon push's sync rebuilt the archive
	// before the push ran. The next push of every mirror then runs in full,
	// so a deferred push need not keep a --full request.
	ArchiveRebuilt bool
}

type mirrorPushResultJSON MirrorPushResult

func (r MirrorPushResult) MarshalJSONTo(out *jsontext.Encoder) error {
	return jsonutil.MarshalDurationFields(out, mirrorPushResultJSON(r))
}

func (r *MirrorPushResult) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	var decoded mirrorPushResultJSON
	if err := jsonutil.UnmarshalDurationFields(in, &decoded); err != nil {
		return err
	}
	*r = MirrorPushResult(decoded)
	return nil
}

// MirrorPushDiagnostics summarizes how a mirror push selected sessions.
type MirrorPushDiagnostics struct {
	Full bool
	// RebuildReason is the human-readable reason a rebuild was chosen
	// instead of an incremental push; empty for an incremental push.
	RebuildReason string
	Cutoff        string
	// LocalSessionCount is the number of local sessions in the push scope.
	// Automatic incremental pushes skip the archive-scale COUNT that
	// produces it and leave it 0; the CLI omits the figure when it is 0.
	LocalSessionCount        int
	CandidateSessions        MirrorSessionCounts
	SkippedUnchangedSessions MirrorSessionCounts
	PushedSessions           MirrorSessionCounts
	// DeletedStaleSessions counts sessions an incremental push removed
	// from the mirror: applied in-scope deletion-journal tombstones,
	// out-of-scope tombstones that were still mirror-resident, and window
	// candidates whose project moved out of the push scope.
	DeletedStaleSessions int
	// CurationRefreshed reports whether this push actually rewrote
	// starred_sessions/pinned_messages, as opposed to skipping the refresh
	// because the local in-scope curation state's fingerprint matched what
	// was already recorded in the mirror.
	CurationRefreshed bool
	// Deferred reports that an automatic push yielded to another publisher.
	// DeferredReason carries the explanation. No mirror state advances.
	Deferred       bool
	DeferredReason string
}

// MirrorSessionCounts summarizes a set of sessions without exposing content.
type MirrorSessionCounts struct {
	Total   int
	ByAgent map[string]int
}

// MirrorPushProgress is reported after each attempted session.
type MirrorPushProgress struct {
	// Phase is PushPhaseWaiting while a daemon push waits for another push
	// to finish, PushPhaseRebuilt once when its sync rebuilt the archive,
	// PushPhaseWaitingForSync while it waits for a local sync to finish, and
	// "" otherwise.
	Phase         string
	SessionsDone  int
	SessionsTotal int
	MessagesDone  int
	Errors        int
}
