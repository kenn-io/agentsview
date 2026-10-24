package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// vectorChunkInsertBatch caps rows per multi-row INSERT so parameter counts
// stay well under PG's 65535 bound (each chunk row binds 3 parameters).
const vectorChunkInsertBatch = 200

// vectorProgressStride bounds how many unchanged sessions the delta scan
// examines between progress reports; pushed sessions always report.
const vectorProgressStride = 2000

// vectorGeneration is the resolved PG generation this push targets: its id and
// the schema-qualified halfvec type. The type must be qualified because the
// connection's search_path is the target schema only, while pgvector's types
// live in whichever schema first installed the extension.
// machineRecorded reports whether this pusher's scoped witness record already
// existed for the generation before this push touched it: recreating the
// vector tables restarts the id sequence, so a reused id can satisfy the memo
// comparison while the recreated generation is empty, and the witness record
// — keyed by local push marker plus sync/filter scope and wiped in the same
// reset — is what survives id reuse safely.
type vectorGeneration struct {
	id                   int64
	createdAt            time.Time
	halfvecType          string
	machineRecorded      bool
	compatibilityColumns bool
}

// vectorPushScope carries the push-wide constants threaded through every
// per-session push and eviction: the target generation, its local fingerprint
// (for the pre-eviction re-check against the source), the id list of all
// generations whose chunk table exists (for the shared-doc cross-generation
// guard, already filtered so a missing table cannot abort a tx), and this
// pusher's owner identity. Bundling them keeps per-session helpers under the
// positional-parameter limit.
type vectorPushScope struct {
	gen    vectorGeneration
	genIDs []int64
	owner  vectorOwnerIdentity
}

// vectorPushStateRow is the PG-side delta state for one owned session in one
// generation, joined with the session's current owner marker and machine.
type vectorPushStateRow struct {
	docAggHash    string
	ownerMarker   string
	machine       string
	sessionExists bool
}

// vectorOwnerIdentity is the set of PG session identities this pusher treats
// as its own, mirroring the session phase's sameSessionOwner: a non-empty
// owner_marker decides ownership outright, while an empty marker (a legacy or
// unclaimed row) falls back to the sessions.machine column checked against
// this pusher's machine name and its recorded marker aliases. One divergence
// from the session phase is deliberate: sameSessionOwner compares against
// each pushed session's own machine field, which lets a legacy row be adopted
// through a locally mirrored remote session; the vector phase compares only
// against this pusher's identities, so a legacy row that names another
// machine is a conflict here even when a mirrored copy exists locally —
// deferring to that machine's own vector push rather than overwriting it.
type vectorOwnerIdentity struct {
	markerID       string
	machine        string
	legacyMachines []string
}

func (id vectorOwnerIdentity) owns(ownerMarker, machine string) bool {
	if ownerMarker != "" {
		return ownerMarker == id.markerID
	}
	if machine == "" || machine == "local" || machine == id.machine {
		return true
	}
	return slices.Contains(id.legacyMachines, machine)
}

// vectorOwnerIdentity resolves this push's owner identity: its marker id plus
// the machine name and marker-machine aliases used to adjudicate legacy PG
// rows whose owner_marker is empty.
func (s *Sync) vectorOwnerIdentity(
	ctx context.Context,
) (vectorOwnerIdentity, error) {
	markerID, err := s.pushMarkerID(ctx)
	if err != nil {
		return vectorOwnerIdentity{}, err
	}
	markerMachine, aliases, _, err := s.pgPushMarkerMachineState(ctx, markerID)
	if err != nil {
		return vectorOwnerIdentity{}, err
	}
	return vectorOwnerIdentity{
		markerID:       markerID,
		machine:        s.machine,
		legacyMachines: pushMarkerLegacyMachines(markerMachine, aliases),
	}, nil
}

