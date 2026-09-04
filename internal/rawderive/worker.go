package rawderive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// jobErrorCode maps an arbitrary pipeline error onto a fixed allowlisted
// code. Provider and materializer errors can embed client source paths or raw
// content, so only these codes may be persisted or returned to callers;
// errors.Is classification on the original error stays internal.
func jobErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, rawsync.ErrNotFound):
		return "object_not_found"
	case errors.Is(err, rawsync.ErrMissingObject):
		return "missing_object"
	case errors.Is(err, rawsync.ErrConflict):
		return "conflict"
	case errors.Is(err, rawsync.ErrInvalid):
		return "invalid"
	default:
		return "internal"
	}
}

// jobErrorDiagnostic renders the allowlisted durable diagnostic for one
// failed pipeline attempt: its stage plus the error code, with a cleanup
// suffix when a materialization cleanup failure joined the outcome.
func jobErrorDiagnostic(stage string, err error) string {
	code := jobErrorCode(err)
	if stage != "" {
		code = stage + ":" + code
	}
	if errors.Is(err, errMaterializationCleanup) {
		code += "+cleanup_failed"
	}
	return code
}

// reportableJobError builds the caller-facing error for one lease outcome.
// Only allowlisted diagnostics and safe sentinels appear; the underlying
// pipeline error never reaches callers who might log it.
func reportableJobError(id int64, stage string, err error) error {
	detail := ""
	if errors.Is(err, errMaterializationCleanup) {
		detail = " (materialization cleanup failed)"
	}
	switch {
	case errors.Is(err, ErrLeaseLost):
		return fmt.Errorf("raw parse job %d: %s: %w%s", id, stage, ErrLeaseLost, detail)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("raw parse job %d: %s: %w%s", id, stage, context.Canceled, detail)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("raw parse job %d: %s: %w%s", id, stage, context.DeadlineExceeded, detail)
	default:
		return fmt.Errorf("raw parse job %d: %s%s", id, jobErrorDiagnostic(stage, err), detail)
	}
}

// JobQueue owns fenced parse-job leasing and retry state.
type JobQueue interface {
	ClaimRawParseJobs(context.Context, string, int, time.Duration) ([]JobLease, error)
	HeartbeatRawParseJob(context.Context, JobLease, time.Duration) error
	RetryRawParseJob(context.Context, JobLease, time.Time, string, string) error
	FailRawParseJob(context.Context, JobLease, string, string) error
}

// ManifestSource loads the authoritative manifest named by a job lease.
type ManifestSource interface {
	Load(context.Context, JobLease) (rawsync.CanonicalManifest, error)
}

// SourceMaterializer reconstructs one verified, isolated provider tree.
type SourceMaterializer interface {
	Materialize(context.Context, rawsync.CanonicalManifest) (*Materialization, error)
}

// SourceParser invokes the provider-owned parser over a materialized tree.
type SourceParser interface {
	Parse(context.Context, rawsync.CanonicalManifest, *Materialization) (ParsedManifest, error)
}

// ProjectionSink atomically applies a normalized outcome and completes the
// supplied fenced lease. Implementations must change neither projection nor
// job state when the manifest is no longer the current source head.
type ProjectionSink interface {
	Project(context.Context, JobLease, rawsync.CanonicalManifest, ParsedManifest) error
}

// WorkerConfig supplies bounded parse-worker dependencies and timing.
type WorkerConfig struct {
	Queue             JobQueue
	Manifests         ManifestSource
	Materializer      SourceMaterializer
	Parser            SourceParser
	Projection        ProjectionSink
	Owner             string
	BatchSize         int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	// AttemptTimeout bounds how long one attempt may keep renewing its
	// durable lease. When the deadline fires, the attempt's contexts cancel
	// and heartbeat renewal stops, so the lease expires and another worker
	// can reclaim the job. The deadline cannot stop arbitrary provider code:
	// a non-cooperative stage that ignores its context can still block this
	// worker's goroutine, but it can no longer keep its lease alive.
	AttemptTimeout time.Duration
	RetryBase      time.Duration
	RetryMax       time.Duration
	MaxAttempts    int
}

