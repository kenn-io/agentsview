package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
)

const (
	lastPushBoundaryStateKey           = "last_push_boundary_state"
	lastPushSourceArchiveIDKey         = "pg_source_archive_id_v1"
	lastPushTargetFingerprintKey       = "pg_target_fingerprint_v1"
	sessionAliasBackfillStateKey       = "pg_session_alias_backfill_v1"
	legacyProjectIdentityStateKey      = "project_identity_publication_revision_v2"
	projectIdentityPublicationStateKey = "project_identity_publication_revision_v3"
	transcriptRevisionBackfillStateKey = "pg_transcript_revision_backfill_v1"
	sessionProvenanceBackfillStateKey  = "pg_session_provenance_backfill_v2"
	unfilteredPublicationScope         = "all-projects"
)

// pushMarkerIDStateKey names the local sync-state entry holding this DB's
// stable push-marker identifier. The push-marker prefixes form PG
// sync_metadata keys for reset-detection marker rows.
const (
	pushMarkerIDStateKey              = "pg_push_marker_id"
	pushMarkerKeyPrefix               = "push_marker:"
	pushMarkerMachineAliasesKeyPrefix = "push_marker_machine_aliases:"
)

var errSessionOwnershipConflict = errors.New("session ownership conflict")
var errSessionExcluded = errors.New("session excluded")

type pushBoundaryState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// PushResult summarizes a push sync operation.
type PushResult struct {
	SessionsPushed   int
	MessagesPushed   int
	SkippedConflicts int
	Errors           int
	Duration         time.Duration
	Vectors          VectorPushResult
}

// pushPrepareProgressStride bounds how many sessions the fingerprint loop
// processes between "preparing" progress reports.
const pushPrepareProgressStride = 500

// timedPushSetupStep runs one pre-batch setup step and logs its duration when
// it exceeds a second, so a slow silent stretch of a push (per-row metadata
// upserts against a remote target, for example) is attributable in the log.
func timedPushSetupStep(name string, fn func() error) error {
	start := time.Now()
	if err := fn(); err != nil {
		return err
	}
	if d := time.Since(start); d > time.Second {
		log.Printf("pgsync: %s took %s", name, d.Round(time.Millisecond))
	}
	return nil
}

// PushProgress is reported after each batch during Push.
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

// PushOptions controls a single push. The zero value matches Push's
// historical behavior.
type PushOptions struct {
	// Full bypasses unchanged-fingerprint and unchanged-hash skips so
	// every session is resent.
	Full bool
	// ScopeVectorsToChangedSessions limits the vector phase's local
	// hash read and PG state read to this push's changed relational
	// sessions, instead of reconciling the whole generation. Ignored
	// when the push runs (or is internally promoted to run) full, so
	// reset recovery and backfills keep generation-wide reconciliation.
	ScopeVectorsToChangedSessions bool
	// LastReconciledVectorGeneration is the PG generation id the caller
	// last reconciled generation-wide. When a scoped push resolves a
	// different active generation id, the vector phase promotes itself to a
	// generation-wide read so a newly active or recreated generation is
	// never left partially populated (see pushVectors). Zero on the first
	// push, which the reconcile bit already forces generation-wide.
	LastReconciledVectorGeneration int64
}

// Push syncs local sessions and messages to PostgreSQL.
// The onProgress callback, if non-nil, is called after each
// batch with current totals.
func (s *Sync) Push(
	ctx context.Context, full bool,
	onProgress func(PushProgress),
) (PushResult, error) {
	return s.PushWithOptions(ctx, PushOptions{Full: full}, onProgress)
}