// pushVectors replicates the local active generation's embeddings into PG,
// pushing only sessions whose aggregate hash changed and evicting state for
// sessions this pusher no longer has. It is a no-op (Skipped) when there is no
// source, no active generation, or no pgvector extension. Sessions are already
// committed by the caller when this runs; a failure here surfaces as a push
// error without undoing them. full bypasses the unchanged-hash skip, matching
// the session phase's --full semantics: a full push re-sends every session's
// vectors, repairing PG rows whose vector_push_state wrongly reports them
// current. failedSessions names sessions whose session-phase push failed this
// run: their vectors are deferred (counted in SessionsDeferred) so pgvector
// data never runs ahead of the corresponding sessions/messages rows.
// scope, when non-nil, limits the local hash read and the PG state read to
// those session IDs: reconciliation (including eviction) touches only the
// scoped sessions, and everything else — PG-only rows, vector-only changes
// from a later embeddings build — waits for the next generation-wide push.
// An empty non-nil scope returns immediately without reading anything.
// onProgress, when non-nil, receives Phase "vectors" reports so interactive
// pushes are not silent through a long first vector push.
func (s *Sync) pushVectors(
	ctx context.Context, full bool, scope []string,
	lastReconciledGeneration int64,
	failedSessions map[string]struct{},
	onProgress func(storage.PushProgress),
) (storage.VectorPushResult, error) {
	var res storage.VectorPushResult
	if s.vectorSource == nil {
		res.Skipped, res.SkippedReason = true, "no vector source configured"
		return res, nil
	}
	if scope != nil && len(scope) == 0 {
		log.Printf("vector push: no changed sessions; deferring reconciliation to the next generation-wide push")
		return res, nil
	}
	// A source that builds embeddings on demand prepares only after the
	// replica proves it can store and register its generation, so a push
	// that would skip vectors does no build work.
	baseSchemaReady := false
	if preparer, ok := s.vectorSource.(storage.VectorExportPreparer); ok {
		if skipped, err := s.prepareVectorBaseSchema(ctx, &res); err != nil || skipped {
			return res, err
		}
		baseSchemaReady = true
		planned, building, err := preparer.PlannedGeneration(ctx)
		if err != nil {
			return res, fmt.Errorf("planning local vector generation: %w", err)
		}
		if building {
			if _, err := s.registerVectorGeneration(ctx, planned); err != nil {
				if s.skipVectorsOnPrivilegeError(err, &res) {
					return res, nil
				}
				return res, err
			}
		}
		if err := preparer.PrepareExport(ctx); err != nil {
			return res, fmt.Errorf("preparing local vector generation: %w", err)
		}
	}
	export, hasGen, err := s.vectorSource.BeginExport(ctx, scope)
	if errors.Is(err, storage.ErrVectorSourceNotReady) {
		res.Skipped, res.SkippedReason = true, err.Error()
		log.Printf("vector push: skipped: %v", err)
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("resolving local vector generation: %w", err)
	}
	if !hasGen {
		res.Skipped, res.SkippedReason = true, "no active local generation"
		return res, nil
	}
	if export == nil {
		return res, errors.New("resolving local vector generation: BeginExport returned a nil export")
	}
	defer func() {
		if export != nil {
			_ = export.Close()
		}
	}()
	gen := export.Generation()
	if !baseSchemaReady {
		if skipped, err := s.prepareVectorBaseSchema(ctx, &res); err != nil || skipped {
			return res, err
		}
	}
	witnessKey, err := s.vectorGenerationWitnessKey(ctx)
	if err != nil {
		return res, err
	}
	// A scoped push writes only its changed sessions' chunks, which is safe
	// only within the exact generation instance this process last reconciled
	// generation-wide. Two signals identify that instance. The PG generation
	// id catches every recreation that keeps the tables: row deletion never
	// resets the sequence, so a re-embed (new fingerprint), reset, or admin
	// drop yields a new id, whoever recreates it. Recreating the tables
	// themselves restarts the sequence, so a reused id can match a stale
	// memo; this pusher's scoped witness record, wiped in the same reset and
	// written only after a clean generation-wide reconciliation, is the
	// witness for that case. Either signal failing means the prior
	// reconciliation no longer covers this generation and scoping would leave
	// search reading an incomplete one until the interval floor.
	// Promote to a generation-wide read. A zero memo means no reconciliation
	// to trust yet, so the reconcile bit already forces this push
	// generation-wide and the id check must not fire.
	requestedScope := scope
	var resolved vectorGeneration
	if scope != nil {
		initialProbe, found, err := s.lookupVectorGeneration(ctx, gen, witnessKey)
		if err != nil {
			return res, err
		}
		if !found {
			log.Printf("vector push: generation %q is not yet registered in PostgreSQL; promoting scoped push to generation-wide reconciliation",
				gen.Fingerprint)
			scope = nil
		} else if !initialProbe.machineRecorded {
			log.Printf("vector push: no prior scoped witness against generation %d; promoting scoped push to generation-wide reconciliation",
				initialProbe.id)
			scope = nil
		} else if lastReconciledGeneration != 0 &&
			initialProbe.id != lastReconciledGeneration {
			log.Printf("vector push: active generation id %d differs from the last reconciled %d; promoting scoped push to generation-wide reconciliation",
				initialProbe.id, lastReconciledGeneration)
			scope = nil
		} else {
			if s.afterVectorGenerationLookup != nil {
				hook := s.afterVectorGenerationLookup
				s.afterVectorGenerationLookup = nil
				hook()
			}
			currentProbe, found, err := s.lookupVectorGeneration(ctx, gen, witnessKey)
			if err != nil {
				return res, err
			}
			if !found {
				log.Printf("vector push: generation %q disappeared before scoped reconciliation; promoting scoped push to generation-wide reconciliation",
					gen.Fingerprint)
				scope = nil
			} else if !currentProbe.machineRecorded {
				log.Printf("vector push: no prior scoped witness against generation %d before scoped reconciliation; promoting scoped push to generation-wide reconciliation",
					currentProbe.id)
				scope = nil
			} else if currentProbe.id != initialProbe.id {
				log.Printf("vector push: active generation id changed from %d to %d before scoped reconciliation; promoting scoped push to generation-wide reconciliation",
					initialProbe.id, currentProbe.id)
				scope = nil
			} else {
				resolved = currentProbe
			}
		}
	}
	if requestedScope != nil && scope == nil {
		_ = export.Close()
		export, hasGen, err = s.vectorSource.BeginExport(ctx, nil)
		if errors.Is(err, storage.ErrVectorSourceNotReady) {
			res.Skipped, res.SkippedReason = true, err.Error()
			log.Printf("vector push: skipped after scoped promotion: %v", err)
			return res, nil
		}
		if err != nil {
			return res, fmt.Errorf(
				"rechecking local vector generation after scoped promotion: %w", err,
			)
		}
		if !hasGen {
			res.Skipped, res.SkippedReason = true, "no active local generation"
			return res, nil
		}
		if export == nil {
			return res, errors.New("rechecking local vector generation after scoped promotion: BeginExport returned a nil export")
		}
		gen = export.Generation()
	}
	if scope == nil {
		resolved, err = s.resolveVectorGeneration(ctx, gen, witnessKey)
		if err != nil {
			if s.skipVectorsOnPrivilegeError(err, &res) {
				return res, nil
			}
			return res, err
		}
	}
	res.GenerationID = resolved.id
	owner, err := s.vectorOwnerIdentity(ctx)
	if err != nil {
		return res, err
	}
	local, err := export.SessionDocHashes(ctx, scope)
	if err != nil {
		return res, fmt.Errorf("reading local vector doc hashes: %w", err)
	}
	var pgState map[string]vectorPushStateRow
	if scope != nil {
		pgState, err = s.readVectorPushStateForSessions(
			ctx, resolved.id, scope,
		)
	} else {
		pgState, err = s.readVectorPushState(ctx, resolved.id)
	}
	if err != nil {
		return res, err
	}
	retained, err := s.readOwnedVectorEvictionState(ctx, owner, scope)
	if err != nil {
		return res, err
	}
	for id, state := range retained {
		if _, present := local[id]; present {
			continue
		}
		if _, present := pgState[id]; !present {
			pgState[id] = state
		}
	}
	if scope != nil {
		log.Printf("vector push: change-scoped reconciliation of %d candidate session(s): %d local hash(es), %d PG state row(s) for generation %d",
			len(scope), len(local), len(pgState), resolved.id)
	} else {
		log.Printf("vector push: comparing %d local session(s) against %d PG state row(s) for generation %d",
			len(local), len(pgState), resolved.id)
	}
	// Session writes commit separately. Invalidate changed compatibility before
	// any chunks become visible, and certify it only after full reconciliation.
	if scope == nil {
		if err := s.markVectorGenerationMachinePending(ctx, gen, resolved, witnessKey); err != nil {
			return res, err
		}
	}
	if err := s.applyVectorDeltas(
		ctx, resolved, owner, full, local, pgState, export,
		failedSessions, onProgress, &res,
	); err != nil {
		return res, err
	}
	if s.afterVectorApply != nil {
		hook := s.afterVectorApply
		s.afterVectorApply = nil
		hook()
	}
	if scope != nil {
		if s.afterScopedVectorApply != nil {
			hook := s.afterScopedVectorApply
			s.afterScopedVectorApply = nil
			hook()
		}
		retryGenerationWide := func(msg string, args ...any) (storage.VectorPushResult, error) {
			log.Printf(msg, args...)
			_ = export.Close()
			export = nil
			return s.pushVectors(
				ctx, full, nil, lastReconciledGeneration, failedSessions, onProgress,
			)
		}
		finalProbe, found, err := s.lookupVectorGeneration(ctx, gen, witnessKey)
		if err != nil {
			return res, err
		}
		if !found {
			return retryGenerationWide(
				"vector push: generation %q disappeared after scoped reconciliation; retrying generation-wide",
				gen.Fingerprint,
			)
		}
		if !finalProbe.machineRecorded {
			return retryGenerationWide(
				"vector push: no prior scoped witness against generation %d after scoped reconciliation; retrying generation-wide",
				finalProbe.id,
			)
		}
		if finalProbe.id != resolved.id {
			return retryGenerationWide(
				"vector push: active generation id changed from %d to %d after scoped reconciliation; retrying generation-wide",
				resolved.id, finalProbe.id,
			)
		}
	} else {
		retryGenerationWide := func(msg string, args ...any) (storage.VectorPushResult, error) {
			log.Printf(msg, args...)
			_ = export.Close()
			export = nil
			return s.pushVectors(
				ctx, full, nil, lastReconciledGeneration, failedSessions, onProgress,
			)
		}
		finalProbe, found, err := s.lookupVectorGeneration(ctx, gen, witnessKey)
		if err != nil {
			return res, err
		}
		if !found {
			return retryGenerationWide(
				"vector push: generation %q disappeared before recording the generation-wide witness; retrying generation-wide",
				gen.Fingerprint,
			)
		}
		if finalProbe.id != resolved.id {
			return retryGenerationWide(
				"vector push: active generation id changed from %d to %d before recording the generation-wide witness; retrying generation-wide",
				resolved.id, finalProbe.id,
			)
		}
		if !finalProbe.createdAt.Equal(resolved.createdAt) {
			return retryGenerationWide(
				"vector push: generation %d was recreated before recording the generation-wide witness; retrying generation-wide",
				resolved.id,
			)
		}
	}
	if scope == nil && res.SessionsDeferred == 0 {
		if s.beforeVectorWitnessRecord != nil {
			hook := s.beforeVectorWitnessRecord
			s.beforeVectorWitnessRecord = nil
			hook()
		}
		recorded, err := s.recordVectorGenerationMachine(
			ctx, gen, resolved, witnessKey,
		)
		if err != nil {
			return res, err
		}
		if !recorded {
			log.Printf("vector push: generation %d changed after verification and before witness insert; retrying generation-wide",
				resolved.id)
			_ = export.Close()
			export = nil
			return s.pushVectors(
				ctx, full, nil, lastReconciledGeneration, failedSessions, onProgress,
			)
		}
	}
	log.Printf("vector push: %d session(s) pushed, %d unchanged, %d deferred, %d evicted, %d chunks",
		res.SessionsPushed, res.SessionsUnchanged, res.SessionsDeferred,
		res.SessionsEvicted, res.ChunksPushed)
	return res, nil
}

