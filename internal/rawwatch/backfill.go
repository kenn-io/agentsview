package rawwatch

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawupload"
)

type BackfillOptions struct {
	Spec      rawcheckpoint.BackfillRunSpec
	Providers []parser.Provider
	BatchSize int
}

// RunBackfill performs one finite attempt. It never waits for retry deadlines:
// incomplete discovery, blocked work or a transport failure returns progress
// and a non-nil sanitized error. A completed run does no discovery or capture.
// Providers must be constructed from precisely Spec's configured roots. Watch
// plans describe scheduling coverage (including sidecars), not that selection.
func RunBackfill(ctx context.Context, store *rawcheckpoint.Store, capturer *rawcapture.Capturer, uploader *rawupload.Uploader, options BackfillOptions) (progress rawcheckpoint.BackfillProgress, resultErr error) {
	if options.BatchSize < 1 || options.BatchSize > 512 || store == nil || capturer == nil || uploader == nil {
		return progress, rawcheckpoint.ErrBackfillConflict
	}
	types := make([]parser.AgentType, 0, len(options.Providers))
	providers := make(map[parser.AgentType]parser.Provider)
	for _, provider := range options.Providers {
		if provider == nil {
			return progress, rawcheckpoint.ErrBackfillConflict
		}
		typ := provider.Definition().Type
		if _, exists := providers[typ]; exists {
			continue
		}
		types = append(types, typ)
		providers[typ] = provider
	}
	selected := slices.Clone(options.Spec.Providers)
	slices.Sort(selected)
	selected = slices.Compact(selected)
	slices.Sort(types)
	if !slices.Equal(types, selected) {
		return progress, rawcheckpoint.ErrBackfillConflict
	}
	progress, err := store.BeginBackfill(ctx, options.Spec)
	if err != nil {
		return progress, err
	}
	if progress.Complete {
		return progress, nil
	}
	runID := options.Spec.RunID
	failure := ""
	// Cancellation must still leave a durable aggregate outcome; cleanup uses a
	// context detached from the cancelled invocation, with a bounded DB timeout.
	defer func() {
		progress, resultErr = finalizeBackfillAttempt(ctx, store, runID, failure, progress, resultErr)
	}()
	if err := store.RecordBackfillFailure(ctx, runID, ""); err != nil {
		return progress, rawcheckpoint.ErrBackfillIncomplete
	}
	auditor := NewBackfillAuditor(store, capturer, options.BatchSize, runID)
	defer auditor.Close()
	drain := func() (bool, error) {
		for range options.BatchSize {
			_, found, err := uploader.UploadNextForBackfill(ctx, runID)
			if err != nil {
				failure = "upload"
				return false, rawcheckpoint.ErrBackfillIncomplete
			}
			if !found {
				return true, nil
			}
		}
		return false, nil
	}
	for _, typ := range types {
		done, err := store.BackfillProviderComplete(ctx, runID, typ)
		if err != nil {
			return progress, rawcheckpoint.ErrBackfillIncomplete
		}
		if done {
			continue
		}
		provider, ok := providers[typ]
		if !ok || provider == nil {
			return progress, rawcheckpoint.ErrBackfillConflict
		}
		pass := rawcheckpoint.BackfillPassResult{}
		if provider.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			pass.ErrorClass = "unsupported"
		}
		if _, ok := provider.(parser.StreamingRawCaptureSourceProvider); !ok {
			pass.ErrorClass = "unsupported"
		}
		roots, rootsErr := store.BackfillRoots(ctx, runID, typ)
		if rootsErr != nil || len(roots) == 0 {
			pass.ErrorClass = "root_unavailable"
		}
		if pass.ErrorClass != "" {
			_ = store.FinishBackfillProvider(ctx, runID, typ, pass)
			failure = pass.ErrorClass
			return progress, rawcheckpoint.ErrBackfillIncomplete
		}
		for {
			exhausted, err := drain()
			if err != nil {
				return progress, err
			}
			if exhausted {
				current, err := store.BackfillProgress(ctx, runID)
				if err != nil {
					return progress, rawcheckpoint.ErrBackfillIncomplete
				}
				// Pending includes invalidated bindings whose queue rows were
				// removed. Neither missing nor deferred work permits a new scan.
				if current.Pending > 0 {
					failure = "deferred"
					return current, rawcheckpoint.ErrBackfillIncomplete
				}
			}
			batch, err := auditor.AuditProvider(ctx, provider)
			pass.Changed += int64(batch.Changed)
			pass.Unsupported += int64(batch.Unsupported)
			pass.Degraded += int64(batch.Degraded)
			if err != nil || batch.PassFinished || pass.Changed+pass.Unsupported+pass.Degraded > 0 {
				pass.Complete = batch.PassFinished && batch.Complete && err == nil
				if err != nil {
					pass.ErrorClass = backfillCaptureFailure(err)
				}
				if err := store.FinishBackfillProvider(ctx, runID, typ, pass); err != nil {
					return progress, rawcheckpoint.ErrBackfillIncomplete
				}
				if !pass.Complete || pass.Changed+pass.Unsupported+pass.Degraded > 0 {
					failure = "discovery_incomplete"
					return progress, rawcheckpoint.ErrBackfillIncomplete
				}
				break
			}
		}
	}
	if _, err := store.SealBackfill(ctx, runID); err != nil {
		return progress, rawcheckpoint.ErrBackfillIncomplete
	}
	for {
		empty, err := drain()
		if err != nil {
			return progress, err
		}
		if !empty {
			continue
		}
		progress, err = store.CompleteBackfill(ctx, runID)
		if err != nil {
			failure = "deferred"
			return progress, rawcheckpoint.ErrBackfillIncomplete
		}
		return progress, nil
	}
}

func finalizeBackfillAttempt(ctx context.Context, store *rawcheckpoint.Store, runID, failure string, progress rawcheckpoint.BackfillProgress, resultErr error) (rawcheckpoint.BackfillProgress, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cancelled := ctx.Err() != nil
	if cancelled {
		failure = "cancelled"
	}
	if resultErr != nil && failure == "" {
		failure = "capture"
	}
	if err := store.RecordBackfillFailure(cleanup, runID, failure); err != nil {
		resultErr = rawcheckpoint.ErrBackfillIncomplete
	}
	if current, err := store.BackfillProgress(cleanup, runID); err == nil {
		progress = current
	} else {
		resultErr = rawcheckpoint.ErrBackfillIncomplete
	}
	if cancelled && !progress.Complete {
		resultErr = rawcheckpoint.ErrBackfillIncomplete
	}
	return progress, resultErr
}

func backfillCaptureFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, rawcapture.ErrSourceChanged):
		return "source_changed"
	case errors.Is(err, rawcapture.ErrUnsupportedProvider), errors.Is(err, rawcapture.ErrUnsupportedSnapshot):
		return "unsupported"
	case errors.Is(err, rawcheckpoint.ErrOutboxFull):
		return "capacity"
	}
	return "capture"
}

// NewBackfillAuditor shares bounded streaming discovery with watch auditing,
// skips immutable bindings and disables watch-only tombstone reconciliation.
func NewBackfillAuditor(store *rawcheckpoint.Store, capturer *rawcapture.Capturer, maxWork int, runID string) *Auditor {
	a := NewAuditor(store, capturer, maxWork)
	a.runID = runID
	return a
}
