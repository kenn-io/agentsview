package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// progressInterval bounds how often BuildOptions.Progress is invoked during
// an embedding pass. A final call always fires once Fill returns,
// regardless of this interval, so callers see a Done total that matches
// the completed (or aborted) run. Tests lower it to 0 for determinism.
var progressInterval = 2 * time.Second

const repairStatusCountTimeout = 2 * time.Second

// CorpusFingerprintParam names the generation parameter whose value identifies
// the source corpus, independently of the embedding model. A change forces a
// full mirror reconciliation before the new vector generation is filled.
const CorpusFingerprintParam = "corpus_fingerprint"

const corpusFingerprintMetaKey = "corpus_fingerprint"

const completedCorpusRevisionMetaKey = "completed_corpus_revision:"

// BuildOptions configures one Build pass.
type BuildOptions struct {
	// FullRebuild forces every document to be re-embedded under the target
	// generation's fingerprint, even if it is already the active one.
	FullRebuild bool
	// Backstop forces a full mirror reconciliation scan (ignoring the
	// refresh watermark) without forcing a re-embed.
	Backstop bool
	// RepairInvalid scans the configured existing generation for malformed,
	// structurally incomplete, wrong-dimension, non-finite, or zero-norm
	// vectors and runs a fill restricted to only the affected documents.
	RepairInvalid bool
	// IncludeAutomated controls whether automated sessions' units are
	// scanned into the mirror at all (see UnitSource.ScanEmbeddableUnits).
	// It is part of the mirror's identity: Build compares it against the
	// scope the mirror was last refreshed under (the metadata table) and
	// forces a full reconciliation scan on any change, so now-out-of-scope
	// rows (and their vectors) are removed and newly-in-scope sessions older
	// than the refresh watermark are picked up. It does not force a re-embed
	// of documents that stay in scope.
	IncludeAutomated bool
	// BatchSize is the encode batch size (config batch_size).
	BatchSize int
	// Concurrency is the number of documents encoded in parallel (config
	// concurrency). Values <= 0 encode sequentially.
	Concurrency int
	// CorpusRevision identifies the source state captured when this build
	// target was resolved. A successful ordinary build persists it only after
	// refresh and fill complete, allowing searches to reject newer source data.
	CorpusRevision string
	// Progress, if non-nil, is called at most ~every 2s with incremental
	// embedding progress, plus once more after the run completes.
	Progress func(BuildProgress)
}

// BuildProgress reports incremental embedding progress during Build. Done
// and Total are both counted in chunks (not documents): kit's Fill hands the
// wrapped encoder each document's chunks, sometimes split across several
// sub-batches when BatchSize is smaller than a document's chunk count, so
// there is no seam to count completed documents from the encoder wrapper
// alone. Counting chunks on both sides keeps the percentage bounded at 100%
// regardless of how many chunks a message splits into.
type BuildProgress struct {
	Phase string // "scanning" | "embedding"
	Done  int64  // chunks encoded so far
	Total int64  // pending chunks at start (approximate denominator)
}

// BuildResult summarizes one Build call.
type BuildResult struct {
	Fingerprint string
	Activated   bool // building generation auto-activated on completion
	Refresh     RefreshStats
	Repair      RepairStats
	Fill        kitvec.FillStats
}