func (s *Sync) lookupVectorGeneration(
	ctx context.Context, info storage.VectorGenerationInfo, witnessKey string,
) (vectorGeneration, bool, error) {
	var genID int64
	var createdAt time.Time
	err := s.pg.QueryRowContext(ctx,
		`SELECT id, created_at FROM vector_generations WHERE fingerprint = $1`,
		info.Fingerprint,
	).Scan(&genID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return vectorGeneration{}, false, nil
	}
	if isUndefinedTable(err) {
		return vectorGeneration{}, false, nil
	}
	if err != nil {
		return vectorGeneration{}, false, fmt.Errorf("looking up vector generation: %w", err)
	}
	extSchema, err := vectorExtensionSchema(ctx, s.pg)
	if err != nil {
		return vectorGeneration{}, false, err
	}
	var machineRecorded bool
	if err := s.pg.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM vector_generation_machines m
     WHERE generation_id = $1 AND machine = $2
       AND (NOT (to_jsonb(m) ?& ARRAY['data_version','include_automated'])
            OR ((to_jsonb(m)->>'data_version')::integer IS NOT DISTINCT FROM $3
                AND (to_jsonb(m)->>'include_automated')::boolean IS NOT DISTINCT FROM $4)))`,
		genID, witnessKey, nullableVectorDataVersion(info.DataVersion), info.IncludeAutomated).Scan(&machineRecorded); err != nil {
		return vectorGeneration{}, false, fmt.Errorf(
			"reading vector push machine record: %w", err,
		)
	}
	compatibilityColumns, err := vectorCompatibilityColumns(ctx, s.pg)
	if err != nil {
		return vectorGeneration{}, false, err
	}
	return vectorGeneration{
		compatibilityColumns: compatibilityColumns,
		id:                   genID,
		createdAt:            createdAt,
		halfvecType:          extSchema + ".halfvec",
		machineRecorded:      machineRecorded,
	}, true, nil
}

// skipVectorsOnPrivilegeError reports whether err is an
// insufficient-privilege error (SQLSTATE 42501) from the vector SETUP phase
// — base tables, generation registration, chunk table — and marks res as
// skipped when it is: a restricted role that cannot create vector tables in
// an already-provisioned schema degrades exactly like a database without
// pgvector, instead of failing a push whose session phase succeeded.
// Privilege errors AFTER setup (mid-push writes) never reach this and still
// fail the push loudly.
// prepareVectorBaseSchema installs pgvector and the base vector tables. It
// reports true when vectors are unavailable on this server or to this role,
// with res marked skipped.
func (s *Sync) prepareVectorBaseSchema(
	ctx context.Context, res *storage.VectorPushResult,
) (bool, error) {
	unavailable, err := ensureVectorBaseSchemaPG(ctx, s.pg)
	if err != nil {
		if s.skipVectorsOnPrivilegeError(err, res) {
			return true, nil
		}
		return false, err
	}
	if unavailable != "" {
		res.Skipped, res.SkippedReason = true, unavailable
		return true, nil
	}
	existing, err := loadExistingColumns(ctx, s.pg, nil, "vector_generation_machines")
	if err != nil {
		return false, err
	}
	if _, err := ensureColumns(ctx, s.pg, existing, []columnMigration{
		{table: "vector_generation_machines", column: "data_version", def: "data_version INTEGER"},
		{table: "vector_generation_machines", column: "include_automated", def: "include_automated BOOLEAN"},
	}); err != nil {
		if isInsufficientPrivilege(err) {
			// Optional metadata must not revoke the legacy writer's capability.
			// Readers refuse hub compatibility while either column is absent.
			return false, nil
		}
		return false, fmt.Errorf("adding vector compatibility metadata: %w", err)
	}
	return false, nil
}

func (s *Sync) skipVectorsOnPrivilegeError(err error, res *storage.VectorPushResult) bool {
	if !isInsufficientPrivilege(err) {
		return false
	}
	res.Skipped = true
	res.SkippedReason = "insufficient PostgreSQL privileges for vector tables"
	log.Printf("vector push: skipped: %v", err)
	return true
}

// applyVectorDeltas pushes changed/new sessions and evicts sessions absent from
// local, accumulating counters into res. Sessions owned by another machine are
// left untouched on both paths and counted in res.Conflicts (ownership
// conflicts mirror the session push). When a project filter is active, sessions
// that fail the filter are excluded from both paths entirely: they are neither
// pushed, counted unchanged, evicted, nor counted as conflicts, mirroring the
// session push's project scope so a filtered `pg push` never touches vector
// state for out-of-scope sessions. Local candidates are scoped by their live
// local project and PG-only eviction candidates by their PG project (see
// vectorSessionsOutOfScope). Changed sessions in failedSessions are deferred
// rather than pushed: their session-phase push failed, so sending newer
// vectors would leave pgvector data ahead of the sessions/messages rows.
// Their delta state is untouched and the next successful push sends them.
// Failed sessions are excluded from eviction the same way: the local hash map
// only lists sessions with embedded docs, so a failed session whose docs all
// vanished locally would otherwise be evicted — vector state running ahead of
// the sessions/messages rows its session-phase push failed to write. full,
// or a session's entry in s.fullVectorSessions, re-sends unchanged sessions
// too, except those the running full push already pushed before an
// interruption, while still honoring out-of-scope exclusion,
// ownership conflicts, and failed-session deferral. onProgress, when non-nil,
// receives a Phase "vectors" report after every pushed session and every
// vectorProgressStride examined ones, so long delta scans stay visible
// without a callback per unchanged session.
func (s *Sync) applyVectorDeltas(
	ctx context.Context, gen vectorGeneration,
	owner vectorOwnerIdentity, full bool,
	local map[string]string, pgState map[string]vectorPushStateRow,
	export storage.VectorExport, failedSessions map[string]struct{}, onProgress func(storage.PushProgress),
	res *storage.VectorPushResult,
) error {
	allGenIDs, err := s.allVectorGenerationIDs(ctx)
	if err != nil {
		return err
	}
	genIDs, err := s.existingChunkGenerations(ctx, allGenIDs)
	if err != nil {
		return err
	}
	scope := vectorPushScope{
		gen:    gen,
		genIDs: genIDs,
		owner:  owner,
	}

	outOfScope, err := s.vectorSessionsOutOfScope(ctx, local, pgState)
	if err != nil {
		return err
	}

	examined := 0
	report := func() {
		if onProgress == nil {
			return
		}
		onProgress(storage.PushProgress{
			Phase:               "vectors",
			VectorSessionsDone:  examined,
			VectorSessionsTotal: len(local),
			VectorChunksPushed:  res.ChunksPushed,
		})
	}
	for sessionID, agg := range local {
		examined++
		if _, skip := outOfScope[sessionID]; skip {
			continue
		}
		prev, hasState := pgState[sessionID]
		if hasState && !owner.owns(prev.ownerMarker, prev.machine) {
			res.Conflicts++
			continue
		}
		_, repair := s.fullVectorSessions[sessionID]
		if !repair && hasState && prev.docAggHash == agg &&
			(!full || s.fullRun.vectorDone(gen.id, sessionID, agg)) {
			res.SessionsUnchanged++
			continue
		}
		if _, failed := failedSessions[sessionID]; failed {
			res.DeferSession(sessionID)
			continue
		}
		outcome, err := s.pushVectorSession(ctx, scope, export, sessionID, agg)
		if err != nil {
			return err
		}
		if outcome.conflict {
			res.Conflicts++
		}
		if outcome.deferred {
			res.DeferSession(sessionID)
		}
		if outcome.pushed {
			res.SessionsPushed++
			res.DocsPushed += outcome.docs
			res.ChunksPushed += outcome.chunks
			res.DocsDeleted += outcome.deleted
			if err := s.fullRun.recordVector(ctx, gen.id, sessionID, agg); err != nil {
				return err
			}
			report()
		} else if examined%vectorProgressStride == 0 {
			report()
		}
	}
	report()

	var evict, foreign []string
	for sessionID, st := range pgState {
		if _, inLocal := local[sessionID]; inLocal {
			continue
		}
		if _, skip := outOfScope[sessionID]; skip {
			continue
		}
		if _, failed := failedSessions[sessionID]; failed {
			res.DeferSession(sessionID)
			continue
		}
		switch {
		case !st.sessionExists || owner.owns(st.ownerMarker, st.machine):
			evict = append(evict, sessionID)
		default:
			foreign = append(foreign, sessionID)
		}
	}
	conflicts, err := s.countLocalForeignVectorSessions(ctx, foreign)
	if err != nil {
		return err
	}
	res.Conflicts += conflicts
	return s.evictVectorSessions(ctx, scope, evict, res)
}

// countLocalForeignVectorSessions counts the foreign-owned eviction candidates
// that still have a live local sessions row. pgState spans every machine
// pushing into the generation, so most foreign-owned rows are simply another
// machine's sessions that this archive never held: those are neither evicted
// nor conflicts. Only a session this archive also holds, whose vectors this
// pusher would otherwise evict, is a genuine ownership conflict.
func (s *Sync) countLocalForeignVectorSessions(
	ctx context.Context, sessionIDs []string,
) (int, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	present, err := s.local.SessionProjectsByIDs(ctx, sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("reading local foreign vector sessions: %w", err)
	}
	return len(present), nil
}

// vectorSessionsOutOfScope computes the set of candidate sessions a filtered
// push must not touch, partitioned by which project value is authoritative.
// Any candidate with a live local sessions row — present in the vectors.db
// hash map or not, since a live session can have zero embedded docs — is
// scoped by its LIVE local project: a session that moved out of the filter
// locally (alpha->beta) keeps its old PG project because the filtered session
// push skipped it, so scoping those by PG would let a stale value re-push or
// evict their vectors. Only candidates genuinely absent from the local
// sessions table keep the PG-project scope; theirs is then the only project
// there is, and it is correct for eviction. Returns nil (empty set) when no
// filter is active, so the common unfiltered path issues no query.
func (s *Sync) vectorSessionsOutOfScope(
	ctx context.Context,
	local map[string]string, pgState map[string]vectorPushStateRow,
) (map[string]struct{}, error) {
	if !s.isFiltered() {
		return nil, nil
	}
	localIDs := make([]string, 0, len(local))
	for id := range local {
		localIDs = append(localIDs, id)
	}
	out, err := s.localOutOfScopeVectorSessions(ctx, localIDs)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = make(map[string]struct{})
	}
	var pgOnly []string
	for id := range pgState {
		if _, inLocal := local[id]; !inLocal {
			pgOnly = append(pgOnly, id)
		}
	}
	absent, err := s.scopeEvictionCandidatesByLocalProject(ctx, pgOnly, out)
	if err != nil {
		return nil, err
	}
	pgOut, err := s.outOfScopeVectorSessions(ctx, absent)
	if err != nil {
		return nil, err
	}
	for id := range pgOut {
		out[id] = struct{}{}
	}
	return out, nil
}

// scopeEvictionCandidatesByLocalProject scopes eviction candidates that still
// have a live local sessions row by that row's project — adding filter
// failures to out — and returns the candidates with no local row at all, whose
// scoping must fall back to the PG project. Candidates reach here because they
// are absent from the vectors.db hash map, which only proves they have no
// embedded docs, not that the session left the local archive. Caller
// guarantees a filter is active.
func (s *Sync) scopeEvictionCandidatesByLocalProject(
	ctx context.Context, candidateIDs []string, out map[string]struct{},
) ([]string, error) {
	if len(candidateIDs) == 0 {
		return nil, nil
	}
	projects, err := s.local.SessionProjectsByIDs(ctx, candidateIDs)
	if err != nil {
		return nil, fmt.Errorf("reading local session projects: %w", err)
	}
	var absent []string
	for _, id := range candidateIDs {
		project, ok := projects[id]
		if !ok {
			absent = append(absent, id)
			continue
		}
		if projectFailsFilter(project, s.projects, s.excludeProjects) {
			out[id] = struct{}{}
		}
	}
	return absent, nil
}

// localOutOfScopeVectorSessions returns the subset of localIDs whose LIVE local
// sessions.project fails this push's filter, read from the local DB rather than
// PG so a session that changed projects locally is scoped by its current value.
// A session absent from the local sessions table is left in scope: the
// per-session in-tx ownership probe skips it when no PG row exists, matching the
// unfiltered path rather than silently swallowing it here. Caller guarantees a
// filter is active.
func (s *Sync) localOutOfScopeVectorSessions(
	ctx context.Context, localIDs []string,
) (map[string]struct{}, error) {
	if len(localIDs) == 0 {
		return nil, nil
	}
	projects, err := s.local.SessionProjectsByIDs(ctx, localIDs)
	if err != nil {
		return nil, fmt.Errorf("reading local session projects: %w", err)
	}
	out := make(map[string]struct{})
	for _, id := range localIDs {
		project, ok := projects[id]
		if !ok {
			continue
		}
		if projectFailsFilter(project, s.projects, s.excludeProjects) {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// projectFailsFilter reports whether project is excluded by the push's
// include/exclude filter, mirroring the session push's SQL predicate: an
// include filter excludes any project not in the allowed set; an exclude filter
// excludes any project in the excluded set. Caller guarantees exactly one of
// projects/excludeProjects is set (storage.ValidateProjectFilters rejects both).
func projectFailsFilter(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 {
		return !slices.Contains(projects, project)
	}
	return slices.Contains(excludeProjects, project)
}

// outOfScopeVectorSessions returns the subset of PG-only eviction candidateIDs
// whose PG sessions.project fails this push's project filter. These are state
// rows whose session no longer exists locally, so there is no live local
// project to scope them by; the PG project is authoritative for the eviction
// decision. Sessions with no PG row are never returned: the evict path already
// treats a vanished session as evictable. Returns nil (empty set) when no
// filter is active, so the common unfiltered path issues no query.
func (s *Sync) outOfScopeVectorSessions(
	ctx context.Context, candidateIDs []string,
) (map[string]struct{}, error) {
	if !s.isFiltered() || len(candidateIDs) == 0 {
		return nil, nil
	}
	query, args := vectorOutOfScopeQuery(
		candidateIDs, s.projects, s.excludeProjects,
	)
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading out-of-scope vector sessions: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning out-of-scope vector session: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating out-of-scope vector sessions: %w", err)
	}
	return out, nil
}

// vectorOutOfScopeQuery builds the SQL that selects candidate session IDs whose
// PG project fails the filter. It mirrors the session push's include/exclude
// semantics: an include filter selects sessions whose project is not in the
// allowed set; an exclude filter selects sessions whose project is in the
// excluded set. Caller guarantees exactly one of projects/excludeProjects is
// set (storage.ValidateProjectFilters rejects both).
func vectorOutOfScopeQuery(
	ids, projects, excludeProjects []string,
) (string, []any) {
	if len(projects) > 0 {
		return `SELECT id FROM sessions
		         WHERE id = ANY($1) AND NOT (project = ANY($2))`,
			[]any{ids, projects}
	}
	return `SELECT id FROM sessions
	         WHERE id = ANY($1) AND project = ANY($2)`,
		[]any{ids, excludeProjects}
}

// publishGenerationRecipe records gen's recipe for adoption by other hosts.
// It runs only on generation-wide pushes. A writer that may not add the
// params column or update the row keeps pushing vectors without publishing.
func (s *Sync) publishGenerationRecipe(
	ctx context.Context, gen storage.VectorGenerationInfo,
) error {
	if gen.Params == nil {
		return nil
	}
	err := ensureVectorRecipeColumn(ctx, s.pg)
	if err == nil {
		err = publishVectorRecipe(ctx, s.pg, gen.Fingerprint, gen.Params)
	}
	if !isInsufficientPrivilege(err) {
		return err
	}
	if !s.recipePrivilegeLogged {
		s.recipePrivilegeLogged = true
		log.Printf("vector push: cannot publish the embedding recipe "+
			"(insufficient privilege); pushing vectors without it: %v", err)
	}
	return nil
}

// registerVectorGeneration registers gen, publishes its recipe, and creates
// its chunk table, returning the generation id.
func (s *Sync) registerVectorGeneration(
	ctx context.Context, gen storage.VectorGenerationInfo,
) (int64, error) {
	genID, err := ensureVectorGeneration(
		ctx, s.pg, gen.Fingerprint, gen.Model, gen.Dimension,
	)
	if err != nil {
		return 0, err
	}
	if err := s.publishGenerationRecipe(ctx, gen); err != nil {
		return 0, err
	}
	if err := ensureVectorChunkTable(ctx, s.pg, genID, gen.Dimension); err != nil {
		return 0, err
	}
	return genID, nil
}

// resolveVectorGeneration registers the generation, creates its chunk table,
// and reads whether this machine has already completed a generation-wide push
// against it. Its id is the generation
// instance's identity, which a scoped push compares against the last one it
// reconciled generation-wide to decide whether to promote; whether this
// scoped witness predated this call is captured first, because that record is
// the incarnation witness the promotion check falls back on when a recreated
// id sequence hands the new generation the memoized id.
func (s *Sync) resolveVectorGeneration(
	ctx context.Context, gen storage.VectorGenerationInfo, witnessKey string,
) (vectorGeneration, error) {
	genID, err := s.registerVectorGeneration(ctx, gen)
	if err != nil {
		return vectorGeneration{}, err
	}
	extSchema, err := vectorExtensionSchema(ctx, s.pg)
	if err != nil {
		return vectorGeneration{}, err
	}
	var createdAt time.Time
	if err := s.pg.QueryRowContext(ctx,
		`SELECT created_at FROM vector_generations WHERE id = $1`, genID,
	).Scan(&createdAt); err != nil {
		return vectorGeneration{}, fmt.Errorf("reading vector generation created_at: %w", err)
	}
	var machineRecorded bool
	if err := s.pg.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM vector_generation_machines
     WHERE generation_id = $1 AND machine = $2)`,
		genID, witnessKey).Scan(&machineRecorded); err != nil {
		return vectorGeneration{}, fmt.Errorf("reading vector push machine record: %w", err)
	}
	compatibilityColumns, err := vectorCompatibilityColumns(ctx, s.pg)
	if err != nil {
		return vectorGeneration{}, err
	}
	return vectorGeneration{
		compatibilityColumns: compatibilityColumns,
		id:                   genID,
		createdAt:            createdAt,
		halfvecType:          extSchema + ".halfvec",
		machineRecorded:      machineRecorded,
	}, nil
}

