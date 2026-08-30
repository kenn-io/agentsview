package rawwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawupload"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// ErrFullSyncIncomplete keeps a full-sync marker retryable when outbox
// backpressure prevents the current pass from reconciling every source.
var ErrFullSyncIncomplete = errors.New("rawwatch: full audit incomplete")

type generationUploader interface {
	UploadNext(context.Context) (rawupload.Result, bool, error)
}

// Worker serializes watcher capture, audit, and upload work so hosted raw mode
// has one bounded laptop-side work stream.
type Worker struct {
	mu        sync.Mutex
	providers []parser.Provider
	capturer  *rawcapture.Capturer
	auditor   *Auditor
	uploader  generationUploader
}

type fullAuditStatus struct {
	degraded int
	complete bool
}

// NewWorker constructs one serialized raw-capture worker.
func NewWorker(
	providers []parser.Provider,
	capturer *rawcapture.Capturer,
	auditor *Auditor,
	uploader generationUploader,
) *Worker {
	return &Worker{
		providers: append([]parser.Provider(nil), providers...),
		capturer:  capturer, auditor: auditor, uploader: uploader,
	}
}

// HandleBatch captures physical sources named by one watcher batch, promotes
// uncertain or removal-shaped batches to bounded audit, then drains uploads.
func (w *Worker) HandleBatch(
	ctx context.Context,
	batch syncpkg.WatchBatch,
) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if batch.FullSync {
		status, err := w.auditAllFullLocked(ctx)
		if err != nil {
			return err
		}
		if err := w.drainLocked(ctx); err != nil {
			return err
		}
		if status.degraded != 0 || !status.complete {
			return fmt.Errorf(
				"%w: degraded=%d discovery_complete=%t",
				ErrFullSyncIncomplete, status.degraded, status.complete,
			)
		}
		return nil
	}
	if len(batch.ReconcileRoots) != 0 || len(batch.Renames) != 0 {
		if err := w.auditAllLocked(ctx); err != nil {
			return err
		}
		return w.drainLocked(ctx)
	}
	for _, provider := range w.providers {
		if provider.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			continue
		}
		plan, err := provider.WatchPlan(ctx)
		if err != nil {
			return err
		}
		needsAudit := false
		seen := make(map[string]struct{})
		for _, path := range batch.Paths {
			for _, root := range plan.Roots {
				if !rawWatchPathWithin(root.Path, path) {
					continue
				}
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					needsAudit = true
					break
				} else if err != nil {
					return fmt.Errorf("rawwatch: inspect changed source: %w", err)
				}
				sources, err := parser.RawCaptureSourcesForChangedPath(
					ctx, provider, parser.ChangedPathRequest{
						Path: path, EventKind: "change", WatchRoot: root.Path,
					},
				)
				if err != nil {
					return err
				}
				if len(sources) == 0 {
					needsAudit = true
				}
				for _, source := range sources {
					key := rawWatchSourceDedupKey(source)
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					if _, err := w.capturer.Capture(ctx, provider, source); err != nil {
						if errors.Is(err, rawcapture.ErrSourceChanged) ||
							rawWatchPathMissing(path) {
							needsAudit = true
							continue
						}
						return fmt.Errorf("rawwatch: capture changed source: %w", err)
					}
				}
			}
		}
		if needsAudit {
			if _, err := w.auditor.AuditProvider(ctx, provider); err != nil {
				return err
			}
		}
	}
	return w.drainLocked(ctx)
}

func rawWatchSourceDedupKey(source parser.SourceRef) string {
	physical := source.FingerprintKey
	if physical == "" {
		physical = source.DisplayPath
	}
	return string(source.Provider) + "\x00" + source.Key + "\x00" + physical
}

func rawWatchPathMissing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

// AuditAll runs one bounded repair pass per supported provider and drains any
// newly ready generations.
func (w *Worker) AuditAll(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.auditAllLocked(ctx); err != nil {
		return err
	}
	return w.drainLocked(ctx)
}

// Drain uploads all currently ready generations until the durable retry fence
// or an empty outbox stops work.
func (w *Worker) Drain(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.drainLocked(ctx)
}

func (w *Worker) auditAllLocked(ctx context.Context) error {
	for _, provider := range w.providers {
		if provider.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			continue
		}
		if _, err := w.auditor.AuditProvider(ctx, provider); err != nil {
			return fmt.Errorf("rawwatch: audit %s: %w", provider.Definition().Type, err)
		}
	}
	return nil
}

func (w *Worker) auditAllFullLocked(ctx context.Context) (fullAuditStatus, error) {
	status := fullAuditStatus{complete: true}
	for _, provider := range w.providers {
		if provider.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			continue
		}
		result, err := w.auditor.AuditProviderFull(ctx, provider)
		if err != nil {
			return status, fmt.Errorf(
				"rawwatch: full audit %s: %w", provider.Definition().Type, err,
			)
		}
		status.degraded += result.Degraded
		status.complete = status.complete && result.Complete
	}
	return status, nil
}

func (w *Worker) drainLocked(ctx context.Context) error {
	if w.uploader == nil {
		return nil
	}
	for {
		_, found, err := w.uploader.UploadNext(ctx)
		if err != nil {
			if errors.Is(err, rawupload.ErrPermanentFailure) {
				continue
			}
			return fmt.Errorf("rawwatch: upload generation: %w", err)
		}
		if !found {
			return nil
		}
	}
}

func rawWatchPathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