// Build runs one embedding pass against gen (the desired vector space, from
// config: Model, Dimensions, and the fingerprinted Params — max_input_chars,
// doc_unit_scheme, and chunk_overlap_chars; see vectorGeneration in
// cmd/agentsview/embeddings.go). It
// Ordinary builds refresh the mirror, resolve which generation to fill
// (top-up the active one, start a new building generation, or reset and
// refill the active one for FullRebuild), fill pending documents, and
// auto-activate a building generation once it fully covers the mirror.
// RepairInvalid instead operates only on an existing generation and never
// refreshes the mirror or changes generation state.
func (ix *Index) Build(
	ctx context.Context, src UnitSource, enc kitvec.EncodeFunc,
	gen kitvec.Generation, o BuildOptions,
) (BuildResult, error) {
	if err := ix.requireWritable(); err != nil {
		return BuildResult{}, err
	}
	if o.RepairInvalid && o.FullRebuild {
		return BuildResult{}, fmt.Errorf("repair-invalid and full-rebuild are mutually exclusive")
	}
	if o.RepairInvalid && o.Backstop {
		return BuildResult{}, fmt.Errorf("repair-invalid and backstop are mutually exclusive")
	}
	if o.RepairInvalid {
		return ix.buildInvalidRepair(ctx, enc, gen, o)
	}

	firstEver, err := ix.noWatermarkYet(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	storedScope, hasScope, err := ix.storedIncludeAutomatedScope(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	// A mirror refreshed before the scope key existed (a refresh watermark
	// is stamped, but scope_include_automated is not) predates this scope
	// feature entirely. Treat that the same as a genuine scope change: force
	// one full reconciliation now so any automated rows that were never
	// meant to be in scope (or, if the config default is true, newly
	// in-scope automated sessions older than the watermark) get resolved,
	// then setIncludeAutomatedScope below stamps the key so every later
	// build compares against a real stored scope again.
	scopeChanged := !hasScope || storedScope != o.IncludeAutomated
	corpusFingerprint := gen.Params[CorpusFingerprintParam]
	storedCorpusFingerprint, hasCorpusFingerprint, err := ix.metaGet(
		ctx, corpusFingerprintMetaKey,
	)
	if err != nil {
		return BuildResult{}, fmt.Errorf("reading corpus fingerprint: %w", err)
	}
	corpusChanged := corpusFingerprint != "" &&
		(!hasCorpusFingerprint || storedCorpusFingerprint != corpusFingerprint)
	full := o.FullRebuild || o.Backstop || firstEver || scopeChanged || corpusChanged
	fp := gen.Fingerprint()
	if full {
		if err := ix.clearCompletedCorpusRevision(ctx, fp); err != nil {
			return BuildResult{}, err
		}
	}
	// Report the scanning phase before the mirror refresh: on a large archive
	// the refresh (and the pending count below) can run for a while with no
	// chunk totals yet, and without a phase report a progress consumer can
	// only render a misleading "0/0 chunks".
	if o.Progress != nil {
		o.Progress(BuildProgress{Phase: "scanning"})
	}
	refreshStats, err := ix.Refresh(ctx, src, full, o.IncludeAutomated)
	if err != nil {
		return BuildResult{}, err
	}
	if err := ix.setIncludeAutomatedScope(ctx, o.IncludeAutomated); err != nil {
		return BuildResult{}, err
	}
	if corpusFingerprint != "" {
		if err := ix.metaSet(ctx, corpusFingerprintMetaKey, corpusFingerprint); err != nil {
			return BuildResult{}, fmt.Errorf("storing corpus fingerprint: %w", err)
		}
	}

	target, wasBuilding, err := ix.resolveBuildTarget(ctx, gen, fp, o.FullRebuild)
	if err != nil {
		return BuildResult{}, err
	}
	result := BuildResult{Fingerprint: target, Refresh: refreshStats}

	total, err := ix.countPending(ctx, target)
	if err != nil {
		return result, err
	}

	wrapped, finish := ix.wrapProgress(validatingEncoder(enc), total, o.Progress)
	fillStore := &repairQueueCompletingStore{
		Store: ix.store,
		db:    ix.db,
		spec:  ix.spec,
	}
	fillStats, fillErr := kitvec.Fill[string, string](ctx, fillStore, target, wrapped, kitvec.FillOptions[string]{
		Split:         ix.split,
		Batch:         kitvec.BatchOptions{BatchSize: o.BatchSize, Concurrency: 1},
		Concurrency:   o.Concurrency,
		OnEncodeError: skipPermanentEncodeError,
	})
	finish()
	result.Fill = fillStats
	if fillErr != nil {
		return result, fillErr
	}

	activated, err := ix.maybeActivate(ctx, target, wasBuilding)
	if err != nil {
		return result, err
	}
	if err := ix.clearActiveFullRebuildPending(ctx, target); err != nil {
		return result, err
	}
	result.Activated = activated
	if o.CorpusRevision != "" {
		if err := ix.metaSet(
			ctx, completedCorpusRevisionMetaKey+target, o.CorpusRevision,
		); err != nil {
			return result, fmt.Errorf("storing completed corpus revision: %w", err)
		}
	}
	return result, nil
}

// buildInvalidRepair is separate from the ordinary build path so repair cannot
// accidentally refresh or expand the mirror, change its stored scope, create a
// generation, or alter generation state. repairInvalidVectors verifies that
// the configured fingerprint already exists before invalidating anything.
func (ix *Index) buildInvalidRepair(
	ctx context.Context, enc kitvec.EncodeFunc, gen kitvec.Generation, o BuildOptions,
) (BuildResult, error) {
	target := gen.Fingerprint()
	result := BuildResult{Fingerprint: target}
	if o.Progress != nil {
		o.Progress(BuildProgress{Phase: "scanning"})
	}
	repair, err := ix.repairInvalidVectors(ctx, target)
	result.Repair = repair.Stats
	if err != nil {
		if repair.Stats.Scanned {
			store := &repairStore{
				base: ix.store, db: ix.db, spec: ix.spec, fingerprint: target,
				ordinal: repair.Ordinal, queueTable: ix.spec.repairQueueTable(), split: ix.split,
			}
			remaining, remainingKnown, countErr := repairRemaining(
				ctx, store, result.Repair.Documents,
			)
			result.Repair.Remaining = remaining
			result.Repair.RemainingKnown = remainingKnown
			return result, errors.Join(err, countErr)
		}
		return result, err
	}
	store := &repairStore{
		base: ix.store, db: ix.db, spec: ix.spec, fingerprint: target,
		ordinal: repair.Ordinal, queueTable: ix.spec.repairQueueTable(), split: ix.split,
	}
	total, err := store.countPendingChunks(ctx, ix.split)
	if err != nil {
		remaining, remainingKnown, countErr := repairRemaining(
			ctx, store, result.Repair.Documents,
		)
		result.Repair.Remaining = remaining
		result.Repair.RemainingKnown = remainingKnown
		return result, errors.Join(err, countErr)
	}

	wrapped, finish := ix.wrapProgress(validatingEncoder(enc), total, o.Progress)
	fill, fillErr := fillRepairQueue(ctx, store, target, wrapped, repairFillOptions{
		Split:       ix.split,
		Batch:       kitvec.BatchOptions{BatchSize: o.BatchSize, Concurrency: 1},
		Concurrency: o.Concurrency,
	})
	finish()
	result.Fill = fill.Stats
	result.Repair.Failed = fill.Failed
	fallback := max(result.Repair.Documents-result.Fill.Documents, 0)
	remaining, remainingKnown, countErr := repairRemaining(ctx, store, fallback)
	result.Repair.Remaining = remaining
	result.Repair.RemainingKnown = remainingKnown
	if countErr != nil {
		return result, errors.Join(fillErr, countErr)
	}
	if fillErr != nil {
		return result, fillErr
	}
	if fill.Failed > 0 {
		return result, fmt.Errorf(
			"invalid vector repair incomplete: %d permanently rejected targets remain queued; first failure: %v",
			fill.Failed, fill.FirstFailure)
	}
	if remaining > 0 {
		return result, fmt.Errorf(
			"invalid vector repair incomplete: %d targets remain queued", remaining)
	}
	return result, nil
}

// repairRemaining preserves the accumulated committed target count as a
// fallback, then attempts a bounded recount independent of a canceled build
// context. Cancellation during any post-scan phase must not hide queue work
// that the scan already committed.
func repairRemaining(
	ctx context.Context, store *repairStore, fallback int,
) (remaining int, known bool, err error) {
	countCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), repairStatusCountTimeout)
	defer cancel()
	remaining, err = store.countTargets(countCtx)
	if err != nil {
		return fallback, false, err
	}
	return remaining, true, nil
}