func (s *Sync) vectorGenerationWitnessKey(ctx context.Context) (string, error) {
	markerID, err := s.pushMarkerID(ctx)
	if err != nil {
		return "", err
	}
	return s.machine + "|" + s.pushMarkerMetadataKey(pushMarkerKeyPrefix, markerID), nil
}

func vectorCompatibilityColumns(ctx context.Context, pg *sql.DB) (bool, error) {
	columns, err := loadExistingColumns(ctx, pg, nil, "vector_generation_machines")
	if err != nil {
		return false, err
	}
	_, version := columns["vector_generation_machines"]["data_version"]
	_, automated := columns["vector_generation_machines"]["include_automated"]
	return version && automated, nil
}

func (s *Sync) markVectorGenerationMachinePending(
	ctx context.Context, info storage.VectorGenerationInfo, gen vectorGeneration, witnessKey string,
) error {
	if !gen.compatibilityColumns {
		return nil
	}
	_, err := s.pg.ExecContext(ctx, `
INSERT INTO vector_generation_machines (generation_id, machine, last_push_at, data_version, include_automated)
SELECT id, $4, now(), NULL, NULL
  FROM vector_generations
 WHERE id = $1 AND fingerprint = $2 AND created_at = $3
ON CONFLICT (generation_id, machine) DO UPDATE SET data_version = NULL, include_automated = NULL
 WHERE vector_generation_machines.data_version IS DISTINCT FROM $5
    OR vector_generation_machines.include_automated IS DISTINCT FROM $6`,
		gen.id, info.Fingerprint, gen.createdAt, witnessKey, nullableVectorDataVersion(info.DataVersion), info.IncludeAutomated)
	if err != nil {
		return fmt.Errorf("marking vector push compatibility pending: %w", err)
	}
	return nil
}