// PushWithOptions is Push with per-push options; see PushOptions.
func (s *Sync) PushWithOptions(
	ctx context.Context, opts PushOptions,
	onProgress func(PushProgress),
) (PushResult, error) {
	full := opts.Full
	start := time.Now()
	var result PushResult
	state := s.effectiveSyncState()
	aliasBackfillState := s.aliasBackfillSyncStateOrDefault()

	// Announce the preparation phase immediately: everything between here
	// and the first batch (marker checks, metadata syncs, fingerprints)
	// produces no per-batch reports, and some of it runs for minutes on a
	// full push against a remote target.
	if onProgress != nil {
		onProgress(PushProgress{Phase: "preparing"})
	}

	if err := CheckDataVersionCompat(ctx, s.pg); err != nil {
		return result, err
	}

	if err := s.normalizeSyncTimestamps(ctx); err != nil {
		return result, err
	}

	lastPush, err := state.GetSyncState("last_push_at")
	if err != nil {
		return result, fmt.Errorf(
			"reading last_push_at: %w", err,
		)
	}
	storedTargetFingerprint, err := state.GetSyncState(
		lastPushTargetFingerprintKey,
	)
	if err != nil {
		return result, fmt.Errorf(
			"reading %s: %w",
			lastPushTargetFingerprintKey, err,
		)
	}
	boundaryState, err := state.GetSyncState(
		lastPushBoundaryStateKey,
	)
	if err != nil {
		return result, fmt.Errorf(
			"reading %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	pushStateCleared := false
	if reset, reason := pushTargetState(
		lastPush,
		boundaryState,
		storedTargetFingerprint,
		s.targetFingerprint,
	); reset {
		log.Printf(
			"pgsync: %s; clearing local push watermark state",
			reason,
		)
		if err := clearPushState(state); err != nil {
			return result, err
		}
		lastPush = ""
		full = true
		pushStateCleared = true
	}
	archiveID, err := s.local.GetArchiveID(ctx)
	if err != nil {
		return result, fmt.Errorf("reading archive id: %w", err)
	}
	s.archiveID = archiveID
	repairedPreviousArchiveID := ""
	storedArchiveID, err := state.GetSyncState(lastPushSourceArchiveIDKey)
	if err != nil {
		return result, fmt.Errorf(
			"reading %s: %w", lastPushSourceArchiveIDKey, err,
		)
	}
	if storedArchiveID != "" && storedArchiveID != archiveID {
		log.Printf(
			"pgsync: source archive identity changed; retiring old archive metadata and clearing local push watermark state",
		)
		if err := s.retireSourceArchiveMetadata(ctx, storedArchiveID); err != nil {
			return result, err
		}
		repairedPreviousArchiveID = storedArchiveID
		if err := clearPushState(state); err != nil {
			return result, err
		}
		lastPush = ""
		boundaryState = ""
		full = true
		pushStateCleared = true
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return result, fmt.Errorf("reading database generation: %w", err)
	}
	s.databaseGeneration = databaseGeneration
	markerID, err := s.pushMarkerID()
	if err != nil {
		return result, err
	}
	markerMachine, markerMachineAliases, markerExists, err := s.pgPushMarkerMachineState(ctx, markerID)
	if err != nil {
		return result, err
	}
	legacyMarkerMachines := pushMarkerLegacyMachines(
		markerMachine, markerMachineAliases,
	)
	var reconciledScopeMoveIDs []string
	var identityRefreshSessionIDs []string
	// Keep the backfill marker scoped to target only; all other push
	// state remains scoped by full effective sync state (including filter
	// fingerprint when present).
	aliasBackfillNeeded := false
	full, aliasBackfillNeeded, err = applySessionAliasBackfillRequirement(
		aliasBackfillState, full,
	)
	if err != nil {
		return result, err
	}
	if aliasBackfillNeeded {
		log.Printf(
			"pgsync: session alias backfill marker missing; forcing full push",
		)
	}
	provenanceBackfillState := aliasBackfillState
	if s.isFiltered() {
		// The target-wide marker cannot describe a partial project scope.
		// Keep filtered completion in the same effective-scope namespace as
		// its watermark and boundary fingerprints.
		provenanceBackfillState = state
	}
	provenanceBackfillNeeded := false
	full, provenanceBackfillNeeded, err = applySessionProvenanceBackfillRequirement(
		provenanceBackfillState, full,
	)
	if err != nil {
		return result, err
	}
	if provenanceBackfillNeeded {
		log.Printf(
			"pgsync: session provenance backfill marker missing; forcing full push",
		)
	}
	transcriptRevisionBackfillNeeded := false
	full, transcriptRevisionBackfillNeeded, err =
		applyTranscriptRevisionBackfillRequirement(state, full)
	if err != nil {
		return result, err
	}
	if transcriptRevisionBackfillNeeded {
		log.Printf(
			"pgsync: transcript revision backfill marker missing; forcing full push",
		)
	}
	if full {
		lastPush = ""
		// Caller requested a full push — the PG schema
		// may have been dropped since schemaDone was set.
		// Clear the memo so EnsureSchema re-runs.
		s.schemaMu.Lock()
		s.schemaDone = false
		s.schemaMu.Unlock()
		if err := s.normalizeSyncTimestamps(
			ctx,
		); err != nil {
			return result, err
		}
		// When a filtered full push runs, clear persisted
		// watermark and boundary state so the next
		// unfiltered push also starts from scratch.
		if s.isFiltered() && !pushStateCleared {
			if err := clearPushState(state); err != nil {
				return result, err
			}
		}
	}

	// Coherence check: if local push state says we've pushed before
	// but this host's push marker is gone from PG, the PG side was
	// reset (schema dropped, DB recreated, etc.). Force a full push
	// so fingerprint-matched sessions are not skipped while missing
	// from PG. Boundary state counts here too: a partial first push
	// can leave last_push_at empty while still caching fingerprints
	// for successfully pushed sessions.
	if lastPush != "" || boundaryState != "" {
		if !markerExists {
			log.Printf(
				"pgsync: local push state set but PG push marker " +
					"missing; PG was reset, forcing full push",
			)
			lastPush = ""
			full = true
			if len(legacyMarkerMachines) == 0 {
				legacyMarkerMachines = nil
			}
			s.schemaMu.Lock()
			s.schemaDone = false
			s.schemaMu.Unlock()
			if err := s.normalizeSyncTimestamps(
				ctx,
			); err != nil {
				return result, err
			}
			// Filtered push against a reset PG: clear
			// watermark and boundary state so the next
			// unfiltered push also starts from scratch.
			if s.isFiltered() && !pushStateCleared {
				if err := clearPushState(state); err != nil {
					return result, err
				}
			}
		}
	}
	if s.isFiltered() {
		scopeMoveCandidates, scopeErr := listPGProjectScopeMoveCandidates(
			ctx, s.local, lastPush,
		)
		if scopeErr != nil {
			return result, fmt.Errorf(
				"listing filtered project-scope move candidates: %w", scopeErr,
			)
		}
		identityRefreshSessionIDs = make(
			[]string, 0, len(scopeMoveCandidates),
		)
		for _, candidate := range scopeMoveCandidates {
			identityRefreshSessionIDs = append(
				identityRefreshSessionIDs, candidate.ID,
			)
		}
		reconciledScopeMoveIDs, scopeErr = reconcilePGProjectScopeMoves(
			ctx, s.pg, markerID, scopeMoveCandidates,
			s.projects, s.excludeProjects,
		)
		if scopeErr != nil {
			return result, scopeErr
		}
	}
	if err := timedPushSetupStep("model pricing sync",
		func() error { return s.syncModelPricing(ctx) }); err != nil {
		return result, err
	}
	if err := timedPushSetupStep("cursor usage event sync",
		func() error { return s.syncCursorUsageEvents(ctx) }); err != nil {
		return result, err
	}
	cutoff := time.Now().UTC().Format(LocalSyncTimestampLayout)

	// Candidate selection shares ListSessionsForMirrorWindow with the
	// DuckDB mirror push: sync_marker >= lastPush, inclusive below and
	// deliberately unbounded above. An upper bound at cutoff would let a
	// clock-skewed future file_mtime push a session's marker past now and
	// mask its later real changes until wall time caught up. The inclusive
	// lower bound also covers boundary-equal sessions (marker == lastPush),
	// which the prior-fingerprint comparison below skips cheaply when
	// unchanged, so no separate boundary re-query is needed.
	allSessions, err := s.local.ListSessionsForMirrorWindow(
		ctx, lastPush, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf(
			"listing sessions for push window: %w", err,
		)
	}

	sessionByID := make(
		map[string]db.Session, len(allSessions),
	)
	for _, sess := range allSessions {
		sessionByID[sess.ID] = sess
	}

	var priorFingerprints map[string]string
	sessionFingerprints := make(map[string]string, len(sessionByID))
	if !full {
		var bErr error
		priorFingerprints, _, _, bErr = readBoundaryAndFingerprints(
			state, lastPush,
		)
		if bErr != nil {
			return result, bErr
		}
	}
	for _, id := range reconciledScopeMoveIDs {
		delete(priorFingerprints, id)
	}

	if err := purgePGExcludedPushSessions(
		ctx, s.pg, sessionByID,
	); err != nil {
		return result, err
	}

	usageFingerprints, err := s.local.UsageEventFingerprints(
		mapKeys(sessionByID),
	)
	if err != nil {
		return result, fmt.Errorf(
			"computing local usage event fingerprints: %w", err,
		)
	}
	// The fingerprint loop issues several local queries per candidate
	// session; on a full push that covers every session and runs for
	// minutes, so it reports its own progress phase rather than sitting
	// silent until the first batch lands.
	log.Printf("pgsync: computing push fingerprints for %d candidate session(s)",
		len(sessionByID))
	reportPrepare := func(done int) {
		if onProgress == nil {
			return
		}
		onProgress(PushProgress{
			Phase:         "preparing",
			SessionsDone:  done,
			SessionsTotal: len(sessionByID),
		})
	}
	reportPrepare(0)
	prepared := 0
	candidateIDs := mapKeys(sessionByID)
	for start := 0; start < len(candidateIDs); start += pushFingerprintBatchSize {
		end := min(start+pushFingerprintBatchSize, len(candidateIDs))
		chunk := candidateIDs[start:end]
		depState, err := readLocalPushDependencyState(ctx, s.local, chunk)
		if err != nil {
			return result, err
		}
		for _, id := range chunk {
			usageFP, usageKnown := usageFingerprints[id]
			dependencyFP, err := depState.dependencyFingerprint(
				s.local, id, usageFP, usageKnown,
			)
			if err != nil {
				return result, fmt.Errorf(
					"computing local dependency fingerprint %s: %w",
					id, err,
				)
			}
			sess := sessionByID[id]
			sessionFingerprints[id] = sessionPushFingerprint(
				sess, pushedSessionMachine(sess, s.machine),
				s.archiveID, usageFP, markerID,
				dependencyFP+"\x00source-database-generation:"+
					s.databaseGeneration,
			)
			prepared++
			if prepared%pushPrepareProgressStride == 0 {
				reportPrepare(prepared)
			}
		}
	}
	reportPrepare(prepared)

	if len(priorFingerprints) > 0 {
		for id := range sessionByID {
			if priorFingerprints[id] == sessionFingerprints[id] {
				delete(sessionByID, id)
			}
		}
	}

	var sessions []db.Session
	for _, sess := range sessionByID {
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	// Non-nil only for change-scoped pushes: sessionByID now holds
	// exactly this push's changed relational sessions, and the vector
	// phase reads state only for them. full is the effective value —
	// a promoted full push keeps generation-wide reconciliation.
	var vectorScope []string
	if opts.ScopeVectorsToChangedSessions && !full {
		vectorScope = mapKeys(sessionByID)
	}

	if len(sessions) == 0 {
		if s.isFiltered() {
			// Filtered pushes use filter-scoped sync state, so
			// they can advance their own watermark without
			// moving the unfiltered/global cursor.
			if err := finalizeFilteredPushState(
				state, lastPush, cutoff, sessions,
				priorFingerprints, sessionFingerprints,
				result.Errors,
			); err != nil {
				return result, err
			}
		} else {
			if err := finalizeUnfilteredPushState(
				state, lastPush, cutoff, sessions,
				priorFingerprints, sessionFingerprints,
				result.Errors,
			); err != nil {
				return result, err
			}
		}
		if err := persistPushTargetFingerprint(
			state, s.targetFingerprint,
		); err != nil {
			return result, err
		}
		if err := s.writePushMarker(
			ctx, markerID, markerMachine, markerMachineAliases,
		); err != nil {
			return result, err
		}
		if err := completeSessionAliasBackfill(
			aliasBackfillState, aliasBackfillNeeded, result,
		); err != nil {
			return result, err
		}
		if err := completeSessionProvenanceBackfill(
			provenanceBackfillState, provenanceBackfillNeeded, result,
		); err != nil {
			return result, err
		}
		if err := completeTranscriptRevisionBackfill(
			state, transcriptRevisionBackfillNeeded, result,
		); err != nil {
			return result, err
		}
		if err := s.syncProjectIdentityObservations(
			ctx, full, identityRefreshSessionIDs,
		); err != nil {
			return result, err
		}
		if err := s.syncWorktreeMappings(ctx, full); err != nil {
			return result, err
		}
		if err := s.finalizeSourceArchiveRepair(
			ctx, state, repairedPreviousArchiveID,
		); err != nil {
			return result, err
		}
		result.Vectors, err = s.runVectorPushPhase(
			ctx, full, vectorScope,
			opts.LastReconciledVectorGeneration, nil, onProgress,
		)
		if err != nil {
			return result, err
		}
		result.Duration = time.Since(start)
		return result, nil
	}

	var pushed []db.Session
	// Sessions whose individual retry also failed: their PG sessions/messages
	// rows are stale or absent, so the vector phase must not push their newer
	// local vectors ahead of them.
	var failedSessions map[string]struct{}
	const batchSize = 50
	for i := 0; i < len(sessions); i += batchSize {
		end := min(i+batchSize, len(sessions))
		batch := sessions[i:end]

		batchResult, err := s.pushBatch(
			ctx, batch, markerID, legacyMarkerMachines, &pushed,
		)
		if err != nil {
			return result, err
		}
		if batchResult.ok {
			result.SessionsPushed += batchResult.sessions
			result.MessagesPushed += batchResult.messages
			result.SkippedConflicts += batchResult.skippedConflicts
		} else {
			// Batch failed — retry each session individually
			// so one bad session doesn't block the rest.
			for _, sess := range batch {
				sr, retryErr := s.pushBatch(
					ctx, []db.Session{sess},
					markerID, legacyMarkerMachines, &pushed,
				)
				if retryErr != nil {
					return result, retryErr
				}
				if sr.ok {
					result.SessionsPushed += sr.sessions
					result.MessagesPushed += sr.messages
					result.SkippedConflicts += sr.skippedConflicts
				} else {
					result.Errors++
					if failedSessions == nil {
						failedSessions = make(map[string]struct{})
					}
					failedSessions[sess.ID] = struct{}{}
				}
			}
		}
		if onProgress != nil {
			onProgress(PushProgress{
				SessionsDone:     end,
				SessionsTotal:    len(sessions),
				MessagesDone:     result.MessagesPushed,
				SkippedConflicts: result.SkippedConflicts,
				Errors:           result.Errors,
			})
		}
	}

	if s.isFiltered() {
		// Filtered pushes use filter-scoped sync state, so
		// they can advance their own watermark without moving
		// the unfiltered/global cursor.
		if err := finalizeFilteredPushState(
			state, lastPush, cutoff, pushed,
			priorFingerprints, sessionFingerprints,
			result.Errors,
		); err != nil {
			return result, err
		}
	} else {
		if err := finalizeUnfilteredPushState(
			state, lastPush, cutoff, pushed,
			priorFingerprints, sessionFingerprints,
			result.Errors,
		); err != nil {
			return result, err
		}
	}
	if err := persistPushTargetFingerprint(
		state, s.targetFingerprint,
	); err != nil {
		return result, err
	}
	// Write the push marker only after the push and local finalization
	// succeed. A reset-recovery push that fails before this point leaves
	// the marker absent, so the next push re-detects the reset and retries
	// rather than skipping the still-missing sessions.
	if err := s.writePushMarker(
		ctx, markerID, markerMachine, markerMachineAliases,
	); err != nil {
		return result, err
	}
	if err := completeSessionAliasBackfill(
		aliasBackfillState, aliasBackfillNeeded, result,
	); err != nil {
		return result, err
	}
	if err := completeSessionProvenanceBackfill(
		provenanceBackfillState, provenanceBackfillNeeded, result,
	); err != nil {
		return result, err
	}
	if err := completeTranscriptRevisionBackfill(
		state, transcriptRevisionBackfillNeeded, result,
	); err != nil {
		return result, err
	}
	if result.Errors == 0 {
		if err := s.syncProjectIdentityObservations(
			ctx, full, identityRefreshSessionIDs,
		); err != nil {
			return result, err
		}
		if err := s.syncWorktreeMappings(ctx, full); err != nil {
			return result, err
		}
		if err := s.finalizeSourceArchiveRepair(
			ctx, state, repairedPreviousArchiveID,
		); err != nil {
			return result, err
		}
	} else {
		log.Printf(
			"pgsync: skipping project identity and mapping publication after %d session push errors",
			result.Errors,
		)
	}
	result.Vectors, err = s.runVectorPushPhase(
		ctx, full, vectorScope,
		opts.LastReconciledVectorGeneration, failedSessions, onProgress,
	)
	if err != nil {
		return result, err
	}
	result.Duration = time.Since(start)
	return result, nil
}

// runVectorPushPhase runs the vector push phase and wraps its error. With no
// source attached the phase never runs: it returns a Skipped result with an
// empty reason, which the summary printer renders as nothing (an unconfigured
// phase is not a diagnosable skip like an unavailable extension). Without this
// the zero-valued VectorPushResult would print "Vectors: 0 session(s) pushed".
// failedSessions names sessions whose session-phase push failed; their vectors
// are deferred so pgvector data never runs ahead of the sessions/messages rows.
// full bypasses the unchanged-hash skip so a --full push also repairs vector
// rows whose push state wrongly reports them current. scope, when non-nil,
// limits reconciliation to those session IDs (empty means no vector work);
// nil keeps the generation-wide read. onProgress, when non-nil, receives
// Phase "vectors" reports as the delta scan advances.
func (s *Sync) runVectorPushPhase(
	ctx context.Context, full bool, scope []string,
	lastReconciledGeneration int64,
	failedSessions map[string]struct{},
	onProgress func(PushProgress),
) (VectorPushResult, error) {
	if s.vectorSource == nil {
		return VectorPushResult{Skipped: true}, nil
	}
	res, err := s.pushVectors(
		ctx, full, scope, lastReconciledGeneration,
		failedSessions, onProgress,
	)
	if err != nil {
		return res, fmt.Errorf("vector push: %w", err)
	}
	return res, nil
}

func (s *Sync) syncProjectIdentityObservations(
	ctx context.Context, force bool, refreshSessionIDs []string,
) error {
	revision, err := s.local.ProjectIdentityPublicationRevision(ctx)
	if err != nil {
		return err
	}
	databaseGeneration, err := s.local.GetDatabaseID(ctx)
	if err != nil {
		return fmt.Errorf("loading source database generation: %w", err)
	}
	revisionValue := strconv.FormatInt(revision, 10)
	state := s.effectiveSyncState()
	stateKey := projectIdentityPublicationStateKey + ":" + databaseGeneration
	publishedRevisionValue, err := state.GetSyncState(stateKey)
	if err != nil {
		return fmt.Errorf("reading project identity publication revision: %w", err)
	}
	adoptLegacyFilteredScope := false
	if s.isFiltered() && publishedRevisionValue == "" {
		legacyValue, loadErr := state.GetSyncState(
			legacyProjectIdentityStateKey + ":" + databaseGeneration,
		)
		if loadErr != nil {
			return fmt.Errorf(
				"reading legacy project identity publication revision: %w",
				loadErr,
			)
		}
		adoptLegacyFilteredScope = legacyValue != ""
	}
	fullPublication := force || publishedRevisionValue == ""
	var publishedRevision int64
	if !fullPublication {
		publishedRevision, err = strconv.ParseInt(publishedRevisionValue, 10, 64)
		if err != nil || publishedRevision < 0 || publishedRevision > revision {
			fullPublication = true
		} else if publishedRevision == revision &&
			len(refreshSessionIDs) == 0 {
			return nil
		}
	}

	var observations []export.ProjectIdentityObservation
	var snapshots []export.ProjectIdentityObservation
	var delta db.ProjectIdentityPublicationDelta
	if fullPublication {
		observations, err = s.local.ListProjectIdentityObservations(ctx, nil)
		if err != nil {
			return fmt.Errorf("loading project identity observations: %w", err)
		}
		observations = filterProjectIdentityObservations(
			observations, s.projects, s.excludeProjects,
		)
		snapshots, err =
			s.local.ListPublishableSessionProjectIdentitySnapshots(
				ctx, nil, s.projects, s.excludeProjects,
			)
		if err != nil {
			return fmt.Errorf("loading session project identity snapshots: %w", err)
		}
	} else {
		delta, err = s.local.LoadProjectIdentityPublicationDelta(
			ctx, publishedRevision, revision, s.projects, s.excludeProjects,
		)
		if err != nil {
			return err
		}
		observations = delta.Observations
		snapshots = delta.Snapshots
	}
	if len(refreshSessionIDs) > 0 {
		refreshSnapshots, loadErr :=
			s.local.ListPublishableSessionProjectIdentitySnapshots(
				ctx, refreshSessionIDs, s.projects, s.excludeProjects,
			)
		if loadErr != nil {
			return fmt.Errorf(
				"loading refreshed session project identity snapshots: %w",
				loadErr,
			)
		}
		snapshots = mergeProjectIdentitySnapshots(snapshots, refreshSnapshots)
	}

	archiveID, err := s.local.GetArchiveID(ctx)
	if err != nil {
		return fmt.Errorf("loading source archive id: %w", err)
	}
	archiveSalt, err := s.local.GetArchiveSalt(ctx)
	if err != nil {
		return fmt.Errorf("loading source archive salt: %w", err)
	}
	log.Printf(
		"pgsync: syncing %d project identity observation(s), %d snapshot(s), "+
			"and %d tombstone(s)",
		len(observations), len(snapshots),
		len(delta.ObservationDeletes)+len(delta.SnapshotDeletes),
	)
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning project identity observation sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertSourceArchiveScope(ctx, tx, archiveID, archiveSalt); err != nil {
		return err
	}
	publicationScope := unfilteredPublicationScope
	if s.isFiltered() {
		publicationScope = pushSyncStateScope(
			"", s.projects, s.excludeProjects,
		)
		if err := prepareFilteredProjectIdentityPublication(
			ctx, tx, archiveID, databaseGeneration, publicationScope,
			fullPublication, adoptLegacyFilteredScope,
			s.projects, s.excludeProjects,
			delta.ObservationDeletes, delta.SnapshotDeletes, refreshSessionIDs,
		); err != nil {
			return err
		}
	} else if fullPublication {
		// Rebuild the archive from the destination's own rows. This removes
		// stale out-of-scope identity without loading or transmitting
		// excluded-project tombstone metadata.
		if err := deleteProjectIdentityArchive(
			ctx, tx, archiveID,
		); err != nil {
			return err
		}
	} else if err := deleteProjectIdentityDelta(
		ctx, tx, archiveID, databaseGeneration,
		delta.ObservationDeletes, delta.SnapshotDeletes,
	); err != nil {
		return err
	}
	if !s.isFiltered() {
		if err := deleteSessionProjectIdentitySnapshotsBySessionID(
			ctx, tx, archiveID, refreshSessionIDs,
		); err != nil {
			return err
		}
	}
	for i, obs := range observations {
		obs.SourceArchiveID = archiveID
		obs.SourceArchiveSalt = archiveSalt
		observations[i] = export.SanitizeStoredProjectIdentityObservation(obs)
	}
	if err := syncProjectIdentityObservationsBatch(
		ctx, tx, observations,
	); err != nil {
		return fmt.Errorf("syncing project identity observations: %w", err)
	}
	if err := ownProjectIdentityObservations(
		ctx, tx, archiveID, publicationScope, observations,
	); err != nil {
		return err
	}
	for i := range snapshots {
		snapshots[i] = export.SanitizeStoredProjectIdentityObservation(snapshots[i])
	}
	if err := insertSessionProjectIdentitySnapshots(
		ctx, tx, archiveID, databaseGeneration, snapshots,
	); err != nil {
		return fmt.Errorf("syncing session project identity snapshots: %w", err)
	}
	if err := ownSessionProjectIdentitySnapshots(
		ctx, tx, archiveID, databaseGeneration, publicationScope, snapshots,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity observation sync: %w", err)
	}
	if err := state.SetSyncState(stateKey, revisionValue); err != nil {
		return fmt.Errorf("recording project identity publication revision: %w", err)
	}
	return nil
}

func mergeProjectIdentitySnapshots(
	base, refresh []export.ProjectIdentityObservation,
) []export.ProjectIdentityObservation {
	merged := make(map[string]export.ProjectIdentityObservation, len(base)+len(refresh))
	for _, snapshot := range base {
		merged[snapshot.SessionID] = snapshot
	}
	for _, snapshot := range refresh {
		merged[snapshot.SessionID] = snapshot
	}
	out := make([]export.ProjectIdentityObservation, 0, len(merged))
	for _, snapshot := range merged {
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func filterProjectIdentityObservations(
	observations []export.ProjectIdentityObservation,
	projects []string,
	excludeProjects []string,
) []export.ProjectIdentityObservation {
	if len(projects) == 0 && len(excludeProjects) == 0 {
		return observations
	}
	out := observations[:0]
	for _, obs := range observations {
		if len(projects) > 0 && !slices.Contains(projects, obs.Project) {
			continue
		}
		if slices.Contains(excludeProjects, obs.Project) {
			continue
		}
		out = append(out, obs)
	}
	return out
}

// pgPushMarkerMachineState reports whether this host's push marker is present
// in PG and returns the current machine plus legacy machine aliases stored with
// the marker.
//
// Scoped marker keys are authoritative for reset detection. If a scoped marker
// is missing, legacy unscoped marker metadata is still returned as alias
// history so ownerless rows from older agentsview versions can be adopted after
// a machine rename.
// A missing marker while the local watermark is set means PG was reset (schema
// dropped or recreated) since this host last pushed, so a full re-push is
// needed. Counting rows by machine cannot detect this reliably: another host
// pushing to the same PG can repopulate rows under a machine value this host
// also writes -- a remote host's sessions synced in over SSH, or this host's
// own renamed identity -- masking the loss of this host's own rows. The marker
// is per-local-DB, so no other pusher can satisfy this check.
func (s *Sync) pgPushMarkerMachineState(
	ctx context.Context, markerID string,
) (string, []string, bool, error) {
	markerKey := s.pushMarkerMetadataKey(pushMarkerKeyPrefix, markerID)
	machine, markerExists, err := s.pgPushMarkerMetadataValue(
		ctx, markerKey,
	)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"checking pg push marker: %w", err,
		)
	}
	if markerExists {
		aliases, err := s.pgPushMarkerMachineAliases(
			ctx,
			s.pushMarkerMetadataKey(
				pushMarkerMachineAliasesKeyPrefix, markerID,
			),
		)
		if err != nil {
			return "", nil, false, err
		}
		return machine, aliases, true, nil
	}
	if s.syncStateTarget == "" {
		return "", nil, false, nil
	}

	legacyMachine, legacyMarkerExists, err := s.pgPushMarkerMetadataValue(
		ctx, pushMarkerKeyPrefix+markerID,
	)
	if err != nil {
		return "", nil, false, fmt.Errorf(
			"checking legacy pg push marker: %w", err,
		)
	}
	aliases, err := s.pgPushMarkerMachineAliases(
		ctx, pushMarkerMachineAliasesKeyPrefix+markerID,
	)
	if err != nil {
		return "", nil, false, err
	}
	if !legacyMarkerExists && len(aliases) == 0 {
		return "", nil, false, nil
	}
	return legacyMachine, aliases, false, nil
}

func (s *Sync) pgPushMarkerMetadataValue(
	ctx context.Context, key string,
) (string, bool, error) {
	var value string
	err := s.pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		key,
	).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		if isUndefinedTable(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func (s *Sync) pgPushMarkerMachineAliases(
	ctx context.Context, key string,
) ([]string, error) {
	var raw string
	err := s.pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		key,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"reading pg push marker machine aliases: %w", err,
		)
	}
	var aliases []string
	if err := json.Unmarshal([]byte(raw), &aliases); err != nil {
		return nil, fmt.Errorf(
			"decoding pg push marker machine aliases: %w", err,
		)
	}
	return normalizePushMarkerMachineAliases("", aliases), nil
}

// writePushMarker records this host's push marker in PG so a later push can
// tell whether PG still holds the rows this host pushed. The primary marker
// value carries the current machine name for debugging and reset detection;
// the alias key preserves previous marker machines so ownerless legacy rows can
// be adopted after renames across multiple incremental pushes.
func (s *Sync) writePushMarker(
	ctx context.Context,
	markerID, previousMarkerMachine string,
	previousAliases []string,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin push marker tx: %w", err)
	}
	aliases := pushMarkerMachineAliases(
		s.machine, previousMarkerMachine, previousAliases,
	)
	aliasesJSON, err := json.Marshal(aliases)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("encoding pg push marker machine aliases: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		s.pushMarkerMetadataKey(pushMarkerKeyPrefix, markerID), s.machine,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writing pg push marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		s.pushMarkerMetadataKey(pushMarkerMachineAliasesKeyPrefix, markerID),
		string(aliasesJSON),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writing pg push marker machine aliases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg push marker: %w", err)
	}
	return nil
}

func (s *Sync) pushMarkerMetadataKey(prefix, markerID string) string {
	if s.syncStateTarget == "" {
		return prefix + markerID
	}
	sum := sha256.Sum256([]byte(s.syncStateTarget))
	return prefix + markerID + ":scope:" + hex.EncodeToString(sum[:8])
}

func pushMarkerLegacyMachines(machine string, aliases []string) []string {
	machines := append([]string{}, aliases...)
	if machine != "" {
		machines = append(machines, machine)
	}
	return normalizePushMarkerMachineAliases("", machines)
}

func pushMarkerMachineAliases(
	currentMachine, previousMachine string,
	previousAliases []string,
) []string {
	aliases := append([]string{}, previousAliases...)
	if previousMachine != "" && previousMachine != currentMachine {
		aliases = append(aliases, previousMachine)
	}
	return normalizePushMarkerMachineAliases(currentMachine, aliases)
}

func normalizePushMarkerMachineAliases(
	currentMachine string, aliases []string,
) []string {
	seen := make(map[string]struct{}, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias == "" || alias == currentMachine {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, alias)
	}
	return out
}

// pushMarkerID returns this local DB's stable push-marker identifier, creating
// and persisting a random one on first use. It is independent of the machine
// name, so a machine rename keeps the same marker, and unique per local DB, so
// a different host pushing to the same PG cannot mask this host's reset.
func (s *Sync) pushMarkerID() (string, error) {
	state := s.local
	if state == nil {
		return "", fmt.Errorf("local db is required")
	}
	id, err := state.GetSyncState(pushMarkerIDStateKey)
	if err != nil {
		return "", fmt.Errorf("reading push marker id: %w", err)
	}
	if id != "" {
		return id, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating push marker id: %w", err)
	}
	id = hex.EncodeToString(buf)
	storedID, err := state.GetOrCreateSyncState(
		pushMarkerIDStateKey, id,
	)
	if err != nil {
		return "", fmt.Errorf("persisting push marker id: %w", err)
	}
	return storedID, nil
}

type batchResult struct {
	ok               bool
	sessions         int
	messages         int
	skippedConflicts int
}

// pushBatch pushes a slice of sessions within a single
// transaction. On success it appends to pushed and returns
// ok=true with session/message counts. On a session-level
// error it rolls back and returns ok=false so the caller
// can retry individually. Fatal errors (BeginTx failure)
// return a non-nil error.
func (s *Sync) pushBatch(
	ctx context.Context,
	batch []db.Session,
	markerID string,
	legacyMarkerMachines []string,
	pushed *[]db.Session,
) (batchResult, error) {
	bunTx, err := s.bunDB().BeginTx(ctx, nil)
	if err != nil {
		return batchResult{}, fmt.Errorf(
			"begin pg tx: %w", err,
		)
	}
	tx := bunTx.Tx

	n := 0
	msgs := 0
	skippedConflicts := 0
	for _, sess := range batch {
		snapshot, err := s.local.ReadSessionReplicationSnapshot(ctx, sess.ID)
		if err != nil {
			log.Printf("pgsync: session %s snapshot: %v", sess.ID, err)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}
		if err := s.pushSession(
			ctx, tx, snapshot.Session, markerID, legacyMarkerMachines,
		); err != nil {
			if errors.Is(err, errSessionOwnershipConflict) {
				skippedConflicts++
				continue
			}
			if errors.Is(err, errSessionExcluded) {
				continue
			}
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		msgCount, err := s.replacePGReplicationSnapshot(
			ctx, tx, bunTx, snapshot,
		)
		if err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		findingsChanged, err := s.pushSecretFindings(
			ctx, bunTx, sess.ID, snapshot.SecretFindings,
		)
		if err != nil {
			log.Printf(
				"pgsync: secret findings %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		// Bump updated_at when messages or secret findings were
		// rewritten but pushSession was a metadata no-op (its
		// WHERE clause skips unchanged rows). PG read-mode session
		// watchers rely on updated_at to surface secret-only changes.
		if msgCount > 0 || findingsChanged {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET updated_at = NOW()
				WHERE id = $1`,
				sess.ID,
			); err != nil {
				log.Printf(
					"pgsync: bumping updated_at %s: %v",
					sess.ID, err,
				)
				_ = tx.Rollback()
				*pushed = (*pushed)[:len(*pushed)-n]
				return batchResult{}, nil
			}
		}

		*pushed = append(*pushed, snapshot.Session)
		n++
		msgs += msgCount
	}

	if err := tx.Commit(); err != nil {
		log.Printf(
			"pgsync: batch commit failed: %v", err,
		)
		*pushed = (*pushed)[:len(*pushed)-n]
		return batchResult{}, nil
	}
	return batchResult{ok: true, sessions: n, messages: msgs, skippedConflicts: skippedConflicts}, nil
}

func (s *Sync) replacePGReplicationSnapshot(
	ctx context.Context, tx *sql.Tx, bunTx bun.IDB,
	snapshot db.SessionReplicationSnapshot,
) (int, error) {
	messageRows, callRows, resultRows, err := db.CanonicalMessageRows(snapshot.Messages)
	if err != nil {
		return 0, err
	}
	usageRows, err := db.CanonicalUsageEventRows(snapshot.UsageEvents)
	if err != nil {
		return 0, err
	}
	matches, err := db.CanonicalSessionDependentRowsMatch(
		ctx, bunTx, snapshot.Session.ID, messageRows, callRows, resultRows, usageRows,
	)
	if err != nil {
		return 0, err
	}
	if matches {
		if err := reconcilePinnedMessages(ctx, tx, snapshot.Session.ID); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if err := db.ReplaceMessageRows(
		ctx, bunTx, snapshot.Session.ID, messageRows,
	); err != nil {
		return 0, err
	}
	if err := db.ReplaceToolRows(
		ctx, bunTx, snapshot.Session.ID, callRows, resultRows,
	); err != nil {
		return 0, err
	}
	if err := db.ReplaceUsageEventRows(
		ctx, bunTx, snapshot.Session.ID, usageRows,
	); err != nil {
		return 0, err
	}
	if err := reconcilePinnedMessages(ctx, tx, snapshot.Session.ID); err != nil {
		return 0, err
	}
	return len(snapshot.Messages), nil
}

func finalizePushState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) error {
	if err := local.SetSyncState(
		"last_push_at", cutoff,
	); err != nil {
		return fmt.Errorf("updating last_push_at: %w", err)
	}
	return writePushBoundaryState(
		local, cutoff, sessions, priorFingerprints,
		sessionFingerprints,
	)
}

func finalizeUnfilteredPushState(
	local syncStateStore,
	lastPush, cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
	errors int,
) error {
	// When all sessions succeeded, advance the watermark to cutoff.
	// When some failed, keep the watermark at lastPush so the failed
	// sessions (plus any already-pushed ones) are re-evaluated next
	// time. Already-pushed sessions are fingerprint-matched and skipped
	// cheaply.
	finalizeCutoff := cutoff
	if errors > 0 {
		finalizeCutoff = lastPush
	}
	return finalizePushState(
		local, finalizeCutoff, sessions,
		priorFingerprints, sessionFingerprints,
	)
}

func finalizeFilteredPushState(
	local syncStateStore,
	lastPush, cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
	errors int,
) error {
	finalizeCutoff := cutoff
	if errors > 0 {
		finalizeCutoff = lastPush
	}
	return finalizePushState(
		local, finalizeCutoff, sessions,
		priorFingerprints, sessionFingerprints,
	)
}

// clearPushState resets the active watermark and boundary state so
// that the next push for this sync-state scope starts from scratch.
func clearPushState(local syncStateStore) error {
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, "",
	); err != nil {
		return fmt.Errorf(
			"clearing boundary state: %w", err,
		)
	}
	if err := local.SetSyncState(
		"last_push_at", "",
	); err != nil {
		return fmt.Errorf(
			"clearing last_push_at: %w", err,
		)
	}
	return nil
}

// retireSourceArchiveMetadata removes governance metadata that belongs to an
// archive identity superseded by a local repair. Filtered pushes release only
// their own publication scope, leaving other scopes intact until they repair.
func (s *Sync) retireSourceArchiveMetadata(
	ctx context.Context, archiveID string,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning old archive metadata retirement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.isFiltered() {
		publicationScope := pushSyncStateScope(
			"", s.projects, s.excludeProjects,
		)
		if err := releaseFilteredProjectIdentityFullOwnership(
			ctx, tx, archiveID, publicationScope,
		); err != nil {
			return err
		}
		if err := releaseFilteredWorktreeMappingFullOwnership(
			ctx, tx, archiveID, publicationScope,
		); err != nil {
			return err
		}
	} else {
		for _, table := range []string{
			"source_project_identity_observation_scopes",
			"source_session_project_identity_snapshot_scopes",
			"source_worktree_project_mapping_scopes",
			"source_project_identity_observations",
			"source_session_project_identity_snapshots",
			"source_worktree_project_mappings",
		} {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM "+table+" WHERE source_archive_id = $1",
				archiveID,
			); err != nil {
				return fmt.Errorf(
					"retiring old archive metadata from %s: %w", table, err,
				)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing old archive metadata retirement: %w", err)
	}
	return nil
}

func (s *Sync) finalizeSourceArchiveRepair(
	ctx context.Context,
	state syncStateStore,
	previousArchiveID string,
) error {
	if previousArchiveID != "" {
		if _, err := s.pg.ExecContext(ctx, `
			DELETE FROM source_archives archive
			WHERE archive.source_archive_id = $1
			  AND NOT EXISTS (
				SELECT 1 FROM sessions
				WHERE source_archive_id = archive.source_archive_id
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM source_project_identity_observations
				WHERE source_archive_id = archive.source_archive_id
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM source_session_project_identity_snapshots
				WHERE source_archive_id = archive.source_archive_id
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM source_worktree_project_mappings
				WHERE source_archive_id = archive.source_archive_id
			  )`, previousArchiveID); err != nil {
			return fmt.Errorf("cleaning up repaired source archive: %w", err)
		}
	}
	if err := persistPushSourceArchiveID(state, s.archiveID); err != nil {
		return err
	}
	return nil
}

func applySessionAliasBackfillRequirement(
	local syncStateStore, full bool,
) (bool, bool, error) {
	needed, err := sessionAliasBackfillNeeded(local)
	if err != nil {
		return full, false, err
	}
	if !needed {
		return full, false, nil
	}
	return true, true, nil
}

func sessionAliasBackfillNeeded(local syncStateStore) (bool, error) {
	done, err := local.GetSyncState(sessionAliasBackfillStateKey)
	if err != nil {
		return false, fmt.Errorf(
			"reading %s: %w", sessionAliasBackfillStateKey, err,
		)
	}
	return done != "1", nil
}

func markSessionAliasBackfillDone(local syncStateStore) error {
	if err := local.SetSyncState(
		sessionAliasBackfillStateKey, "1",
	); err != nil {
		return fmt.Errorf(
			"updating %s: %w", sessionAliasBackfillStateKey, err,
		)
	}
	return nil
}

func completeSessionAliasBackfill(
	local syncStateStore, needed bool, result PushResult,
) error {
	// Skipped ownership conflicts are sessions owned by another machine on
	// the hub; this host neither can nor should re-push them, so they do not
	// indicate an incomplete backfill of this host's own sessions. Only real
	// push errors (this host's own sessions that failed to push) should defer
	// the marker and re-force a full push. Gating on skipped conflicts made
	// the one-time alias backfill impossible to complete on any shared hub —
	// every push saw the marker missing and fell back to a full sweep.
	if !needed || result.Errors > 0 {
		return nil
	}
	return markSessionAliasBackfillDone(local)
}

func sessionProvenanceBackfillNeeded(local syncStateStore) (bool, error) {
	done, err := local.GetSyncState(sessionProvenanceBackfillStateKey)
	if err != nil {
		return false, fmt.Errorf(
			"reading session provenance backfill state: %w", err)
	}
	return done == "", nil
}

// applySessionProvenanceBackfillRequirement forces one full push while the
// provenance backfill marker is missing. Callers select the marker namespace:
// target-wide for unfiltered pushes, or effective-filter-scoped for filtered
// pushes, so each scope repairs its own fingerprint-matched rows exactly once.
func applySessionProvenanceBackfillRequirement(
	local syncStateStore, full bool,
) (bool, bool, error) {
	needed, err := sessionProvenanceBackfillNeeded(local)
	if err != nil {
		return full, false, err
	}
	if !needed {
		return full, false, nil
	}
	return true, true, nil
}

func markSessionProvenanceBackfillDone(local syncStateStore) error {
	if err := local.SetSyncState(
		sessionProvenanceBackfillStateKey, "1",
	); err != nil {
		return fmt.Errorf(
			"marking session provenance backfill done: %w", err)
	}
	return nil
}

// completeSessionProvenanceBackfill marks the caller-selected target or filter
// scope complete only after every session in that scope was pushed without an
// error.
func completeSessionProvenanceBackfill(
	local syncStateStore, needed bool, result PushResult,
) error {
	if !needed || result.Errors > 0 {
		return nil
	}
	return markSessionProvenanceBackfillDone(local)
}

func applyTranscriptRevisionBackfillRequirement(
	local syncStateStore, full bool,
) (bool, bool, error) {
	done, err := local.GetSyncState(transcriptRevisionBackfillStateKey)
	if err != nil {
		return full, false, fmt.Errorf(
			"reading %s: %w", transcriptRevisionBackfillStateKey, err,
		)
	}
	if done == "1" {
		return full, false, nil
	}
	return true, true, nil
}

func markTranscriptRevisionBackfillDone(local syncStateStore) error {
	if err := local.SetSyncState(
		transcriptRevisionBackfillStateKey, "1",
	); err != nil {
		return fmt.Errorf(
			"updating %s: %w", transcriptRevisionBackfillStateKey, err,
		)
	}
	return nil
}

func completeTranscriptRevisionBackfill(
	local syncStateStore, needed bool, result PushResult,
) error {
	if !needed || result.Errors > 0 {
		return nil
	}
	return markTranscriptRevisionBackfillDone(local)
}

func persistPushTargetFingerprint(
	local syncStateStore,
	fingerprint string,
) error {
	if err := local.SetSyncState(
		lastPushTargetFingerprintKey,
		fingerprint,
	); err != nil {
		return fmt.Errorf(
			"updating %s: %w",
			lastPushTargetFingerprintKey, err,
		)
	}
	return nil
}

func persistPushSourceArchiveID(local syncStateStore, archiveID string) error {
	if err := local.SetSyncState(lastPushSourceArchiveIDKey, archiveID); err != nil {
		return fmt.Errorf("updating %s: %w", lastPushSourceArchiveIDKey, err)
	}
	return nil
}

func pushTargetState(
	lastPush, boundaryState,
	storedTargetFingerprint, currentTargetFingerprint string,
) (bool, string) {
	if currentTargetFingerprint == "" {
		return false, ""
	}
	if lastPush == "" && boundaryState == "" {
		return false, ""
	}
	if storedTargetFingerprint == "" {
		return true,
			"local push state exists without a stored PG target fingerprint"
	}
	if storedTargetFingerprint != currentTargetFingerprint {
		return true, "PG target fingerprint changed"
	}
	return false, ""
}

func readBoundaryAndFingerprints(
	local syncStateStore,
	cutoff string,
) (
	fingerprints map[string]string,
	boundary map[string]string,
	boundaryOK bool,
	err error,
) {
	raw, err := local.GetSyncState(
		lastPushBoundaryStateKey,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf(
			"reading %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if raw == "" {
		return nil, nil, false, nil
	}
	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		return nil, nil, false, nil
	}
	fingerprints = state.Fingerprints
	if cutoff != "" &&
		state.Cutoff == cutoff &&
		state.Fingerprints != nil {
		boundary = state.Fingerprints
		boundaryOK = true
	}
	return fingerprints, boundary, boundaryOK, nil
}

func writePushBoundaryState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
	sessionFingerprints map[string]string,
) error {
	state := pushBoundaryState{
		Cutoff: cutoff,
		Fingerprints: make(
			map[string]string,
			len(priorFingerprints)+len(sessions),
		),
	}
	maps.Copy(state.Fingerprints, priorFingerprints)
	for _, sess := range sessions {
		fp, ok := sessionFingerprints[sess.ID]
		if !ok {
			return fmt.Errorf(
				"missing session fingerprint for %s",
				sess.ID,
			)
		}
		state.Fingerprints[sess.ID] = fp
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf(
			"encoding %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, string(data),
	); err != nil {
		return fmt.Errorf(
			"writing %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	return nil
}

func mapKeys(m map[string]db.Session) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func readPGExcludedSessionIDs(
	ctx context.Context, pg pgSessionQueryer, ids []string,
) (map[string]struct{}, error) {
	ids = uniqueNonEmptyStrings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	query, args := pgExcludedSessionIDsQuery(ids)
	rows, err := pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"reading pg excluded sessions: %w", err,
		)
	}
	defer rows.Close()

	excluded := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning pg excluded session id: %w", err,
			)
		}
		excluded[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating pg excluded sessions: %w", err,
		)
	}
	return excluded, nil
}

func pgExcludedSessionIDsQuery(ids []string) (string, []any) {
	return `SELECT id FROM excluded_sessions
			 WHERE id = ANY($1)`, []any{ids}
}

func purgePGExcludedPushSessions(
	ctx context.Context, pg *sql.DB, sessionByID map[string]db.Session,
) error {
	tombstoneIDsBySession := make(map[string][]string, len(sessionByID))
	candidateIDs := []string{}
	for id, sess := range sessionByID {
		tombstoneIDs := pgSessionTombstoneIDs(sess)
		tombstoneIDsBySession[id] = tombstoneIDs
		candidateIDs = append(candidateIDs, tombstoneIDs...)
	}
	excludedIDs, err := readPGExcludedSessionIDs(ctx, pg, candidateIDs)
	if err != nil {
		return err
	}
	if len(excludedIDs) == 0 {
		return nil
	}

	purgeIDs := []string{}
	for id, tombstoneIDs := range tombstoneIDsBySession {
		if !hasPGExcludedSessionID(tombstoneIDs, excludedIDs) {
			continue
		}
		purgeIDs = append(purgeIDs, tombstoneIDs...)
		delete(sessionByID, id)
	}
	purgeIDs = uniqueNonEmptyStrings(purgeIDs)
	if len(purgeIDs) == 0 {
		return nil
	}
	if err := insertPGExcludedSessionIDs(ctx, pg, purgeIDs); err != nil {
		return err
	}
	return deletePGExcludedSessionRows(ctx, pg, purgeIDs)
}

func reconcilePGProjectScopeMoves(
	ctx context.Context,
	pg *sql.DB,
	ownerMarker string,
	changedSessions []db.Session,
	projects []string,
	excludeProjects []string,
) ([]string, error) {
	if len(changedSessions) == 0 {
		return nil, nil
	}
	localProjects := make(map[string]string, len(changedSessions))
	changedIDs := make([]string, 0, len(changedSessions))
	for _, session := range changedSessions {
		localProjects[session.ID] = session.Project
		changedIDs = append(changedIDs, session.ID)
	}
	rows, err := pg.QueryContext(ctx, `
		SELECT id, project
		FROM sessions
		WHERE owner_marker = $1 AND id = ANY($2)`,
		ownerMarker, changedIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"listing changed pg sessions for scope reconciliation: %w", err,
		)
	}
	defer rows.Close()

	staleIDs := []string{}
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("scanning owned pg session for scope reconciliation: %w", err)
		}
		if !projectInPGSyncScope(project, projects, excludeProjects) {
			continue
		}
		if !projectInPGSyncScope(
			localProjects[id], projects, excludeProjects,
		) {
			staleIDs = append(staleIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating owned pg sessions for scope reconciliation: %w", err)
	}
	if len(staleIDs) == 0 {
		return nil, nil
	}
	sort.Strings(staleIDs)
	if _, err := pg.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE owner_marker = $1 AND id = ANY($2)`, ownerMarker, staleIDs); err != nil {
		return nil, fmt.Errorf("deleting pg sessions that moved out of scope: %w", err)
	}
	return staleIDs, nil
}

// listPGProjectScopeMoveCandidates returns the same incremental sync-marker
// window as the normal push, but without the project filter. A session that
// moves out of scope is absent from the filtered push window, so this bounded
// companion read is what lets reconciliation delete its formerly in-scope PG
// row. An empty watermark is the intentional one-time full-scan path.
func listPGProjectScopeMoveCandidates(
	ctx context.Context,
	local *db.DB,
	lastPush string,
) ([]db.Session, error) {
	return local.ListSessionsForMirrorWindow(ctx, lastPush, nil, nil)
}

func projectInPGSyncScope(
	project string,
	projects []string,
	excludeProjects []string,
) bool {
	if len(projects) > 0 && !slices.Contains(projects, project) {
		return false
	}
	return !slices.Contains(excludeProjects, project)
}

func hasPGExcludedSessionID(
	ids []string, excluded map[string]struct{},
) bool {
	for _, id := range ids {
		if _, ok := excluded[id]; ok {
			return true
		}
	}
	return false
}

type pgSessionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type pgSessionExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func deletePGExcludedSessionRows(
	ctx context.Context, pg pgSessionExecer, ids []string,
) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := pg.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ANY($1)`,
		ids,
	); err != nil {
		return fmt.Errorf("deleting pg excluded session rows: %w", err)
	}
	return nil
}

func deletePGSessionIfExcluded(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) (bool, error) {
	ids := pgSessionTombstoneIDs(sess)
	excluded, err := readPGExcludedSessionIDs(ctx, tx, ids)
	if err != nil {
		return false, err
	}
	if len(excluded) == 0 {
		return false, nil
	}
	if err := insertPGExcludedSessionIDs(ctx, tx, ids); err != nil {
		return false, err
	}
	if err := deletePGExcludedSessionRows(ctx, tx, ids); err != nil {
		return false, err
	}
	return true, nil
}

// sessionPushFingerprint builds the change-detection fingerprint for a
// session. pushedMachine is the value pushSession actually writes to PG
// (pushedSessionMachine), not the raw sess.Machine: a "local"/empty sentinel
// row is written under the fallback machine, so the fingerprint must track the
// fallback to force a re-push when s.machine changes.
func sessionPushFingerprint(
	sess db.Session, pushedMachine,
	sourceArchiveID, usageEventFingerprint, ownerMarker,
	dependencyFingerprint string,
) string {
	fields := []string{
		sess.ID,
		sess.Project,
		pushedMachine,
		sourceArchiveID,
		ownerMarker,
		dependencyFingerprint,
		sess.Agent,
		sess.AgentLabel,
		sess.Entrypoint,
		sess.SessionKind,
		stringValue(sess.FirstMessage),
		stringValue(sess.DisplayName),
		stringValue(sess.SessionName),
		stringValue(sess.StartedAt),
		stringValue(sess.EndedAt),
		stringValue(sess.DeletedAt),
		stringValue(sess.DeletionCause),
		fmt.Sprintf("%d", sess.MessageCount),
		fmt.Sprintf("%d", sess.UserMessageCount),
		fmt.Sprintf("%t", sess.IsAutomated),
		fmt.Sprintf("%d", sess.TotalOutputTokens),
		fmt.Sprintf("%d", sess.PeakContextTokens),
		fmt.Sprintf("%t", sess.HasTotalOutputTokens),
		fmt.Sprintf("%t", sess.HasPeakContextTokens),
		stringValue(sess.ParentSessionID),
		stringValue(sess.ParserParentSessionID),
		sess.RelationshipType,
		stringValue(sess.FilePath),
		stringValue(sess.FileHash),
		sess.CreatedAt,
		fmt.Sprintf("%d", sess.ToolFailureSignalCount),
		fmt.Sprintf("%d", sess.ToolRetryCount),
		fmt.Sprintf("%d", sess.EditChurnCount),
		fmt.Sprintf("%d", sess.ConsecutiveFailureMax),
		sess.Outcome,
		sess.OutcomeConfidence,
		sess.EndedWithRole,
		fmt.Sprintf("%d", sess.FinalFailureStreak),
		stringValue(sess.SignalsPendingSince),
		fmt.Sprintf("%d", sess.CompactionCount),
		fmt.Sprintf("%d", sess.MidTaskCompactionCount),
		float64Value(sess.ContextPressureMax),
		intPtrValue(sess.HealthScore),
		stringValue(sess.HealthGrade),
		fmt.Sprintf("%t", sess.HasToolCalls),
		fmt.Sprintf("%t", sess.HasContextData),
		fmt.Sprintf("%d", sess.QualitySignalVersion),
		fmt.Sprintf("%d", sess.ShortPromptCount),
		fmt.Sprintf("%t", sess.UnstructuredStart),
		fmt.Sprintf("%d", sess.MissingSuccessCriteriaCount),
		fmt.Sprintf("%d", sess.MissingVerificationCount),
		fmt.Sprintf("%d", sess.DuplicatePromptCount),
		fmt.Sprintf("%d", sess.NoCodeContextCount),
		fmt.Sprintf("%d", sess.RunawayToolLoopCount),
		fmt.Sprintf("%d", sess.DataVersion),
		sess.Cwd,
		sess.GitBranch,
		sess.SourceSessionID,
		sess.SourceVersion,
		sess.TranscriptFidelity,
		stringValue(sess.TranscriptRevision),
		fmt.Sprintf("%d", sess.ParserMalformedLines),
		fmt.Sprintf("%t", sess.IsTruncated),
		stringValue(sess.TerminationStatus),
		fmt.Sprintf("%d", sess.SecretLeakCount),
		sess.SecretsRulesVersion,
		usageEventFingerprint,
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return b.String()
}

// pushedSessionMachine resolves the machine field for a PG row. Old rows
// pushed before this fix with machine="local" will be repaired gradually as
// each session is modified (message count change, etc.) and re-fingerprinted.
func pushedSessionMachine(sess db.Session, fallbackMachine string) string {
	if sess.Machine != "" && sess.Machine != "local" {
		return sess.Machine
	}
	return fallbackMachine
}

func sameSessionOwner(
	existingOwnerMarker, existingMachine, markerID, pushedMachine string,
	legacyMarkerMachines []string,
) bool {
	if existingOwnerMarker != "" {
		return existingOwnerMarker == markerID
	}
	if existingMachine == "" {
		return true
	}
	if existingMachine == "local" {
		return true
	}
	if slices.Contains(legacyMarkerMachines, existingMachine) {
		return true
	}
	return existingMachine == pushedMachine
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func transcriptRevisionValue(value *string) string {
	if value == nil || *value == "" {
		return "0"
	}
	return *value
}

func float64Value(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%g", *value)
}

func intPtrValue(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

// nilStr converts a nil or empty *string to SQL NULL.
// Sanitizes before checking emptiness so strings like "\x00"
// that reduce to "" are correctly returned as NULL.
func nilStr(s *string) any {
	if s == nil {
		return nil
	}
	v := sanitizePG(*s)
	if v == "" {
		return nil
	}
	return v
}

// nilStrTS converts a nil or empty *string timestamp to a
// *time.Time for PG TIMESTAMPTZ columns.
func nilStrTS(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	t, ok := ParseSQLiteTimestamp(*s)
	if !ok {
		return nil
	}
	return t
}

// pushSession upserts a single session into PG.
// Local file metadata remains SQLite-only. The file hash is copied into the
// backend-neutral transcript_revision column so PG readers can observe
// transcript content changes without depending on local sync metadata.
func (s *Sync) pushSession(
	ctx context.Context, tx *sql.Tx, sess db.Session, markerID string,
	legacyMarkerMachines []string,
) error {
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	isAutomated := sess.IsAutomated
	pushedMachine := pushedSessionMachine(sess, s.machine)
	var existingMachine sql.NullString
	var existingOwnerMarker sql.NullString
	checkErr := tx.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sess.ID,
	).Scan(&existingMachine, &existingOwnerMarker)
	if checkErr != nil && !errors.Is(checkErr, sql.ErrNoRows) {
		return fmt.Errorf("checking session ownership %s: %w", sess.ID, checkErr)
	}
	if checkErr == nil && !sameSessionOwner(
		existingOwnerMarker.String,
		existingMachine.String,
		markerID,
		pushedMachine,
		legacyMarkerMachines,
	) {
		log.Printf(
			"pgsync: session %s: skipping — already owned by machine %q, "+
				"this pusher is %q; sync from the origin machine to update",
			sess.ID, existingMachine.String, pushedMachine,
		)
		return errSessionOwnershipConflict
	}
	if legacyMarkerMachines == nil {
		legacyMarkerMachines = []string{}
	}
	legacyMarkerMachinesJSON, err := json.Marshal(legacyMarkerMachines)
	if err != nil {
		return fmt.Errorf("encoding legacy marker machines: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent,
			first_message, display_name, source_display_name,
			session_name, created_at, started_at, ended_at,
			deleted_at, source_deleted_at, deletion_cause,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			is_automated, data_version,
			cwd, git_branch, source_session_id,
			source_version, parser_malformed_lines,
			is_truncated, termination_status,
			parent_session_id, parser_parent_session_id, relationship_type,
			tool_failure_signal_count, tool_retry_count,
			edit_churn_count, consecutive_failure_max,
			outcome, outcome_confidence,
			ended_with_role, final_failure_streak,
			signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max,
			health_score, health_grade,
			has_tool_calls, has_context_data,
			secret_leak_count, secrets_rules_version,
			quality_signal_version,
			short_prompt_count, unstructured_start,
			missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count,
			transcript_fidelity, transcript_revision,
			agent_label, entrypoint, session_kind,
			source_archive_id, source_database_generation, file_path,
			updated_at
			)
			SELECT
				$1, $2, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12, $13, $14, $15,
				$16, $17, $18, $19,
				$20, $21, $22, $23,
				$24, $25, $26, $27, $28, $29, $30,
				$31, $32, $33,
				$34, $35, $36, $37,
				$38, $39, $40, $41,
				$42,
				$43, $44,
				$45,
				$46, $47, $48, $49,
				$50, $51,
				$52, $53, $54, $55, $56, $57, $58, $59, $60, $61,
				$62, $63, $64, $65, $66, $67,
				NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM excluded_sessions WHERE id = $1
			)
			ON CONFLICT (id) DO UPDATE SET
			machine = EXCLUDED.machine,
			owner_marker = EXCLUDED.owner_marker,
			project = EXCLUDED.project,
			agent = EXCLUDED.agent,
			agent_label = EXCLUDED.agent_label,
			entrypoint = EXCLUDED.entrypoint,
			session_kind = EXCLUDED.session_kind,
			source_archive_id = EXCLUDED.source_archive_id,
			source_database_generation = EXCLUDED.source_database_generation,
			file_path = EXCLUDED.file_path,
			first_message = EXCLUDED.first_message,
			display_name = CASE
				WHEN sessions.display_name IS DISTINCT FROM
					sessions.source_display_name THEN sessions.display_name
				ELSE EXCLUDED.display_name
			END,
			source_display_name = EXCLUDED.display_name,
			session_name = EXCLUDED.session_name,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			deleted_at = CASE
				WHEN sessions.deleted_at IS DISTINCT FROM
					sessions.source_deleted_at THEN sessions.deleted_at
				ELSE EXCLUDED.deleted_at
			END,
			deletion_cause = CASE
				WHEN sessions.deleted_at IS DISTINCT FROM
					sessions.source_deleted_at THEN sessions.deletion_cause
				ELSE EXCLUDED.deletion_cause
			END,
			source_deleted_at = EXCLUDED.deleted_at,
			message_count = EXCLUDED.message_count,
			user_message_count = EXCLUDED.user_message_count,
			total_output_tokens = EXCLUDED.total_output_tokens,
			peak_context_tokens = EXCLUDED.peak_context_tokens,
			has_total_output_tokens = EXCLUDED.has_total_output_tokens,
			has_peak_context_tokens = EXCLUDED.has_peak_context_tokens,
			is_automated = EXCLUDED.is_automated,
			data_version = EXCLUDED.data_version,
			cwd = EXCLUDED.cwd,
			git_branch = EXCLUDED.git_branch,
			source_session_id = EXCLUDED.source_session_id,
			source_version = EXCLUDED.source_version,
			transcript_fidelity = EXCLUDED.transcript_fidelity,
			transcript_revision = EXCLUDED.transcript_revision,
			parser_malformed_lines = EXCLUDED.parser_malformed_lines,
			is_truncated = EXCLUDED.is_truncated,
			termination_status = EXCLUDED.termination_status,
			parent_session_id = EXCLUDED.parent_session_id,
			parser_parent_session_id = EXCLUDED.parser_parent_session_id,
			relationship_type = EXCLUDED.relationship_type,
			tool_failure_signal_count = EXCLUDED.tool_failure_signal_count,
			tool_retry_count = EXCLUDED.tool_retry_count,
			edit_churn_count = EXCLUDED.edit_churn_count,
			consecutive_failure_max = EXCLUDED.consecutive_failure_max,
			outcome = EXCLUDED.outcome,
			outcome_confidence = EXCLUDED.outcome_confidence,
			ended_with_role = EXCLUDED.ended_with_role,
			final_failure_streak = EXCLUDED.final_failure_streak,
			signals_pending_since = EXCLUDED.signals_pending_since,
			compaction_count = EXCLUDED.compaction_count,
			mid_task_compaction_count = EXCLUDED.mid_task_compaction_count,
			context_pressure_max = EXCLUDED.context_pressure_max,
			health_score = EXCLUDED.health_score,
			health_grade = EXCLUDED.health_grade,
			has_tool_calls = EXCLUDED.has_tool_calls,
			has_context_data = EXCLUDED.has_context_data,
			secret_leak_count = EXCLUDED.secret_leak_count,
			secrets_rules_version = EXCLUDED.secrets_rules_version,
			quality_signal_version = EXCLUDED.quality_signal_version,
			short_prompt_count = EXCLUDED.short_prompt_count,
			unstructured_start = EXCLUDED.unstructured_start,
			missing_success_criteria_count = EXCLUDED.missing_success_criteria_count,
			missing_verification_count = EXCLUDED.missing_verification_count,
			duplicate_prompt_count = EXCLUDED.duplicate_prompt_count,
			no_code_context_count = EXCLUDED.no_code_context_count,
			runaway_tool_loop_count = EXCLUDED.runaway_tool_loop_count,
			updated_at = NOW()
		WHERE ((
				sessions.owner_marker = ''
				AND (sessions.machine = EXCLUDED.machine
					OR sessions.machine = 'local'
					OR sessions.machine = ''
					OR sessions.machine IN (
						SELECT jsonb_array_elements_text($68::jsonb)
					))
			)
			OR sessions.owner_marker = EXCLUDED.owner_marker)
			AND NOT EXISTS (
				SELECT 1 FROM excluded_sessions
				WHERE id = EXCLUDED.id
			)
			AND (
			sessions.machine IS DISTINCT FROM EXCLUDED.machine
			OR sessions.owner_marker IS DISTINCT FROM EXCLUDED.owner_marker
			OR sessions.project IS DISTINCT FROM EXCLUDED.project
			OR sessions.agent IS DISTINCT FROM EXCLUDED.agent
			OR sessions.agent_label IS DISTINCT FROM EXCLUDED.agent_label
			OR sessions.entrypoint IS DISTINCT FROM EXCLUDED.entrypoint
			OR sessions.session_kind IS DISTINCT FROM EXCLUDED.session_kind
			OR sessions.source_archive_id IS DISTINCT FROM EXCLUDED.source_archive_id
			OR sessions.source_database_generation IS DISTINCT FROM
				EXCLUDED.source_database_generation
			OR sessions.file_path IS DISTINCT FROM EXCLUDED.file_path
			OR sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
			OR sessions.source_display_name IS DISTINCT FROM EXCLUDED.display_name
			OR sessions.session_name IS DISTINCT FROM EXCLUDED.session_name
			OR sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
			OR sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
			OR sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
			OR sessions.source_deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
			OR sessions.deletion_cause IS DISTINCT FROM EXCLUDED.deletion_cause
			OR sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
			OR sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
			OR sessions.total_output_tokens IS DISTINCT FROM EXCLUDED.total_output_tokens
			OR sessions.peak_context_tokens IS DISTINCT FROM EXCLUDED.peak_context_tokens
			OR sessions.has_total_output_tokens IS DISTINCT FROM EXCLUDED.has_total_output_tokens
			OR sessions.has_peak_context_tokens IS DISTINCT FROM EXCLUDED.has_peak_context_tokens
			OR sessions.is_automated IS DISTINCT FROM EXCLUDED.is_automated
			OR sessions.data_version IS DISTINCT FROM EXCLUDED.data_version
			OR sessions.cwd IS DISTINCT FROM EXCLUDED.cwd
			OR sessions.git_branch IS DISTINCT FROM EXCLUDED.git_branch
			OR sessions.source_session_id IS DISTINCT FROM EXCLUDED.source_session_id
			OR sessions.source_version IS DISTINCT FROM EXCLUDED.source_version
			OR sessions.transcript_fidelity IS DISTINCT FROM EXCLUDED.transcript_fidelity
			OR sessions.transcript_revision IS DISTINCT FROM EXCLUDED.transcript_revision
			OR sessions.parser_malformed_lines IS DISTINCT FROM EXCLUDED.parser_malformed_lines
			OR sessions.is_truncated IS DISTINCT FROM EXCLUDED.is_truncated
			OR sessions.termination_status IS DISTINCT FROM EXCLUDED.termination_status
			OR sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
			OR sessions.parser_parent_session_id IS DISTINCT FROM EXCLUDED.parser_parent_session_id
			OR sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type
			OR sessions.tool_failure_signal_count IS DISTINCT FROM EXCLUDED.tool_failure_signal_count
			OR sessions.tool_retry_count IS DISTINCT FROM EXCLUDED.tool_retry_count
			OR sessions.edit_churn_count IS DISTINCT FROM EXCLUDED.edit_churn_count
			OR sessions.consecutive_failure_max IS DISTINCT FROM EXCLUDED.consecutive_failure_max
			OR sessions.outcome IS DISTINCT FROM EXCLUDED.outcome
			OR sessions.outcome_confidence IS DISTINCT FROM EXCLUDED.outcome_confidence
			OR sessions.ended_with_role IS DISTINCT FROM EXCLUDED.ended_with_role
			OR sessions.final_failure_streak IS DISTINCT FROM EXCLUDED.final_failure_streak
			OR sessions.signals_pending_since IS DISTINCT FROM EXCLUDED.signals_pending_since
			OR sessions.compaction_count IS DISTINCT FROM EXCLUDED.compaction_count
			OR sessions.mid_task_compaction_count IS DISTINCT FROM EXCLUDED.mid_task_compaction_count
			OR sessions.context_pressure_max IS DISTINCT FROM EXCLUDED.context_pressure_max
			OR sessions.health_score IS DISTINCT FROM EXCLUDED.health_score
			OR sessions.health_grade IS DISTINCT FROM EXCLUDED.health_grade
			OR sessions.has_tool_calls IS DISTINCT FROM EXCLUDED.has_tool_calls
			OR sessions.has_context_data IS DISTINCT FROM EXCLUDED.has_context_data
			OR sessions.secret_leak_count IS DISTINCT FROM EXCLUDED.secret_leak_count
			OR sessions.secrets_rules_version IS DISTINCT FROM EXCLUDED.secrets_rules_version
			OR sessions.quality_signal_version IS DISTINCT FROM EXCLUDED.quality_signal_version
			OR sessions.short_prompt_count IS DISTINCT FROM EXCLUDED.short_prompt_count
			OR sessions.unstructured_start IS DISTINCT FROM EXCLUDED.unstructured_start
			OR sessions.missing_success_criteria_count IS DISTINCT FROM EXCLUDED.missing_success_criteria_count
			OR sessions.missing_verification_count IS DISTINCT FROM EXCLUDED.missing_verification_count
			OR sessions.duplicate_prompt_count IS DISTINCT FROM EXCLUDED.duplicate_prompt_count
			OR sessions.no_code_context_count IS DISTINCT FROM EXCLUDED.no_code_context_count
			OR sessions.runaway_tool_loop_count IS DISTINCT FROM EXCLUDED.runaway_tool_loop_count)`,
		sess.ID, pushedMachine, markerID,
		sanitizePG(sess.Project),
		sess.Agent,
		nilStr(sess.FirstMessage),
		nilStr(sess.DisplayName),
		nilStr(sess.DisplayName),
		nilStr(sess.SessionName),
		createdAt,
		nilStrTS(sess.StartedAt),
		nilStrTS(sess.EndedAt),
		nilStrTS(sess.DeletedAt),
		nilStrTS(sess.DeletedAt),
		nilStr(sess.DeletionCause),
		sess.MessageCount, sess.UserMessageCount,
		sess.TotalOutputTokens, sess.PeakContextTokens,
		sess.HasTotalOutputTokens, sess.HasPeakContextTokens,
		isAutomated, sess.DataVersion,
		sanitizePG(sess.Cwd), sanitizePG(sess.GitBranch),
		sanitizePG(sess.SourceSessionID),
		sanitizePG(sess.SourceVersion),
		sess.ParserMalformedLines,
		sess.IsTruncated, nilStr(sess.TerminationStatus),
		nilStr(sess.ParentSessionID),
		nilStr(sess.ParserParentSessionID),
		sess.RelationshipType,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sanitizePG(sess.EndedWithRole), sess.FinalFailureStreak,
		nilStr(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax,
		sess.HealthScore, nilStr(sess.HealthGrade),
		sess.HasToolCalls, sess.HasContextData,
		sess.SecretLeakCount, sess.SecretsRulesVersion,
		sess.QualitySignalVersion,
		sess.ShortPromptCount, sess.UnstructuredStart,
		sess.MissingSuccessCriteriaCount,
		sess.MissingVerificationCount, sess.DuplicatePromptCount,
		sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		sanitizePG(sess.TranscriptFidelity),
		transcriptRevisionValue(sess.TranscriptRevision),
		sanitizePG(sess.AgentLabel),
		sanitizePG(sess.Entrypoint),
		sanitizePG(sess.SessionKind),
		s.archiveID,
		s.databaseGeneration,
		sess.FilePath,
		string(legacyMarkerMachinesJSON),
	)
	if err != nil {
		return err
	}
	if rowsAffected, rowsErr := result.RowsAffected(); rowsErr == nil && rowsAffected == 0 {
		excluded, excludedErr := deletePGSessionIfExcluded(ctx, tx, sess)
		if excludedErr != nil {
			return excludedErr
		}
		if excluded {
			return errSessionExcluded
		}
		refreshErr := tx.QueryRowContext(ctx,
			`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sess.ID,
		).Scan(&existingMachine, &existingOwnerMarker)
		if refreshErr != nil {
			// The guarded upsert changed no rows and we cannot
			// re-read the current owner, so we cannot prove this
			// pusher owns the session. Surface the error instead of
			// reporting a blocked write as success, so the caller's
			// retry path handles it rather than pushing messages for
			// a row this pusher did not write.
			return fmt.Errorf(
				"re-reading session %s ownership after blocked upsert: %w",
				sess.ID, refreshErr,
			)
		}
		if !sameSessionOwner(
			existingOwnerMarker.String, existingMachine.String,
			markerID, pushedMachine, legacyMarkerMachines,
		) {
			log.Printf(
				"pgsync: session %s: skipping — already owned by machine %q, this pusher is %q; sync from the origin machine to update",
				sess.ID, existingMachine.String, pushedMachine,
			)
			return errSessionOwnershipConflict
		}
	}
	excluded, excludedErr := deletePGSessionIfExcluded(ctx, tx, sess)
	if excludedErr != nil {
		return excludedErr
	}
	if excluded {
		return errSessionExcluded
	}
	if err := replacePGSessionAliases(ctx, tx, sess); err != nil {
		return err
	}
	return nil
}

func reconcilePinnedMessages(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE pinned_messages p
		SET source_uuid = m.source_uuid
		FROM messages m
		WHERE p.session_id = $1
			AND m.session_id = p.session_id
			AND m.ordinal = p.message_id
			AND p.source_uuid = ''
			AND m.source_uuid <> ''`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"backfilling pg pin source_uuid: %w", err,
		)
	}

	// Move shifted source-backed pins out of the real ordinal range
	// first. Pins already on their resolved target stay in place so
	// duplicate repairs prefer the current target row's metadata.
	// When multiple messages share a source_uuid (the schema allows
	// it), prefer the message at the pin's current message_id so a
	// correctly-placed pin is not relocated to a different duplicate.
	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.ordinal,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		numbered AS (
			SELECT id,
				ROW_NUMBER() OVER (ORDER BY id) AS temp_ordinal
			FROM matched
			WHERE target_ordinal <> message_id
				OR target_ordinal <> ordinal
		)
		UPDATE pinned_messages p
		SET message_id = (-2000000000 + numbered.temp_ordinal::INT),
			ordinal = (-2000000000 + numbered.temp_ordinal::INT)
		FROM numbered
		WHERE p.id = numbered.id`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"staging pg pins for source_uuid realignment: %w", err,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.created_at,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		ranked AS (
			SELECT id, target_ordinal,
				ROW_NUMBER() OVER (
					PARTITION BY target_ordinal
					ORDER BY
						(message_id = target_ordinal) DESC,
						created_at DESC,
						id DESC
				) AS target_rank
			FROM matched
		)
		DELETE FROM pinned_messages p
		USING ranked r
		WHERE p.session_id = $1
			AND r.target_rank = 1
			AND p.message_id = r.target_ordinal
			AND p.id <> r.id`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"clearing pg pin target conflicts: %w", err,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		WITH matched AS (
			SELECT DISTINCT ON (p.id)
				p.id, p.message_id, p.created_at,
				m.ordinal AS target_ordinal
			FROM pinned_messages p
			JOIN messages m
				ON m.session_id = p.session_id
				AND m.source_uuid = p.source_uuid
			WHERE p.session_id = $1
				AND p.source_uuid <> ''
			ORDER BY p.id,
				CASE WHEN m.ordinal = p.message_id THEN 0 ELSE 1 END,
				m.ordinal
		),
		ranked AS (
			SELECT id, target_ordinal,
				ROW_NUMBER() OVER (
					PARTITION BY target_ordinal
					ORDER BY
						(message_id = target_ordinal) DESC,
						created_at DESC,
						id DESC
				) AS target_rank
			FROM matched
		)
		UPDATE pinned_messages p
		SET message_id = r.target_ordinal,
			ordinal = r.target_ordinal
		FROM ranked r
		WHERE p.id = r.id
			AND r.target_rank = 1`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"realigning pg pins by source_uuid: %w", err,
		)
	}

	// Prune pins whose anchor no longer exists. For source-backed
	// pins (source_uuid <> '') the canonical anchor is source_uuid,
	// so a pin must be dropped when no message in this session has
	// that source_uuid — otherwise a stale pin can survive on top
	// of an unrelated message that now occupies the same ordinal.
	// The ordinal-NOT-EXISTS clause additionally removes legacy
	// pins (source_uuid = '') with a stale ordinal and clears any
	// non-rank-1 duplicate left at the sentinel ordinal by step 2.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM pinned_messages p
		WHERE p.session_id = $1
			AND (
				(
					p.source_uuid <> ''
					AND NOT EXISTS (
						SELECT 1 FROM messages m
						WHERE m.session_id = p.session_id
							AND m.source_uuid = p.source_uuid
					)
				)
				OR NOT EXISTS (
					SELECT 1 FROM messages m
					WHERE m.session_id = p.session_id
						AND m.ordinal = p.message_id
				)
			)`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"pruning stale pg pins: %w", err,
		)
	}

	return nil
}

func localMessageRoleTimePGFingerprint(
	local *db.DB, sessionID string,
) (string, error) {
	return local.MessageRoleTimeFingerprintWithTimestampNormalizer(
		sessionID,
		pgPushTimestampFingerprintText,
	)
}

func pgPushTimestampFingerprintText(value string) string {
	t, ok := ParseSQLiteTimestamp(value)
	if !ok {
		return ""
	}
	return FormatISO8601(t.Truncate(time.Microsecond))
}

func bulkInsertCursorUsageEvents(
	ctx context.Context, tx *sql.Tx, events []db.CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	const cursorBatch = 100
	for i := 0; i < len(events); i += cursorBatch {
		end := min(i+cursorBatch, len(events))
		batch := events[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO cursor_usage_events (
			occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_microdollars, cursor_token_fee_microdollars,
			user_id, user_email, is_headless, dedup_key
		) VALUES `)
		args := make([]any, 0, len(batch)*13)
		for j, ev := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*13 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5, p+6,
				p+7, p+8, p+9, p+10, p+11, p+12,
			)
			occurredAt, ok := ParseSQLiteTimestamp(ev.OccurredAt)
			if !ok {
				return fmt.Errorf("parsing cursor usage occurred_at %q", ev.OccurredAt)
			}
			args = append(args,
				occurredAt,
				sanitizePG(ev.Model),
				sanitizePG(ev.Kind),
				ev.InputTokens,
				ev.OutputTokens,
				ev.CacheWriteTokens,
				ev.CacheReadTokens,
				ev.Charged.Microdollars,
				ev.CursorTokenFee.Microdollars,
				sanitizePG(ev.UserID),
				sanitizePG(ev.UserEmail),
				ev.IsHeadless,
				sanitizePG(ev.DedupKey),
			)
		}
		b.WriteString(` ON CONFLICT DO NOTHING`)
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting cursor_usage_events: %w", err)
		}
	}
	return nil
}

// pushSecretFindings replaces a session's secret findings in PG.
// It deletes all existing rows for the session then bulk-inserts
// the current local set. It reports whether it changed any rows
// (deleted existing or inserted new) so the caller can bump
// sessions.updated_at for secret-only changes that pushSession and
// pushMessages would otherwise miss. Per-finding rules_version is
// pushed via this table; the session-level
// sessions.secrets_rules_version is pushed by pushSession alongside
// the rest of the session columns.
func (s *Sync) pushSecretFindings(
	ctx context.Context, bunTx bun.IDB, sessionID string,
	findings []db.SecretFinding,
) (bool, error) {
	existing, err := bunTx.NewSelect().Model((*bunmodel.SecretFinding)(nil)).
		Where("session_id = ?", sessionID).Count(ctx)
	if err != nil {
		return false, fmt.Errorf("counting pg secret findings for %s: %w", sessionID, err)
	}
	if err := db.ReplaceSecretFindingRows(
		ctx, bunTx, sessionID, db.CanonicalSecretFindingRows(findings),
	); err != nil {
		return false, err
	}
	return existing > 0 || len(findings) > 0, nil
}

// normalizeSyncTimestamps ensures schema exists and normalizes
// local sync state timestamps.
func (s *Sync) normalizeSyncTimestamps(
	ctx context.Context,
) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if err := s.ensureSchemaLocked(ctx); err != nil {
		return err
	}
	return NormalizeLocalSyncStateTimestamps(s.effectiveSyncState())
}

// sanitizePG strips null bytes and replaces invalid UTF-8
// sequences so text can be safely inserted into PostgreSQL,
// which enforces strict UTF-8 encoding. It delegates to
// db.SanitizeUTF8 so the local fingerprint builders apply the
// exact same normalization.
func sanitizePG(s string) string {
	return db.SanitizeUTF8(s)
}

func nilIfEmpty(s string) any {
	s = sanitizePG(s)
	if s == "" {
		return nil
	}
	return s
}

func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	// Cursor admin rows are global and unattributed, so project-filtered pushes
	// cannot sync them honestly.
	if s.isFiltered() {
		return nil
	}

	// The PG push is explicit and on-demand, so it keeps the full-history
	// load (sinceID 0): the remote dedup index makes re-inserts no-ops and
	// there is no per-filesystem-event pressure to bound, unlike the DuckDB
	// automatic push which tracks a high-water id in mirror metadata.
	events, err := s.local.GetCursorUsageEvents(ctx, 0)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning cursor usage sync tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := bulkInsertCursorUsageEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg cursor usage sync: %w", err)
	}
	return nil
}