func validatingEncoder(enc kitvec.EncodeFunc) kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors, err := enc(ctx, texts)
		if err != nil {
			return nil, err
		}
		if err := validateEmbeddings(vectors); err != nil {
			return nil, err
		}
		return vectors, nil
	}
}

// skipPermanentEncodeError implements kitvec.FillOptions.OnEncodeError: a
// document the embeddings endpoint permanently rejects for input-specific
// reasons (e.g. a token-window overflow, whitespace-only content some servers
// refuse, or a content-policy rejection) is skipped — kit stamps it for the
// generation with no vectors so it stops being pending — instead of aborting
// the whole fill. Without this, one poison document would wedge every future
// build at the same doc_key-ordered scan position: later documents would never
// embed, a first build would never reach Missing==0, and auto-activation would
// never fire.
//
// Every other failure (5xx, network, timeout, 429 rate-limiting, auth, route,
// model, media-type, or other config/API failures) still aborts the fill, since
// the next scheduled build should retry the document rather than silently
// giving up on it.
func skipPermanentEncodeError(doc string, err error) bool {
	if !isPermanentEncodeError(err) {
		return false
	}
	log.Printf("vector build: skipping document %s: permanently rejected by embeddings endpoint: %v",
		doc, err)
	return true
}

func isPermanentEncodeError(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr != nil && statusErr.Permanent()
}