// Unknown source provenance remains NULL so older mirrors keep publishing
// without becoming eligible for hub discovery.
func nullableVectorDataVersion(version int) any {
	if version <= 0 {
		return nil
	}
	return version
}

func (s *Sync) recordVectorGenerationMachine(
	ctx context.Context, info storage.VectorGenerationInfo, gen vectorGeneration, witnessKey string,
) (bool, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("starting vector push witness transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `
INSERT INTO vector_generation_machines (generation_id, machine, last_push_at, data_version, include_automated)
SELECT id, $4, now(), $5, $6
  FROM vector_generations
 WHERE id = $1 AND fingerprint = $2 AND created_at = $3
ON CONFLICT (generation_id, machine) DO UPDATE SET last_push_at = EXCLUDED.last_push_at,
 data_version = EXCLUDED.data_version, include_automated = EXCLUDED.include_automated`
	args := []any{gen.id, info.Fingerprint, gen.createdAt, witnessKey, nullableVectorDataVersion(info.DataVersion), info.IncludeAutomated}
	if !gen.compatibilityColumns {
		query = `INSERT INTO vector_generation_machines (generation_id, machine, last_push_at)
SELECT id, $4, now() FROM vector_generations
 WHERE id=$1 AND fingerprint=$2 AND created_at=$3
ON CONFLICT (generation_id,machine) DO UPDATE SET last_push_at=EXCLUDED.last_push_at`
		args = args[:4]
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("recording vector push machine: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("confirming vector push machine record: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	// The marker and filter scope identify this publisher across machine renames.
	// Publish its new compatibility and retire aliases in one transaction.
	suffix := witnessKey[strings.LastIndex(witnessKey, "|"+pushMarkerKeyPrefix):]
	if _, err := tx.ExecContext(ctx, `
DELETE FROM vector_generation_machines
 WHERE generation_id = $1 AND machine <> $2
   AND right(machine, length($3)) = $3`, gen.id, witnessKey, suffix); err != nil {
		return false, fmt.Errorf("retiring renamed vector push witnesses: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing vector push witness: %w", err)
	}
	return true, nil
}

// readOwnedVectorEvictionState finds retained coverage even before this
// publisher's first push of a replacement generation. Ownership and the
// effective change scope limit discovery; project/failure guards and the
// locked per-session eviction recheck still adjudicate every candidate.
func (s *Sync) readOwnedVectorEvictionState(ctx context.Context, owner vectorOwnerIdentity, sessionIDs []string) (map[string]vectorPushStateRow, error) {
	machines := append([]string{"", "local", owner.machine}, owner.legacyMachines...)
	rows, err := s.pg.QueryContext(ctx, `
SELECT DISTINCT ps.session_id, '', s.owner_marker, s.machine, true
 FROM vector_push_state ps JOIN sessions s ON s.id=ps.session_id
 WHERE (COALESCE(s.owner_marker,'')=$1 OR
   (COALESCE(s.owner_marker,'')='' AND COALESCE(s.machine,'')=ANY($2)))
 AND ($3::text[] IS NULL OR ps.session_id=ANY($3))`, owner.markerID, machines, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("reading retained owned vector state: %w", err)
	}
	defer rows.Close()
	return scanVectorPushState(rows)
}

// readVectorPushState loads the delta state for genID, joined with each
// session's current owner marker and machine (the machine adjudicates legacy
// rows whose owner_marker is empty; see vectorOwnerIdentity). A LEFT JOIN miss
// (sessionExists false) marks a state row whose session row is gone; such rows
// are always stale.
func (s *Sync) readVectorPushState(
	ctx context.Context, genID int64,
) (map[string]vectorPushStateRow, error) {
	rows, err := s.pg.QueryContext(ctx, `
SELECT ps.session_id, ps.doc_agg_hash, s.owner_marker, s.machine,
       (s.id IS NOT NULL)
  FROM vector_push_state ps
  LEFT JOIN sessions s ON s.id = ps.session_id
 WHERE ps.generation_id = $1`, genID)
	if err != nil {
		return nil, fmt.Errorf("reading vector push state: %w", err)
	}
	defer rows.Close()
	return scanVectorPushState(rows)
}

// readVectorPushStateForSessions is readVectorPushState limited to the given
// session IDs, so a change-scoped push reads state proportional to its
// changed set instead of the whole generation.
func (s *Sync) readVectorPushStateForSessions(
	ctx context.Context, genID int64, sessionIDs []string,
) (map[string]vectorPushStateRow, error) {
	rows, err := s.pg.QueryContext(ctx, `
SELECT ps.session_id, ps.doc_agg_hash, s.owner_marker, s.machine,
       (s.id IS NOT NULL)
  FROM vector_push_state ps
  LEFT JOIN sessions s ON s.id = ps.session_id
 WHERE ps.generation_id = $1
   AND ps.session_id = ANY($2)`, genID, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("reading scoped vector push state: %w", err)
	}
	defer rows.Close()
	return scanVectorPushState(rows)
}

func scanVectorPushState(
	rows *sql.Rows,
) (map[string]vectorPushStateRow, error) {
	state := make(map[string]vectorPushStateRow)
	for rows.Next() {
		var sessionID, aggHash string
		var ownerMarker, machine sql.NullString
		var exists bool
		if err := rows.Scan(
			&sessionID, &aggHash, &ownerMarker, &machine, &exists,
		); err != nil {
			return nil, fmt.Errorf("scanning vector push state: %w", err)
		}
		state[sessionID] = vectorPushStateRow{
			docAggHash:    aggHash,
			ownerMarker:   ownerMarker.String,
			machine:       machine.String,
			sessionExists: exists,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vector push state: %w", err)
	}
	return state, nil
}

// vectorSessionOutcome reports what one pushVectorSession call did. pushed is
// false when the session was skipped: conflict marks a skip because the
// sessions row is owned by another machine (counted in res.Conflicts);
// deferred marks a skip because the session's export hash no longer matched
// its delta-scan hash — the local index changed mid-push, so nothing was
// written and the next push re-derives the delta; a skip with neither flag
// means the sessions row is gone (the session phase did not push it).
// docs/chunks/deleted are zero on any skip.
type vectorSessionOutcome struct {
	pushed   bool
	conflict bool
	deferred bool
	docs     int
	chunks   int
	deleted  int
}

// pushVectorSession replaces one session's docs, chunks, and push state for the
// scoped generation in a single transaction. It re-verifies ownership inside
// the transaction: a missing sessions row means the session phase did not push
// it (skip), and a different owner marker means another machine owns it
// (conflict skip, mirroring the session push's conflict).
//
// doc_key is source_uuid-based and stable across ordinal-shifting rewrites, but
// vector_documents has a UNIQUE (session_id, ordinal): upserting by doc_key
// alone collides with a not-yet-updated sibling when ordinals shift. So the tx
// first parks every existing session row at a unique negative ordinal, then
// upserts the current docs onto their final ordinals (freed by the park), then
// deletes rows still parked — docs that vanished locally — together with every
// generation's chunks for them (see deleteParkedVectorDocs). This mirrors the
// local mirror's park-to-sentinel slot replacement (internal/vector/mirror.go).
func (s *Sync) pushVectorSession(
	ctx context.Context, scope vectorPushScope, export storage.VectorExport, sessionID, aggHash string,
) (vectorSessionOutcome, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return vectorSessionOutcome{}, fmt.Errorf("begin vector push tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE locks the sessions row for the life of this tx so a concurrent
	// pusher cannot take ownership between this probe and the vector writes
	// below (under READ COMMITTED an unlocked probe could read a stale owner).
	// This is deadlock-safe: the session push phase has already committed before
	// pushVectors runs (see pushVectors), so no session-push tx holds this row
	// concurrently, and each vector push tx locks exactly one session row (by
	// primary key) before its own vector-table writes — a single, consistent
	// lock so two vector pushes cannot form a cycle.
	var ownerMarker, machine sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT owner_marker, machine FROM sessions WHERE id = $1 FOR UPDATE`,
		sessionID,
	).Scan(&ownerMarker, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return vectorSessionOutcome{}, nil
	}
	if err != nil {
		return vectorSessionOutcome{}, fmt.Errorf(
			"checking vector session ownership %s: %w", sessionID, err)
	}
	if !scope.owner.owns(ownerMarker.String, machine.String) {
		return vectorSessionOutcome{conflict: true}, nil
	}

	// Export the full docs (content + decoded chunk blobs) only after the
	// session passes the existence/ownership probe. A local-only session with no
	// PG row, or one owned elsewhere, skips forever; fetching docs first would
	// re-export them on every push for sessions that never land.
	docs, exportHash, err := export.SessionDocs(ctx, sessionID)
	if err != nil {
		return vectorSessionOutcome{}, fmt.Errorf(
			"reading local docs for session %s: %w", sessionID, err)
	}
	// The export hash covers exactly the docs read above, in the same local
	// snapshot. A mismatch with the delta-scan hash means the local index
	// changed between the scan and this export — typically an embeddings
	// rebuild clearing and refilling the generation in place — and the docs
	// may be a partial view. Writing them would replace valid PG vectors and
	// record aggHash as current, which a same-fingerprint rebuild (same doc
	// content, same hash) would never repair. Defer instead: nothing is
	// written, and the next push re-derives the delta from the settled index.
	if exportHash != aggHash {
		return vectorSessionOutcome{deferred: true}, nil
	}

	compatible, err := vectorSessionContentMatches(ctx, tx, sessionID, docs)
	if err != nil {
		return vectorSessionOutcome{}, err
	}
	// Remove invalidated chunks before their coverage records, so later scope
	// eviction can prune shared documents without orphaned searchable vectors.
	// Re-read generations under the session lock; another pusher may have
	// published one after this push collected its outer scope.
	generations, err := existingChunkGenerationsTx(ctx, tx)
	if err != nil {
		return vectorSessionOutcome{}, err
	}
	if !compatible {
		for _, id := range generations {
			if id == scope.gen.id {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+vectorChunkTable(id)+` WHERE doc_key IN (
 SELECT doc_key FROM vector_documents WHERE session_id=$1)`, sessionID); err != nil {
				return vectorSessionOutcome{}, fmt.Errorf("removing invalidated vector chunks: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vector_push_state WHERE session_id=$1 AND generation_id<>$2`, sessionID, scope.gen.id); err != nil {
			return vectorSessionOutcome{}, fmt.Errorf("invalidating stale vector coverage: %w", err)
		}
	} else {
		// Metadata still advances the export hash, including coverage retained
		// for pinned generations whose embedding inputs have not changed.
		if _, err := tx.ExecContext(ctx, `UPDATE vector_push_state SET doc_agg_hash=$2 WHERE session_id=$1`, sessionID, aggHash); err != nil {
			return vectorSessionOutcome{}, fmt.Errorf("refreshing vector coverage metadata: %w", err)
		}
	}
	if err := parkSessionVectorDocs(ctx, tx, sessionID); err != nil {
		return vectorSessionOutcome{}, err
	}
	if err := upsertVectorDocs(ctx, tx, docs); err != nil {
		return vectorSessionOutcome{}, err
	}
	chunks, err := replaceVectorChunks(ctx, tx, scope.gen, sessionID, docs)
	if err != nil {
		return vectorSessionOutcome{}, err
	}
	deleted, err := deleteParkedVectorDocs(ctx, tx, sessionID, scope.genIDs)
	if err != nil {
		return vectorSessionOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, $2, $3)
ON CONFLICT (generation_id, session_id)
DO UPDATE SET doc_agg_hash = EXCLUDED.doc_agg_hash`,
		scope.gen.id, sessionID, aggHash); err != nil {
		return vectorSessionOutcome{}, fmt.Errorf(
			"upserting vector push state %s: %w", sessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return vectorSessionOutcome{}, fmt.Errorf("commit vector push tx: %w", err)
	}
	return vectorSessionOutcome{
		pushed: true, docs: len(docs), chunks: chunks, deleted: deleted,
	}, nil
}

// vectorSessionContentMatches ignores display metadata while requiring the
// complete live document key/content set to match before retaining coverage.
func vectorSessionContentMatches(ctx context.Context, tx *sql.Tx, sessionID string, docs []storage.VectorPushDoc) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT doc_key, content_hash FROM vector_documents WHERE session_id=$1 AND ordinal>=0`, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading vector content compatibility: %w", err)
	}
	defer rows.Close()
	stored := make(map[string]string, len(docs))
	for rows.Next() {
		var key, hash string
		if err := rows.Scan(&key, &hash); err != nil {
			return false, err
		}
		stored[key] = hash
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(stored) != len(docs) {
		return false, nil
	}
	for _, doc := range docs {
		if hash, ok := stored[doc.DocKey]; !ok || hash != doc.ContentHash {
			return false, nil
		}
	}
	return true, nil
}

// parkSessionVectorDocs moves every non-negative-ordinal vector_documents row
// for sessionID to a unique negative ordinal below the session's existing
// parked floor, freeing the (session_id, ordinal) unique index so a subsequent
// doc_key upsert of shifted/renumbered docs cannot collide with a sibling not
// yet updated. Rows already parked (negative) are left as-is; seeding below
// MIN(ordinal) keeps every parked ordinal distinct across overlapping pushes,
// mirroring the local mirror's parkingFloor (internal/vector/mirror.go). The
// transform is injective, so the single statement never violates the index at
// any intermediate row.
func parkSessionVectorDocs(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx, `
WITH floor AS (
    SELECT COALESCE(MIN(ordinal), 0) AS f
      FROM vector_documents WHERE session_id = $1 AND ordinal < 0
), parked AS (
    SELECT doc_key, row_number() OVER (ORDER BY ordinal) AS rn
      FROM vector_documents WHERE session_id = $1 AND ordinal >= 0
)
UPDATE vector_documents d
   SET ordinal = (SELECT f FROM floor) - parked.rn
  FROM parked
 WHERE d.doc_key = parked.doc_key`, sessionID); err != nil {
		return fmt.Errorf("parking vector docs for session %s: %w", sessionID, err)
	}
	return nil
}

// upsertVectorDocs upserts each doc by its globally unique doc_key onto its
// final ordinal. The caller parks the session's prior rows to negative ordinals
// first, so every final (session_id, ordinal) slot is free and an ordinal shift
// cannot collide with a sibling row that has not been updated yet.
func upsertVectorDocs(ctx context.Context, tx *sql.Tx, docs []storage.VectorPushDoc) error {
	for _, doc := range docs {
		offsets := doc.OffsetsJSON
		if offsets == "" {
			offsets = "[]"
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, source_uuid, ordinal, ordinal_end,
    subordinate, offsets, content, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (doc_key) DO UPDATE SET
    session_id = EXCLUDED.session_id,
    source_uuid = EXCLUDED.source_uuid,
    ordinal = EXCLUDED.ordinal,
    ordinal_end = EXCLUDED.ordinal_end,
    subordinate = EXCLUDED.subordinate,
    offsets = EXCLUDED.offsets,
    content = EXCLUDED.content,
    content_hash = EXCLUDED.content_hash`,
			doc.DocKey, doc.SessionID, sanitizePG(doc.SourceUUID),
			doc.Ordinal, doc.OrdinalEnd, doc.Subordinate,
			sanitizePG(offsets), sanitizePG(doc.Content),
			doc.ContentHash); err != nil {
			return fmt.Errorf("upserting vector doc %s: %w", doc.DocKey, err)
		}
	}
	return nil
}

// replaceVectorChunks deletes all of the session's chunk rows for genID and
// re-inserts the current chunk set. Sessions are small, so wholesale replace is
// simpler and correct versus per-doc surgical deletes. It returns the number of
// chunk rows inserted.
func replaceVectorChunks(
	ctx context.Context, tx *sql.Tx, gen vectorGeneration,
	sessionID string, docs []storage.VectorPushDoc,
) (int, error) {
	table := vectorChunkTable(gen.id)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE doc_key IN (
    SELECT doc_key FROM vector_documents WHERE session_id = $1)`, table),
		sessionID); err != nil {
		return 0, fmt.Errorf("clearing chunks for session %s: %w", sessionID, err)
	}

	type chunkRow struct {
		docKey     string
		chunkIndex int
		embedding  string
	}
	var rows []chunkRow
	for _, doc := range docs {
		for _, chunk := range doc.Chunks {
			literal, err := halfvecLiteral(chunk.Embedding)
			if err != nil {
				return 0, fmt.Errorf(
					"doc %s chunk %d: %w", doc.DocKey, chunk.ChunkIndex, err)
			}
			rows = append(rows, chunkRow{
				docKey:     doc.DocKey,
				chunkIndex: chunk.ChunkIndex,
				embedding:  literal,
			})
		}
	}

	for start := 0; start < len(rows); start += vectorChunkInsertBatch {
		end := min(start+vectorChunkInsertBatch, len(rows))
		batch := rows[start:end]
		var values strings.Builder
		args := make([]any, 0, len(batch)*3)
		for i, r := range batch {
			if i > 0 {
				values.WriteByte(',')
			}
			base := i * 3
			fmt.Fprintf(&values, "($%d,$%d,$%d::%s)",
				base+1, base+2, base+3, gen.halfvecType)
			args = append(args, r.docKey, r.chunkIndex, r.embedding)
		}
		stmt := fmt.Sprintf(
			`INSERT INTO %s (doc_key, chunk_index, embedding) VALUES %s`,
			table, values.String())
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return 0, fmt.Errorf("inserting chunks for session %s: %w", sessionID, err)
		}
	}
	return len(rows), nil
}

