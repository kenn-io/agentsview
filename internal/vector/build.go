package vector

import (
	"context"
	"database/sql"
	"fmt"
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

// chunksTable is the kit-managed table mapping each embedded chunk to its
// vec0 row, scoped by generation ordinal and doc_key.
const chunksTable = vectorsPrefix + "_chunks"

// BuildOptions configures one Build pass.
type BuildOptions struct {
	// FullRebuild forces every document to be re-embedded under the target
	// generation's fingerprint, even if it is already the active one.
	FullRebuild bool
	// Backstop forces a full mirror reconciliation scan (ignoring the
	// refresh watermark) without forcing a re-embed.
	Backstop bool
	// BatchSize is the encode batch size (config batch_size).
	BatchSize int
	// Progress, if non-nil, is called at most ~every 2s with incremental
	// embedding progress, plus once more after the run completes.
	Progress func(BuildProgress)
}

// BuildProgress reports incremental embedding progress during Build.
type BuildProgress struct {
	Phase string // "scanning" | "embedding"
	Done  int64  // chunks encoded so far
	Total int64  // pending docs at start (approximate denominator)
}

// BuildResult summarizes one Build call.
type BuildResult struct {
	Fingerprint string
	Activated   bool // building generation auto-activated on completion
	Refresh     RefreshStats
	Fill        kitvec.FillStats
}

// Build runs one embedding pass against gen (the desired vector space, from
// config: Model, Dimensions, Params{"max_input_chars": itoa(n)}). It
// refreshes the vector_messages mirror, resolves which generation to fill
// (top-up the active one, start a new building generation, or reset and
// refill the active one for FullRebuild), fills pending documents, and
// auto-activates a building generation once it fully covers the mirror.
func (ix *Index) Build(
	ctx context.Context, src MessageSource, enc kitvec.EncodeFunc,
	gen kitvec.Generation, o BuildOptions,
) (BuildResult, error) {
	if err := ix.requireWritable(); err != nil {
		return BuildResult{}, err
	}

	firstEver, err := ix.noWatermarkYet(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	refreshStats, err := ix.Refresh(ctx, src, o.FullRebuild || o.Backstop || firstEver)
	if err != nil {
		return BuildResult{}, err
	}

	fp := gen.Fingerprint()
	target, wasBuilding, err := ix.resolveBuildTarget(ctx, gen, fp, o.FullRebuild)
	if err != nil {
		return BuildResult{}, err
	}

	total, err := ix.countPending(ctx, target)
	if err != nil {
		return BuildResult{}, err
	}

	wrapped, finish := ix.wrapProgress(enc, total, o.Progress)
	fillStats, fillErr := kitvec.Fill[string, string](ctx, ix.store, target, wrapped, kitvec.FillOptions[string]{
		Split: ix.split,
		Batch: kitvec.BatchOptions{BatchSize: o.BatchSize, Concurrency: 1},
	})
	finish()
	result := BuildResult{Fingerprint: target, Refresh: refreshStats, Fill: fillStats}
	if fillErr != nil {
		return result, fillErr
	}

	activated, err := ix.maybeActivate(ctx, target, wasBuilding)
	if err != nil {
		return result, err
	}
	result.Activated = activated
	return result, nil
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
			if err := ix.resetGeneration(ctx, fp); err != nil {
				return "", false, err
			}
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
	if fullRebuild && existed {
		if err := ix.resetGeneration(ctx, target); err != nil {
			return "", false, err
		}
	}
	return target, true, nil
}

// generationExists reports whether a generation with fingerprint fp has
// already been registered, in any state.
func (ix *Index) generationExists(ctx context.Context, fp string) (bool, error) {
	var ordinal int64
	err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal FROM `+generationsTable+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check generation exists for fingerprint %s: %w", fp, err)
	}
	return true, nil
}

// countPending returns the number of vector_messages rows not yet stamped
// at their current content_hash for fp's generation, an approximate
// denominator for BuildProgress.Total. It applies the same
// s.revision = d.content_hash predicate generationCoverageQuery's Missing
// column uses, so a document whose content changed since it was last
// stamped (a stale revision) counts as pending rather than complete — kit's
// Fill treats it as pending re-embed for the same reason.
func (ix *Index) countPending(ctx context.Context, fp string) (int64, error) {
	ordinal, err := ix.ordinalForFingerprint(ctx, fp)
	if err != nil {
		return 0, err
	}
	var total int64
	if err := ix.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM vector_messages d WHERE NOT EXISTS (
    SELECT 1 FROM `+stampsTable+` s WHERE s.ordinal = ? AND s.doc_key = d.doc_key
      AND s.revision = d.content_hash)`,
		ordinal,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("count pending documents: %w", err)
	}
	return total, nil
}

// ordinalForFingerprint looks up a generation's generations-table ordinal
// from its fingerprint (kit's store uses the fingerprint as gen_key).
func (ix *Index) ordinalForFingerprint(ctx context.Context, fp string) (int64, error) {
	var ordinal int64
	if err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal FROM `+generationsTable+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("lookup generation ordinal for fingerprint %s: %w", fp, err)
	}
	return ordinal, nil
}

// resetGeneration clears fp's generation of all embedded state (its vec0
// vectors, chunk map, and stamps) in a single transaction, so a subsequent
// Fill call re-embeds every document from scratch. It leaves the
// generation row itself (and its state) untouched.
func (ix *Index) resetGeneration(ctx context.Context, fp string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var ordinal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT ordinal FROM `+generationsTable+` WHERE gen_key = ?`, fp,
	).Scan(&ordinal); err != nil {
		return fmt.Errorf("lookup generation ordinal for fingerprint %s: %w", fp, err)
	}

	vecTable := fmt.Sprintf("%s_v%d", vectorsPrefix, ordinal)
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+vecTable); err != nil {
		return fmt.Errorf("clearing vec0 table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+chunksTable+` WHERE ordinal = ?`, ordinal); err != nil {
		return fmt.Errorf("clearing chunk map: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+stampsTable+` WHERE ordinal = ?`, ordinal); err != nil {
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
		`UPDATE `+generationsTable+` SET state = ? WHERE state = ? AND gen_key != ?`,
		string(sqlitevec.StateRetired), string(sqlitevec.StateActive), target,
	); err != nil {
		return fmt.Errorf("retire old active generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+generationsTable+` SET state = ? WHERE gen_key = ?`,
		string(sqlitevec.StateActive), target,
	); err != nil {
		return fmt.Errorf("activate generation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activate generation: %w", err)
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