// Worker processes bounded batches of leased raw parse jobs.
type Worker struct {
	Queue             JobQueue
	Manifests         ManifestSource
	Materializer      SourceMaterializer
	Parser            SourceParser
	Projection        ProjectionSink
	Owner             string
	BatchSize         int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	AttemptTimeout    time.Duration
	RetryBase         time.Duration
	RetryMax          time.Duration
	MaxAttempts       int
	now               func() time.Time
}

// BatchResult summarizes durable outcomes for one claimed batch.
type BatchResult struct {
	Claimed   int
	Succeeded int
	Retried   int
	Failed    int
	LeaseLost int
}

// NewWorker validates and constructs a raw parse worker.
func NewWorker(config WorkerConfig) (*Worker, error) {
	if config.Queue == nil || config.Manifests == nil || config.Materializer == nil ||
		config.Parser == nil || config.Projection == nil {
		return nil, fmt.Errorf("%w: all raw parse worker dependencies are required", rawsync.ErrInvalid)
	}
	if !ValidLeaseOwner(config.Owner) || config.BatchSize <= 0 ||
		config.BatchSize > MaxClaimBatchSize {
		return nil, fmt.Errorf(
			"%w: raw parse worker identity and a batch size between 1 and %d are required",
			rawsync.ErrInvalid, MaxClaimBatchSize,
		)
	}
	if config.LeaseDuration <= 0 || config.HeartbeatInterval <= 0 ||
		config.HeartbeatInterval >= config.LeaseDuration {
		return nil, fmt.Errorf("%w: raw parse worker lease timing is invalid", rawsync.ErrInvalid)
	}
	if config.RetryBase <= 0 || config.RetryMax < config.RetryBase {
		return nil, fmt.Errorf("%w: raw parse worker retry timing is invalid", rawsync.ErrInvalid)
	}
	if config.MaxAttempts <= 0 {
		return nil, fmt.Errorf("%w: raw parse worker max attempts must be positive", rawsync.ErrInvalid)
	}
	if config.AttemptTimeout <= 0 {
		return nil, fmt.Errorf("%w: raw parse worker attempt timeout must be positive", rawsync.ErrInvalid)
	}
	return &Worker{
		Queue:             config.Queue,
		Manifests:         config.Manifests,
		Materializer:      config.Materializer,
		Parser:            config.Parser,
		Projection:        config.Projection,
		Owner:             config.Owner,
		BatchSize:         config.BatchSize,
		LeaseDuration:     config.LeaseDuration,
		HeartbeatInterval: config.HeartbeatInterval,
		AttemptTimeout:    config.AttemptTimeout,
		RetryBase:         config.RetryBase,
		RetryMax:          config.RetryMax,
		MaxAttempts:       config.MaxAttempts,
		now:               time.Now,
	}, nil
}

// RunBatch claims and processes at most BatchSize jobs concurrently.
func (w *Worker) RunBatch(ctx context.Context) (BatchResult, error) {
	if w == nil {
		return BatchResult{}, fmt.Errorf("%w: raw parse worker is missing", rawsync.ErrInvalid)
	}
	leasing, err := w.Queue.ClaimRawParseJobs(
		ctx, w.Owner, w.BatchSize, w.LeaseDuration,
	)
	if err != nil {
		return BatchResult{}, fmt.Errorf("claiming raw parse jobs: %w", err)
	}
	result := BatchResult{Claimed: len(leasing)}
	if len(leasing) == 0 {
		return result, nil
	}
	completed := make(chan leaseResult, len(leasing))
	for _, lease := range leasing {
		go func() {
			completed <- w.processLease(ctx, lease)
		}()
	}
	var batchErr error
	for range leasing {
		job := <-completed
		result.Succeeded += job.succeeded
		result.Retried += job.retried
		result.Failed += job.failed
		result.LeaseLost += job.leaseLost
		if job.err != nil {
			batchErr = errors.Join(batchErr, job.err)
		}
	}
	return result, batchErr
}

type leaseResult struct {
	id        int64
	succeeded int
	retried   int
	failed    int
	leaseLost int
	err       error
}