var errEmbeddableUnitFound = errors.New("embeddable unit found")

// hasEmbeddableUnitsForSession uses the archive's eligibility rules, stopping
// at the first unit so eviction does not materialize a transcript under the
// PostgreSQL ownership lock. Only our callback's sentinel ends a scan cleanly.
func hasEmbeddableUnitsForSession(
	ctx context.Context, sessionID string,
	scan func(context.Context, string, bool, int, func(db.EmbeddableUnit) error) error,
) (bool, error) {
	err := scan(ctx, sessionID, true, 0, func(db.EmbeddableUnit) error {
		return errEmbeddableUnitFound
	})
	if errors.Is(err, errEmbeddableUnitFound) {
		return true, nil
	}
	return false, err
}

// evictVectorSessions removes each session's chunks and push state for the
// scoped generation and prunes its now-orphaned shared doc rows. If the archive
// has no embeddable units even with automated sessions included, all generations
// are invalidated. Scope exclusions retain other generations. There is one
// transaction per session. Ownership was adjudicated from the delta scan's
// state read, which can be minutes stale by the time evictions run, so each
// transaction re-probes the sessions row FOR UPDATE (the same lock discipline
// as pushVectorSession): a session another pusher claimed since the scan is
// skipped as a conflict rather than having that owner's fresh chunks deleted.
// A still-missing sessions row keeps the eviction — the state row is stale by
// definition — and a row recreated after this tx commits is re-pushed by its
// new owner's next vector push.
func (s *Sync) evictVectorSessions(
	ctx context.Context, scope vectorPushScope, sessionIDs []string,
	res *storage.VectorPushResult,
) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	for _, sessionID := range sessionIDs {
		tx, err := s.pg.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin vector evict tx: %w", err)
		}
		var ownerMarker, machine sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT owner_marker, machine FROM sessions WHERE id = $1 FOR UPDATE`,
			sessionID,
		).Scan(&ownerMarker, &machine)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No sessions row: the push state is stale, evict below.
		case err != nil:
			_ = tx.Rollback()
			return fmt.Errorf(
				"checking vector evict ownership %s: %w", sessionID, err)
		case !scope.owner.owns(ownerMarker.String, machine.String):
			_ = tx.Rollback()
			res.Conflicts++
			continue
		}
		// Absence from the export can also mean an automated-scope exclusion.
		// Consult the archive with that filter disabled before invalidating
		// other generations, under the same ownership reservation as deletion.
		hasUnits, err := hasEmbeddableUnitsForSession(ctx, sessionID, s.local.ScanEmbeddableUnitsForSession)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("checking evicted session content %s: %w", sessionID, err)
		}
		generations, err := existingChunkGenerationsTx(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, id := range generations {
			if hasUnits && id != scope.gen.id {
				continue
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE doc_key IN (
    SELECT doc_key FROM vector_documents WHERE session_id = $1)`, vectorChunkTable(id)),
				sessionID); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("evicting chunks for session %s: %w", sessionID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM vector_push_state
 WHERE session_id = $2 AND (generation_id = $1 OR $3)`,
			scope.gen.id, sessionID, !hasUnits); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("evicting push state for session %s: %w", sessionID, err)
		}
		deleted, err := deleteOrphanVectorDocs(ctx, tx, sessionID, generations)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit vector evict tx: %w", err)
		}
		res.SessionsEvicted++
		res.DocsDeleted += deleted
	}
	return nil
}

// existingChunkGenerations returns the subset of genIDs whose per-generation
// chunk table currently exists, checked with to_regclass on the schema-
// qualified table name. Filtering up front means the shared-doc cross-
// generation guard never references a missing chunk table (a generation row can
// exist before its chunk table, or after a partial reset), which would
// otherwise abort the deleting transaction.
func (s *Sync) existingChunkGenerations(
	ctx context.Context, genIDs []int64,
) ([]int64, error) {
	quotedSchema, err := quoteIdentifier(s.schema)
	if err != nil {
		return nil, fmt.Errorf("quoting schema for chunk-table probe: %w", err)
	}
	var existing []int64
	for _, id := range genIDs {
		var present bool
		if err := s.pg.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`,
			quotedSchema+"."+vectorChunkTable(id)).Scan(&present); err != nil {
			return nil, fmt.Errorf(
				"probing chunk table for generation %d: %w", id, err)
		}
		if present {
			existing = append(existing, id)
		}
	}
	return existing, nil
}