// noWatermarkYet reports whether Refresh has never advanced the stored
// refresh watermark, i.e. this would be the mirror's first scan.
func (ix *Index) noWatermarkYet(ctx context.Context) (bool, error) {
	watermark, err := ix.refreshWatermark(ctx)
	if err != nil {
		return false, err
	}
	return watermark == "", nil
}

// resolveBuildTarget decides which generation fingerprint Build should fill
// and whether it is a newly (or still) building generation that should be
// auto-activated once it fully covers the mirror. See the package's build
// brief for the exact decision table.
//
// FullRebuild resets the target generation whenever it already exists —
// active, building, or retired — not only when it happens to be the active
// one: a fingerprint that already exists as a retired (or still building)
// generation carries stamps and vectors from its earlier life, and without
// a reset EnsureGeneration would reuse them, letting Fill skip every
// document and silently reactivate stale embeddings instead of performing
// the requested full rebuild.
func (ix *Index) resolveBuildTarget(
	ctx context.Context, gen kitvec.Generation, fp string, fullRebuild bool,
) (target string, wasBuilding bool, err error) {
	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return "", false, err
	}

	if hasActive && active == fp {
		if fullRebuild {
			if err := ix.markActiveFullRebuildPending(ctx, fp); err != nil {
				return "", false, err
			}
			if err := ix.resetGenerationForFullRebuild(ctx, fp); err != nil {
				_ = ix.clearActiveFullRebuildPending(ctx, fp)
				return "", false, err
			}
		}
		// fp is already the active generation, so anything else still in
		// state building was abandoned by an earlier failed first build
		// (the config since reverted back to this active fingerprint) and
		// would otherwise stay building forever: this is the only path
		// through resolveBuildTarget that reaches an active fp without
		// going through EnsureGeneration, which is where the abandoned-gen
		// retirement normally happens.
		if err := ix.retireAbandonedBuildingGenerations(ctx, fp); err != nil {
			return "", false, err
		}
		return fp, false, nil
	}

	existed, err := ix.generationExists(ctx, fp)
	if err != nil {
		return "", false, err
	}

	target, err = ix.EnsureGeneration(ctx, gen, sqlitevec.StateBuilding)
	if err != nil {
		return "", false, err
	}
	if err := ix.retireAbandonedBuildingGenerations(ctx, target); err != nil {
		return "", false, err
	}
	if fullRebuild && existed {
		if err := ix.resetGenerationForFullRebuild(ctx, target); err != nil {
			return "", false, err
		}
	}
	return target, true, nil
}

// resetGenerationForFullRebuild invalidates the generation's completed corpus
// revision before removing its vectors and stamps. Clearing first keeps
// StaleActive fail-closed if either the reset or the subsequent fill fails.
// Build writes the completed revision again only after fill and activation
// succeed.
func (ix *Index) resetGenerationForFullRebuild(ctx context.Context, fp string) error {
	if err := ix.clearCompletedCorpusRevision(ctx, fp); err != nil {
		return err
	}
	return ix.resetGeneration(ctx, fp)
}

func (ix *Index) clearCompletedCorpusRevision(ctx context.Context, fp string) error {
	if _, err := ix.db.ExecContext(ctx,
		`DELETE FROM `+ix.spec.MetaTable+` WHERE key = ?`,
		completedCorpusRevisionMetaKey+fp,
	); err != nil {
		return fmt.Errorf("clearing completed corpus revision: %w", err)
	}
	return nil
}