func (w *Worker) processLease(parent context.Context, lease JobLease) leaseResult {
	result := leaseResult{id: lease.ID}
	timeoutCtx, cancelTimeout := context.WithTimeout(parent, w.AttemptTimeout)
	workCtx, cancelWork := context.WithCancelCause(timeoutCtx)
	stopHeartbeat := w.startHeartbeat(workCtx, cancelWork, lease)
	stage, operationErr := w.runPipeline(workCtx, lease)
	heartbeatErr := stopHeartbeat()
	workCause := context.Cause(workCtx)
	cancelWork(nil)
	cancelTimeout()
	if operationErr == nil {
		// The pipeline committed its outcome; a heartbeat failure after
		// that point cannot un-commit it.
		result.succeeded = 1
		return result
	}
	if heartbeatErr != nil && errors.Is(operationErr, context.Canceled) &&
		errors.Is(workCause, heartbeatErr) {
		// The heartbeat failure canceled cooperative work mid-attempt. Prune
		// only the generic cancellation it induced so substantive failures
		// that joined the outcome -- independent parser errors, materialization
		// cleanup -- are not discarded when the heartbeat event is restored.
		stripped, _ := stripCanceledLeaves(operationErr)
		if stripped == nil {
			// Cancellation was the pipeline's only substantive cause. A
			// cooperative stage reports the work context's generic cancellation,
			// so restore the heartbeat cause and let the durable outcome record
			// the event that actually stopped the attempt, without exposing
			// transport detail.
			stage = "heartbeat"
			operationErr = heartbeatErr
			heartbeatErr = nil
		} else {
			// Substantive causes remain: the pipeline stage survives, the
			// stripped error keeps every non-cancellation cause and sentinel
			// (including errMaterializationCleanup), and the heartbeat failure
			// joins below as ErrLeaseLost or a sanitized diagnostic.
			operationErr = stripped
		}
	}
	if heartbeatErr != nil && !errors.Is(heartbeatErr, context.Canceled) {
		if errors.Is(heartbeatErr, ErrLeaseLost) {
			// Lease loss dominates the outcome classification, but the
			// pipeline failure remains the durable stage and cause.
			operationErr = errors.Join(operationErr, ErrLeaseLost)
		} else {
			// Secondary heartbeat diagnostics join only in sanitized
			// form; they never replace the pipeline failure.
			operationErr = errors.Join(
				operationErr,
				fmt.Errorf("heartbeat:%s", jobErrorCode(heartbeatErr)),
			)
		}
	}
	result.err = reportableJobError(lease.ID, stage, operationErr)
	if errors.Is(operationErr, ErrLeaseLost) {
		result.leaseLost = 1
		return result
	}
	if parent.Err() != nil {
		return result
	}
	if lease.Attempt >= w.MaxAttempts {
		failureErr := w.Queue.FailRawParseJob(
			parent, lease, stage, jobErrorDiagnostic(stage, operationErr),
		)
		if errors.Is(failureErr, ErrLeaseLost) {
			result.leaseLost = 1
			result.err = errors.Join(result.err, reportableJobError(lease.ID, stage, ErrLeaseLost))
			return result
		}
		if failureErr != nil {
			result.err = errors.Join(result.err, fmt.Errorf(
				"recording terminal failure: %s", jobErrorDiagnostic("fail", failureErr),
			))
			return result
		}
		result.failed = 1
		return result
	}
	retryAt := w.now().Add(retryDelay(lease.Attempt, w.RetryBase, w.RetryMax))
	retryErr := w.Queue.RetryRawParseJob(
		parent, lease, retryAt, stage, jobErrorDiagnostic(stage, operationErr),
	)
	if errors.Is(retryErr, ErrLeaseLost) {
		result.leaseLost = 1
		result.err = errors.Join(result.err, reportableJobError(lease.ID, stage, ErrLeaseLost))
		return result
	}
	if retryErr != nil {
		result.err = errors.Join(result.err, fmt.Errorf(
			"recording retry: %s", jobErrorDiagnostic("retry", retryErr),
		))
		return result
	}
	result.retried = 1
	return result
}

