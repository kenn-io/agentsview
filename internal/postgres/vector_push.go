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
)

// VectorGenerationInfo identifies the local embedding generation being pushed.
// Machines whose embedding config produces the same Fingerprint share one PG
// generation (and one chunk table); Model and Dimension are recorded for
// diagnostics and to size the halfvec column.
type VectorGenerationInfo struct {
	Fingerprint string
	Model       string
	Dimension   int
}

// VectorPushChunk is one embedded slice of a document. ChunkIndex is stable
// within a doc_key so re-pushes overwrite the same (doc_key, chunk_index) row.
type VectorPushChunk struct {
	ChunkIndex int
	Embedding  []float32
}

// VectorPushDoc mirrors one local vectors.db document row plus its embeddings.
// DocKey is globally unique and shared across generations, which is why
// vector_documents is a single backend-agnostic table upserted by doc_key
// rather than a per-generation table.
type VectorPushDoc struct {
	DocKey      string
	SessionID   string
	SourceUUID  string
	Ordinal     int
	OrdinalEnd  int
	Subordinate bool
	OffsetsJSON string
	Content     string
	ContentHash string
	Chunks      []VectorPushChunk
}

// VectorPushSource supplies one transaction-owned local export for a PG push
// phase. The export keeps generation metadata, aggregate hashes, and document
// and chunk reads on one SQLite snapshot.
type VectorPushSource interface {
	BeginExport(ctx context.Context, sessionIDs []string) (VectorExport, bool, error)
}

type VectorExport interface {
	Generation() VectorGenerationInfo
	SessionDocHashes(
		ctx context.Context, sessionIDs []string,
	) (map[string]string, error)
	SessionDocs(
		ctx context.Context, sessionID string,
	) ([]VectorPushDoc, string, error)
	Close() error
}

// ErrVectorSourceNotReady marks a Generation error meaning the local vector
// index exists but is not safe to export right now — an embeddings build is
// rewriting it (or one was interrupted), so its session coverage is partial. A
// push that ran anyway would read that partial view as truth and evict or
// overwrite valid PG vectors. pushVectors turns this into a clean phase skip;
// the next push after the build completes sends everything that changed.
var ErrVectorSourceNotReady = errors.New(
	"local vector index is not fully embedded (build in progress or interrupted)")