// retireAbandonedBuildingGenerations transitions every generation still in
// state building other than keep to retired. A generation is abandoned
// when the embedding config (model, dimensions, or params) changes mid
// first-build: resolveBuildTarget starts a fresh building generation under
// the new fingerprint, but the old one never got the chance to activate or
// retire itself and would otherwise stay in state building forever —
// fingerprintByState's ORDER BY ordinal LIMIT 1 could then report the
// abandoned generation's stale coverage as BuildingError's percent instead
// of the generation actually being built.
//
// This only changes state; kit's store has no API to drop a generation's
// vec0 table, chunk map, or stamps, so an abandoned generation's rows stay
// on disk (bloating vectors.db) until an operator rebuilds vectors.db from
// scratch or a future kit API adds reclamation.
func (ix *Index) retireAbandonedBuildingGenerations(ctx context.Context, keep string) error {
	if _, err := ix.db.ExecContext(ctx,
		`UPDATE `+ix.spec.generationsTable()+` SET state = ? WHERE state = ? AND gen_key != ?`,
		string(sqlitevec.StateRetired), string(sqlitevec.StateBuilding), keep,
	); err != nil {
		return fmt.Errorf("retire abandoned building generations: %w", err)
	}
	return nil
}

// generationExists reports whether a generation with fingerprint fp has
// already been registered, in any state.
func (ix *Index) generationExists(ctx context.Context, fp string) (bool, error) {
	var ordinal int64
	err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal FROM `+ix.spec.generationsTable()+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check generation exists for fingerprint %s: %w", fp, err)
	}
	return true, nil
}

// countPending returns the total number of chunks the documents not yet
// stamped at their current content_hash (for fp's generation) would produce
// under the index's split configuration — the denominator BuildProgress.Total
// reports. It counts chunks rather than documents so the denominator stays
// in the same unit as BuildProgress.Done (chunks encoded so far); see
// BuildProgress's doc comment for why a per-document count isn't reachable
// from the encoder wrapper. It applies the same s.revision = d.content_hash
// predicate generationCoverageQuery's Missing column uses, so a document
// whose content changed since it was last stamped (a stale revision) counts
// as pending rather than complete — kit's Fill treats it as pending re-embed
// for the same reason.
func (ix *Index) countPending(ctx context.Context, fp string) (int64, error) {
	ordinal, err := ix.ordinalForFingerprint(ctx, fp)
	if err != nil {
		return 0, err
	}
	rows, err := ix.db.QueryContext(ctx, `
SELECT content FROM `+ix.spec.DocsTable+` d WHERE NOT EXISTS (
    SELECT 1 FROM `+ix.spec.stampsTable()+` s WHERE s.ordinal = ? AND s.doc_key = d.doc_key
      AND s.revision = d.content_hash)`,
		ordinal,
	)
	if err != nil {
		return 0, fmt.Errorf("count pending documents: %w", err)
	}
	defer rows.Close()

	var total int64
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return 0, fmt.Errorf("scanning pending document content: %w", err)
		}
		total += int64(len(kitvec.Split(content, ix.split)))
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating pending documents: %w", err)
	}
	return total, nil
}

// ordinalForFingerprint looks up a generation's generations-table ordinal
// from its fingerprint (kit's store uses the fingerprint as gen_key).
func (ix *Index) ordinalForFingerprint(ctx context.Context, fp string) (int64, error) {
	var ordinal int64
	if err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal FROM `+ix.spec.generationsTable()+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("lookup generation ordinal for fingerprint %s: %w", fp, err)
	}
	return ordinal, nil
}