// allVectorGenerationIDs lists every registered generation so eviction can
// check each generation's chunk table before removing a shared doc row.
func (s *Sync) allVectorGenerationIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT id FROM vector_generations`)
	if err != nil {
		return nil, fmt.Errorf("listing vector generations: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning vector generation id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vector generations: %w", err)
	}
	return ids, nil
}

// deleteParkedVectorDocs removes the session's still-parked doc rows — docs
// that vanished from the local mirror — together with every generation's
// chunks for them. The local mirror is shared across generations and kit's
// removal deletes a vanished doc's vectors from every generation
// (sqlitevec's all-generations delete), so PG mirrors that. Preserving
// another generation's chunks instead would leave them referencing a row
// hidden behind the read path's ordinal >= 0 tombstone guard: dead KNN slots
// that can never hydrate, silently shrinking that generation's results.
// genIDs must be pre-filtered to existing chunk tables (see
// existingChunkGenerations) so a missing table cannot abort the tx. Returns
// the number of doc rows deleted.
func deleteParkedVectorDocs(
	ctx context.Context, tx *sql.Tx, sessionID string, genIDs []int64,
) (int, error) {
	for _, id := range genIDs {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE doc_key IN (
    SELECT doc_key FROM vector_documents
     WHERE session_id = $1 AND ordinal < 0)`, vectorChunkTable(id)),
			sessionID); err != nil {
			return 0, fmt.Errorf(
				"clearing parked chunks for session %s: %w", sessionID, err)
		}
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM vector_documents WHERE session_id = $1 AND ordinal < 0`,
		sessionID)
	if err != nil {
		return 0, fmt.Errorf("pruning parked docs for session %s: %w", sessionID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned docs for session %s: %w", sessionID, err)
	}
	return int(affected), nil
}

// deleteOrphanVectorDocs deletes the session's doc rows that no listed
// generation's chunk table still references, for whole-session eviction (the
// session left this pusher's embedded set — possibly just a project-filter
// change, with the docs still present locally). vector_documents is shared
// across generations by doc_key, so a doc another generation still embeds
// survives here — those rows keep their non-negative ordinals and stay
// hydratable, unlike parked rows (see deleteParkedVectorDocs). genIDs must be
// pre-filtered to existing chunk tables (see existingChunkGenerations) so a
// missing table cannot abort the tx. Returns the number of doc rows deleted.
func deleteOrphanVectorDocs(
	ctx context.Context, tx *sql.Tx, sessionID string, genIDs []int64,
) (int, error) {
	var conds strings.Builder
	for _, id := range genIDs {
		fmt.Fprintf(&conds,
			" AND NOT EXISTS (SELECT 1 FROM %s c WHERE c.doc_key = d.doc_key)",
			vectorChunkTable(id))
	}
	stmt := "DELETE FROM vector_documents d WHERE d.session_id = $1" + conds.String()
	result, err := tx.ExecContext(ctx, stmt, sessionID)
	if err != nil {
		return 0, fmt.Errorf("pruning orphan docs for session %s: %w", sessionID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned docs for session %s: %w", sessionID, err)
	}
	return int(affected), nil
}

// halfvecLiteral renders v in pgvector's text input format ("[1,2,3]"), bound
// as a parameter and cast with ::halfvec. It errors on any non-finite element
// (NaN, +Inf, -Inf): pgvector rejects those on input, so a single pathological
// vector would otherwise abort the whole multi-row INSERT with an opaque driver
// error. Catching it here lets the caller attribute the failure to a doc/chunk.
func halfvecLiteral(v []float32) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		f64 := float64(f)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			return "", fmt.Errorf("non-finite embedding value at index %d", i)
		}
		b.WriteString(strconv.FormatFloat(f64, 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

// clearUsageOnlyVectorSessions discovers indexed sessions in PostgreSQL rather
// than the local push candidates, which omit sessions deleted from the archive.
// Usage-only storage retains no vectors for this archive, including generations
// that are no longer active. Other archives' sessions remain untouched.
func (s *Sync) clearUsageOnlyVectorSessions(ctx context.Context) error {
	var exists bool
	if err := s.pg.QueryRowContext(ctx, `SELECT to_regclass('vector_documents') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	owner, err := s.vectorOwnerIdentity(ctx)
	if err != nil {
		return err
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT s.id, s.owner_marker, s.machine
		FROM sessions s
		JOIN (
			SELECT session_id FROM vector_documents
			UNION SELECT session_id FROM vector_push_state
		) indexed ON indexed.session_id = s.id`)
	if err != nil {
		return fmt.Errorf("listing usage-only vector sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		var marker, machine sql.NullString
		if err := rows.Scan(&id, &marker, &machine); err != nil {
			rows.Close()
			return err
		}
		if owner.owns(marker.String, machine.String) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.clearOwnedSessionVectors(ctx, owner, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) clearOwnedSessionVectors(ctx context.Context, owner vectorOwnerIdentity, id string) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Match eviction's ownership lock: another archive may have claimed the
	// session since discovery. A missing owner cannot authorize this deletion.
	var marker, machine sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT owner_marker, machine FROM sessions WHERE id = $1 FOR UPDATE`, id,
	).Scan(&marker, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !owner.owns(marker.String, machine.String) {
		return nil
	}
	if err := clearSessionVectorsTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// clearSessionVectorsTx removes every generation's content for an owned
// session in the same transaction that applies its usage-only projection.
func clearSessionVectorsTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('vector_documents') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	generations, err := existingChunkGenerationsTx(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range generations {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+vectorChunkTable(id)+` WHERE doc_key IN (SELECT doc_key FROM vector_documents WHERE session_id = $1)`, sessionID); err != nil {
			return fmt.Errorf("clearing usage-only vector chunks: %w", err)
		}
	}
	for _, table := range []string{"vector_documents", "vector_push_state"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id = $1`, sessionID); err != nil {
			return fmt.Errorf("clearing usage-only vector content: %w", err)
		}
	}
	return nil
}