// VectorPushResult summarizes the vector push phase. Skipped is set (with a
// human reason) when the phase cannot run: no source, no active generation, or
// no pgvector extension. The counters describe what changed on PG.
//
// DocsDeleted counts vector_documents rows removed: on eviction of a whole
// session and when a doc vanished from a re-pushed session (its shared row is
// removed only when no other generation still embeds it). Conflicts counts
// sessions this pusher left untouched because the PG owner marker names a
// different machine — on the push path (a locally changed session PG says
// another machine owns) and on the evict path (an owned-elsewhere session
// absent from local, kept rather than evicted). The user-facing print of
// Conflicts lands with the CLI wiring.
type VectorPushResult struct {
	Skipped           bool
	SkippedReason     string
	SessionsPushed    int
	SessionsUnchanged int
	// SessionsDeferred counts sessions whose vector reconciliation was
	// withheld this run — a failed session-phase push, an export hash
	// that diverged mid-push, or an eviction abandoned because the local
	// generation changed; the delta state is untouched, so the next
	// generation-wide reconciliation sends them.
	SessionsDeferred int
	DocsPushed       int
	ChunksPushed     int
	DocsDeleted      int
	SessionsEvicted  int
	Conflicts        int
	// GenerationID is the PG id of the generation this phase reconciled,
	// zero when the phase was skipped or found no active generation. The
	// watch orchestrator records it after a clean generation-wide pass so
	// a later push against a different generation id — a re-embed, or a
	// reset/drop that recreated the row under any machine — promotes the
	// next scoped push to a generation-wide reconciliation instead of
	// writing only the changed sessions' chunks into it.
	GenerationID int64
}

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
	id              int64
	createdAt       time.Time
	halfvecType     string
	machineRecorded bool
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
	markerID, err := s.pushMarkerID()
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
	onProgress func(PushProgress),
) (VectorPushResult, error) {
	var res VectorPushResult
	if s.vectorSource == nil {
		res.Skipped, res.SkippedReason = true, "no vector source configured"
		return res, nil
	}
	if scope != nil && len(scope) == 0 {
		log.Printf("vector push: no changed sessions; deferring reconciliation to the next generation-wide push")
		return res, nil
	}
	export, hasGen, err := s.vectorSource.BeginExport(ctx, scope)
	if errors.Is(err, ErrVectorSourceNotReady) {
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
		return res, fmt.Errorf("resolving local vector generation: BeginExport returned a nil export")
	}
	defer func() {
		if export != nil {
			_ = export.Close()
		}
	}()
	gen := export.Generation()
	unavailable, err := ensureVectorBaseSchemaPG(ctx, s.pg)
	if err != nil {
		if s.skipVectorsOnPrivilegeError(err, &res) {
			return res, nil
		}
		return res, err
	}
	if unavailable != "" {
		res.Skipped, res.SkippedReason = true, unavailable
		return res, nil
	}
	witnessKey, err := s.vectorGenerationWitnessKey()
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
		initialProbe, found, err := s.lookupVectorGeneration(ctx, gen.Fingerprint, witnessKey)
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
			currentProbe, found, err := s.lookupVectorGeneration(ctx, gen.Fingerprint, witnessKey)
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
		if errors.Is(err, ErrVectorSourceNotReady) {
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
			return res, fmt.Errorf(
				"rechecking local vector generation after scoped promotion: BeginExport returned a nil export",
			)
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
	if scope != nil {
		log.Printf("vector push: change-scoped reconciliation of %d candidate session(s): %d local hash(es), %d PG state row(s) for generation %d",
			len(scope), len(local), len(pgState), resolved.id)
	} else {
		log.Printf("vector push: comparing %d local session(s) against %d PG state row(s) for generation %d",
			len(local), len(pgState), resolved.id)
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
		retryGenerationWide := func(msg string, args ...any) (VectorPushResult, error) {
			log.Printf(msg, args...)
			_ = export.Close()
			export = nil
			return s.pushVectors(
				ctx, full, nil, lastReconciledGeneration, failedSessions, onProgress,
			)
		}
		finalProbe, found, err := s.lookupVectorGeneration(ctx, gen.Fingerprint, witnessKey)
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
		retryGenerationWide := func(msg string, args ...any) (VectorPushResult, error) {
			log.Printf(msg, args...)
			_ = export.Close()
			export = nil
			return s.pushVectors(
				ctx, full, nil, lastReconciledGeneration, failedSessions, onProgress,
			)
		}
		finalProbe, found, err := s.lookupVectorGeneration(ctx, gen.Fingerprint, witnessKey)
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
			ctx, gen.Fingerprint, resolved, witnessKey,
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
	ctx context.Context, fingerprint, witnessKey string,
) (vectorGeneration, bool, error) {
	var genID int64
	var createdAt time.Time
	err := s.pg.QueryRowContext(ctx,
		`SELECT id, created_at FROM vector_generations WHERE fingerprint = $1`,
		fingerprint,
	).Scan(&genID, &createdAt)
	if err == sql.ErrNoRows {
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
    SELECT 1 FROM vector_generation_machines
     WHERE generation_id = $1 AND machine = $2)`,
		genID, witnessKey).Scan(&machineRecorded); err != nil {
		return vectorGeneration{}, false, fmt.Errorf(
			"reading vector push machine record: %w", err,
		)
	}
	return vectorGeneration{
		id:              genID,
		createdAt:       createdAt,
		halfvecType:     extSchema + ".halfvec",
		machineRecorded: machineRecorded,
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
func (s *Sync) skipVectorsOnPrivilegeError(err error, res *VectorPushResult) bool {
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
// the sessions/messages rows its session-phase push failed to write. full
// re-sends
// unchanged sessions too, while still honoring out-of-scope exclusion,
// ownership conflicts, and failed-session deferral. onProgress, when non-nil,
// receives a Phase "vectors" report after every pushed session and every
// vectorProgressStride examined ones, so long delta scans stay visible
// without a callback per unchanged session.
func (s *Sync) applyVectorDeltas(
	ctx context.Context, gen vectorGeneration,
	owner vectorOwnerIdentity, full bool,
	local map[string]string, pgState map[string]vectorPushStateRow,
	export VectorExport, failedSessions map[string]struct{}, onProgress func(PushProgress),
	res *VectorPushResult,
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
		onProgress(PushProgress{
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
		if !full && hasState && prev.docAggHash == agg {
			res.SessionsUnchanged++
			continue
		}
		if _, failed := failedSessions[sessionID]; failed {
			res.SessionsDeferred++
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
			res.SessionsDeferred++
		}
		if outcome.pushed {
			res.SessionsPushed++
			res.DocsPushed += outcome.docs
			res.ChunksPushed += outcome.chunks
			res.DocsDeleted += outcome.deleted
			report()
		} else if examined%vectorProgressStride == 0 {
			report()
		}
	}
	report()

	var evict []string
	for sessionID, st := range pgState {
		if _, inLocal := local[sessionID]; inLocal {
			continue
		}
		if _, skip := outOfScope[sessionID]; skip {
			continue
		}
		if _, failed := failedSessions[sessionID]; failed {
			res.SessionsDeferred++
			continue
		}
		switch {
		case !st.sessionExists || owner.owns(st.ownerMarker, st.machine):
			evict = append(evict, sessionID)
		default:
			res.Conflicts++
		}
	}
	return s.evictVectorSessions(ctx, scope, evict, res)
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
// projects/excludeProjects is set (ValidateProjectFilters rejects both).
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
// set (ValidateProjectFilters rejects both).
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

// resolveVectorGeneration registers the generation, creates its chunk table,
// and reads whether this machine has already completed a generation-wide push
// against it. Its id is the generation
// instance's identity, which a scoped push compares against the last one it
// reconciled generation-wide to decide whether to promote; whether this
// scoped witness predated this call is captured first, because that record is
// the incarnation witness the promotion check falls back on when a recreated
// id sequence hands the new generation the memoized id.
func (s *Sync) resolveVectorGeneration(
	ctx context.Context, gen VectorGenerationInfo, witnessKey string,
) (vectorGeneration, error) {
	genID, err := ensureVectorGeneration(
		ctx, s.pg, gen.Fingerprint, gen.Model, gen.Dimension,
	)
	if err != nil {
		return vectorGeneration{}, err
	}
	if err := ensureVectorChunkTable(ctx, s.pg, genID, gen.Dimension); err != nil {
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
	return vectorGeneration{
		id:              genID,
		createdAt:       createdAt,
		halfvecType:     extSchema + ".halfvec",
		machineRecorded: machineRecorded,
	}, nil
}

func (s *Sync) vectorGenerationWitnessKey() (string, error) {
	markerID, err := s.pushMarkerID()
	if err != nil {
		return "", err
	}
	return s.machine + "|" + s.pushMarkerMetadataKey(pushMarkerKeyPrefix, markerID), nil
}

func (s *Sync) recordVectorGenerationMachine(
	ctx context.Context, fingerprint string, gen vectorGeneration, witnessKey string,
) (bool, error) {
	res, err := s.pg.ExecContext(ctx, `
INSERT INTO vector_generation_machines (generation_id, machine, last_push_at)
SELECT id, $4, now()
  FROM vector_generations
 WHERE id = $1 AND fingerprint = $2 AND created_at = $3
ON CONFLICT (generation_id, machine) DO UPDATE SET last_push_at = EXCLUDED.last_push_at`,
		gen.id, fingerprint, gen.createdAt, witnessKey)
	if err != nil {
		return false, fmt.Errorf("recording vector push machine: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("confirming vector push machine record: %w", err)
	}
	return rows > 0, nil
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
	ctx context.Context, scope vectorPushScope, export VectorExport, sessionID, aggHash string,
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
func upsertVectorDocs(ctx context.Context, tx *sql.Tx, docs []VectorPushDoc) error {
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
	sessionID string, docs []VectorPushDoc,
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

// evictVectorSessions removes each session's chunks and push state for the
// scoped generation and prunes its now-orphaned shared doc rows, one
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
	res *VectorPushResult,
) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	table := vectorChunkTable(scope.gen.id)
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
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE doc_key IN (
    SELECT doc_key FROM vector_documents WHERE session_id = $1)`, table),
			sessionID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("evicting chunks for session %s: %w", sessionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM vector_push_state
 WHERE generation_id = $1 AND session_id = $2`,
			scope.gen.id, sessionID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("evicting push state for session %s: %w", sessionID, err)
		}
		deleted, err := deleteOrphanVectorDocs(ctx, tx, sessionID, scope.genIDs)
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
	stmt := fmt.Sprintf(
		`DELETE FROM vector_documents d WHERE d.session_id = $1%s`,
		conds.String())
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