// resetGeneration clears fp's generation of all embedded state (its vec0
// vectors, chunk map, and stamps) in a single transaction, so a subsequent
// Fill call re-embeds every document from scratch. It leaves the generation
// row itself (and its state) untouched, along with durable repair targets:
// an ordinary build may complete them only with a non-empty validated save;
// repair may also complete a revision-current, verified zero-chunk document.
func (ix *Index) resetGeneration(ctx context.Context, fp string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var ordinal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT ordinal FROM `+ix.spec.generationsTable()+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal); err != nil {
		return fmt.Errorf("lookup generation ordinal for fingerprint %s: %w", fp, err)
	}

	vecTable := fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, ordinal)
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+vecTable); err != nil {
		return fmt.Errorf("clearing vec0 table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+ix.spec.chunksTable()+` WHERE ordinal = ?`, ordinal); err != nil {
		return fmt.Errorf("clearing chunk map: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+ix.spec.stampsTable()+` WHERE ordinal = ?`, ordinal); err != nil {
		return fmt.Errorf("clearing stamps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset generation: %w", err)
	}
	return nil
}

// maybeActivate activates target (retiring the previous active generation)
// when it was a building generation whose fill just brought its coverage
// of the mirror to zero Missing documents. It is a no-op, returning false,
// for the active-generation top-up and full-rebuild-in-place cases, which
// never pass wasBuilding=true.
func (ix *Index) maybeActivate(ctx context.Context, target string, wasBuilding bool) (bool, error) {
	if !wasBuilding {
		return false, nil
	}
	ordinal, err := ix.ordinalForFingerprint(ctx, target)
	if err != nil {
		return false, err
	}
	info, err := ix.GenerationByID(ctx, ordinal)
	if err != nil {
		return false, err
	}
	if info.Missing != 0 {
		return false, nil
	}
	if err := ix.activateGeneration(ctx, target); err != nil {
		return false, err
	}
	return true, nil
}

// activateGeneration retires whichever generation is currently active
// (other than target, a no-op when there is none) and activates target,
// in one transaction.
func (ix *Index) activateGeneration(ctx context.Context, target string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activate generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE `+ix.spec.generationsTable()+` SET state = ? WHERE state = ? AND gen_key != ?`,
		string(sqlitevec.StateRetired), string(sqlitevec.StateActive), target,
	); err != nil {
		return fmt.Errorf("retire old active generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+ix.spec.generationsTable()+` SET state = ? WHERE gen_key = ?`,
		string(sqlitevec.StateActive), target,
	); err != nil {
		return fmt.Errorf("activate generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+ix.spec.MetaTable+` WHERE key = ? AND value != ?`,
		activeFullRebuildKey, target,
	); err != nil {
		return fmt.Errorf("clear stale active full rebuild marker: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activate generation: %w", err)
	}
	return nil
}

// ActiveFullRebuildPending reports whether fingerprint is the active
// generation of a same-fingerprint full rebuild that cleared stamps in place
// and has not completed yet.
func (ix *Index) ActiveFullRebuildPending(
	ctx context.Context, fingerprint string,
) (bool, error) {
	value, ok, err := ix.metaGet(ctx, activeFullRebuildKey)
	if err != nil {
		return false, err
	}
	return ok && value == fingerprint, nil
}

func (ix *Index) markActiveFullRebuildPending(
	ctx context.Context, fingerprint string,
) error {
	if err := ix.requireWritable(); err != nil {
		return err
	}
	if err := ix.metaSet(ctx, activeFullRebuildKey, fingerprint); err != nil {
		return fmt.Errorf("record active full rebuild: %w", err)
	}
	return nil
}

func (ix *Index) clearActiveFullRebuildPending(
	ctx context.Context, fingerprint string,
) error {
	if err := ix.requireWritable(); err != nil {
		return err
	}
	value, ok, err := ix.metaGet(ctx, activeFullRebuildKey)
	if err != nil {
		return fmt.Errorf("read active full rebuild marker: %w", err)
	}
	if !ok || value != fingerprint {
		return nil
	}
	if err := ix.metaDelete(ctx, activeFullRebuildKey); err != nil {
		return fmt.Errorf("clear active full rebuild marker: %w", err)
	}
	return nil
}

// wrapProgress wraps enc so every successful encode call atomically adds
// its chunk count to a running total and, when onProgress is non-nil,
// reports it at most once per progressInterval. The returned finish func
// always reports the final count once, regardless of the interval, so the
// caller sees a Done total matching the completed (or aborted) run.
func (ix *Index) wrapProgress(
	enc kitvec.EncodeFunc, total int64, onProgress func(BuildProgress),
) (kitvec.EncodeFunc, func()) {
	if onProgress == nil {
		return enc, func() {}
	}

	var (
		done atomic.Int64
		mu   sync.Mutex
		last time.Time
	)
	report := func(force bool) {
		mu.Lock()
		if !force && time.Since(last) < progressInterval {
			mu.Unlock()
			return
		}
		last = time.Now()
		mu.Unlock()
		onProgress(BuildProgress{
			Phase: "embedding",
			Done:  done.Load(),
			Total: total,
		})
	}

	wrapped := func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors, err := enc(ctx, texts)
		if err != nil {
			return nil, err
		}
		done.Add(int64(len(texts)))
		report(false)
		return vectors, nil
	}
	return wrapped, func() { report(true) }
}