func (w *Worker) runPipeline(
	ctx context.Context,
	lease JobLease,
) (stage string, resultErr error) {
	manifest, err := w.Manifests.Load(ctx, lease)
	if err != nil {
		return "manifest", err
	}
	if manifest.ManifestID != lease.ManifestID || manifest.Identity != lease.Identity {
		return "manifest", fmt.Errorf("%w: loaded manifest does not match its lease", rawsync.ErrInvalid)
	}
	var materialized *Materialization
	if manifest.Manifest.Kind != rawsync.ManifestTombstone {
		materialized, err = w.Materializer.Materialize(ctx, manifest)
		if err != nil {
			return "materialize", err
		}
	}
	var cleanupReported bool
	if materialized != nil {
		defer func() {
			if cleanupErr := materialized.Cleanup(); cleanupErr != nil && !cleanupReported {
				// The deferred cleanup is one retry of the pre-projection
				// removal. Cleanup failures always join the operation error,
				// and the durable stage only becomes the cleanup stage when
				// nothing else failed first. When the pre-projection removal
				// already reported its failure, the retry outcome must not
				// duplicate it in the joined error.
				if resultErr == nil {
					stage = "materialize"
				}
				resultErr = errors.Join(resultErr, cleanupErr)
			}
		}()
	}
	parsed, err := w.Parser.Parse(ctx, manifest, materialized)
	if err != nil {
		return "parse", err
	}
	if materialized != nil {
		if err := materialized.Cleanup(); err != nil {
			cleanupReported = true
			return "materialize", fmt.Errorf("cleaning raw materialization: %w", err)
		}
	}
	if err := w.Projection.Project(ctx, lease, manifest, parsed); err != nil {
		return "projection", err
	}
	return "", nil
}

// startHeartbeat renews the leased job on a fixed cadence until the
// attempt's work context is done. The heartbeat context must derive from
// the work context, never from the longer-lived parent: once the
// AttemptTimeout deadline fires, renewal stops even when a non-cooperative
// pipeline stage never returns, so the durable lease is left to expire.
func (w *Worker) startHeartbeat(
	workCtx context.Context,
	cancelWork context.CancelCauseFunc,
	lease JobLease,
) func() error {
	heartbeatCtx, cancelHeartbeat := context.WithCancel(workCtx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(w.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if heartbeatCtx.Err() != nil {
					// The attempt deadline (or cancellation) already fired;
					// a tick that lost the select race must not renew past
					// the attempt's lifetime.
					done <- nil
					return
				}
				if err := w.Queue.HeartbeatRawParseJob(
					heartbeatCtx, lease, w.LeaseDuration,
				); err != nil {
					if heartbeatCtx.Err() != nil {
						done <- nil
						return
					}
					cancelWork(err)
					done <- err
					return
				}
			}
		}
	}()
	return func() error {
		cancelHeartbeat()
		return <-done
	}
}

// stripCanceledLeaves prunes context.Canceled leaves from an operation
// error while preserving every substantive non-cancellation cause and
// sentinel. Ordinary unwrap chains collapse only when cancellation is their
// entire content; errors.Join multi-error trees drop just their canceled
// branches and keep the rest. The second result reports whether any
// cancellation leaf was removed.
func stripCanceledLeaves(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if err == context.Canceled {
		return nil, true
	}
	if tree, ok := err.(interface{ Unwrap() []error }); ok {
		branches := tree.Unwrap()
		kept := make([]error, 0, len(branches))
		removed := false
		for _, branch := range branches {
			stripped, branchRemoved := stripCanceledLeaves(branch)
			removed = removed || branchRemoved
			if stripped != nil {
				kept = append(kept, stripped)
			}
		}
		switch {
		case !removed:
			// Rebuilding an untouched tree would needlessly replace the
			// original error and its formatting.
			return err, false
		case len(kept) == 0:
			return nil, true
		case len(kept) == 1:
			return kept[0], true
		default:
			return errors.Join(kept...), true
		}
	}
	if chain, ok := err.(interface{ Unwrap() error }); ok {
		stripped, removed := stripCanceledLeaves(chain.Unwrap())
		if removed {
			// Wrapper text is diagnostic context, not an independent cause. Once
			// its child changes, return the stripped child rather than retaining
			// the original wrapper and its canceled branch.
			return stripped, true
		}
	}
	return err, false
}

func retryDelay(attempt int, base, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for range attempt - 1 {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
