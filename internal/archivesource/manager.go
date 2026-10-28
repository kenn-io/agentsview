package archivesource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/stringutil"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/docbank/docbank"
)

type drainRun struct {
	done      chan struct{}
	err       error
	stats     syncer.SyncStats
	scheduled bool
}

// Scheduled drains wait this long before repeating a copy or parse that failed
// twice in a row.
const (
	sourceRetryInitial = time.Hour
	sourceRetryMax     = 24 * time.Hour
)

// retryKey names a failed archive pass, or one source within it.
type retryKey struct {
	archive config.ArchiveSource
	source  db.ArchiveSourceProvenance
}

type retryState struct {
	target, message string
	delay           time.Duration
	next            time.Time
}

// DrainError reports unfinished attachment ingestion. Committed SQLite history
// remains available to push, and later drains retry unfinished sources.
type DrainError struct{ Err error }

func (e DrainError) Error() string { return e.Err.Error() }
func (e DrainError) Unwrap() error { return e.Err }

// Manager owns one drain for a receiving database. A caller's cancellation ends
// its wait, while the owner's context controls the shared work.
type Manager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          config.Config
	database     *db.DB
	engine       *syncer.Engine
	beforeSource func(context.Context) error
	now          func() time.Time
	mu           sync.Mutex
	active       *drainRun
	// retries is owned by the single active drain.
	retries map[retryKey]retryState
}

func NewManager(ownerCtx context.Context, cfg config.Config, database *db.DB, engine *syncer.Engine, beforeSource func(context.Context) error) (*Manager, error) {
	if err := cfg.ValidateArchiveSources(); err != nil {
		return nil, err
	}
	if len(cfg.ArchiveSources) > 0 {
		only, err := database.IsArchiveOnly(ownerCtx)
		if err != nil {
			return nil, err
		}
		if only {
			return nil, db.ErrArchiveOnly
		}
	}
	ctx, cancel := context.WithCancel(ownerCtx)
	return &Manager{
		ctx: ctx, cancel: cancel, cfg: cfg, database: database, engine: engine, beforeSource: beforeSource,
		now: time.Now, retries: make(map[retryKey]retryState),
	}, nil
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	run := m.active
	m.mu.Unlock()
	if run != nil {
		<-run.done
	}
}

// Drain reads pending attachments now, including sources that failed recently.
func (m *Manager) Drain(ctx context.Context, progress syncer.ProgressFunc) (syncer.SyncStats, error) {
	return m.start(ctx, progress, false)
}

// DrainScheduled serves automatic passes. A source or archive that fails again
// on its retry waits an hour, doubling to a day, so it is not copied and parsed
// on every pass.
func (m *Manager) DrainScheduled(ctx context.Context) (syncer.SyncStats, error) {
	return m.start(ctx, nil, true)
}

func (m *Manager) start(ctx context.Context, progress syncer.ProgressFunc, scheduled bool) (syncer.SyncStats, error) {
	if err := ctx.Err(); err != nil {
		return syncer.SyncStats{}, err
	}
	if len(m.cfg.ArchiveSources) == 0 {
		return syncer.SyncStats{}, nil
	}
	m.mu.Lock()
	run := m.active
	// A manual caller must not inherit a scheduled run's backoff. Let that run
	// finish, then start a manual one that retries deferred sources.
	for run != nil && run.scheduled && !scheduled {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return syncer.SyncStats{}, ctx.Err()
		case <-run.done:
		}
		m.mu.Lock()
		run = m.active
	}
	if run == nil {
		report := progress
		if progress != nil {
			var callbacks sync.Mutex
			report = func(p syncer.Progress) {
				callbacks.Lock()
				defer callbacks.Unlock()
				if progress != nil {
					progress(p)
				}
			}
			// A request may release its writer as soon as Drain returns.
			defer func() {
				callbacks.Lock()
				progress = nil
				callbacks.Unlock()
			}()
		}
		run = &drainRun{done: make(chan struct{}), scheduled: scheduled}
		m.active = run
		go func() {
			stats, err := m.drain(report, scheduled)
			m.mu.Lock()
			run.err = err
			run.stats = stats
			m.active = nil
			close(run.done)
			m.mu.Unlock()
		}()
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return syncer.SyncStats{}, ctx.Err()
	case <-run.done:
		if run.err != nil {
			return run.stats, DrainError{Err: run.err}
		}
		return run.stats, run.err
	}
}

func (m *Manager) drain(progress syncer.ProgressFunc, scheduled bool) (syncer.SyncStats, error) {
	var total syncer.SyncStats
	var result error
	for _, source := range m.cfg.ArchiveSources {
		if err := m.ctx.Err(); err != nil {
			return total, errors.Join(result, err)
		}
		stats, err := m.drainAttachment(source, progress, scheduled)
		total.Merge(stats)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("archive source %q incomplete; later syncs retry it: %w", source.Name, err))
		}
	}
	return total, result
}

func (m *Manager) drainAttachment(cfg config.ArchiveSource, progress syncer.ProgressFunc, scheduled bool) (total syncer.SyncStats, retErr error) {
	attachment := retryKey{archive: cfg}
	if err := m.deferred(attachment, "", scheduled, ""); err != nil {
		return total, err
	}
	index, err := m.openIndex(cfg, progress)
	if index != nil {
		defer func() { retErr = errors.Join(retErr, index.Close()) }()
	}
	if err != nil {
		m.recordFailure(attachment, "", err)
		return total, err
	}
	delete(m.retries, attachment)
	key := db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: index.Info.SnapshotID}
	status, err := m.database.ArchiveAttachmentStatus(m.ctx, key)
	if err != nil || status.Pending == 0 {
		return total, err
	}

	coverage, err := index.Coverage(m.ctx)
	if err != nil {
		return total, err
	}
	if progress != nil {
		progress(syncer.Progress{Phase: syncer.PhaseDiscovering, Detail: fmt.Sprintf("Archive %q: %d Claude/Codex sources (%d pending), %d deletion rules, %d supplemental original files", cfg.Name, coverage.SupportedSources, status.Pending, coverage.DeletionRules, coverage.SupplementalFiles)})
	}
	scratch := filepath.Join(m.cfg.DataDir, "archive-sources", "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return total, err
	}
	var after db.ArchiveSourceIdentity
	for {
		pending, err := m.database.PendingArchiveSources(m.ctx, key, after, 128)
		if err != nil {
			return total, errors.Join(retErr, err)
		}
		if len(pending) == 0 {
			return total, retErr
		}
		for _, item := range pending {
			if err := m.ctx.Err(); err != nil {
				return total, errors.Join(retErr, err)
			}
			after = item.Provenance.Source
			key := retryKey{archive: cfg, source: item.Provenance}
			if err := m.deferred(key, item.Target, scheduled, item.LastError); err != nil {
				retErr = errors.Join(retErr, err)
				continue
			}
			if m.beforeSource != nil {
				if err := m.beforeSource(m.ctx); err != nil {
					return total, errors.Join(retErr, err)
				}
			}
			source, err := index.source(m.ctx, item.Provenance.ManifestID)
			if err == nil {
				var stats syncer.SyncStats
				stats, err = index.syncSource(m.ctx, cfg.Repository, source, m.cfg, m.engine, scratch, item.Target, progress)
				total.Merge(stats)
			}
			if err != nil {
				m.recordFailure(key, item.Target, err)
				message := stringutil.SafeTruncate(err.Error(), maxSourceErrorBytes)
				recordErr := m.engine.RunExclusive(func() error { return m.database.RecordArchiveSourceError(m.ctx, item.Provenance, message) })
				retErr = errors.Join(retErr, err, recordErr)
				if errors.Is(err, docbank.ErrBackupRepositoryLocked) {
					return total, retErr
				}
				continue
			}
			delete(m.retries, key)
		}
	}
}

// maxSourceErrorBytes matches the limit RecordArchiveSourceError accepts.
const maxSourceErrorBytes = 64 << 10

// openIndex registers the attachment under the sync lock. A backup bootstrap
// copies the saved database first, outside that lock. A directory index holds
// the source's writer lock, which must not wait for the sync lock: a rebuild
// holding the sync lock can be waiting for that directory.
func (m *Manager) openIndex(cfg config.ArchiveSource, progress syncer.ProgressFunc) (*Index, error) {
	open := func() (*Index, error) {
		return OpenIndex(m.ctx, cfg, filepath.Join(m.cfg.DataDir, "archive-sources"), indexProgress(progress))
	}
	if cfg.Directory == "" {
		index, err := open()
		if err != nil {
			return nil, err
		}
		return index, m.engine.RunExclusive(func() error { return index.register(m.ctx, m.database, m.cfg) })
	}
	var index *Index
	err := m.engine.RunExclusive(func() error {
		var err error
		if index, err = open(); err != nil {
			return err
		}
		return index.register(m.ctx, m.database, m.cfg)
	})
	return index, err
}

// deferred reports a scheduled pass that skips work still inside its backoff.
// A changed completion target, such as a parser upgrade, retries immediately.
func (m *Manager) deferred(key retryKey, target string, scheduled bool, detail string) error {
	state, ok := m.retries[key]
	if !scheduled || !ok || state.target != target || !m.now().Before(state.next) {
		return nil
	}
	if detail == "" {
		detail = state.message
	}
	return fmt.Errorf("retrying after %s: %s", state.next.Format(time.RFC3339), detail)
}

// recordFailure backs off failures that would repeat the same work. Missing
// mounts, busy repositories and cancellation retry on the next pass.
func (m *Manager) recordFailure(key retryKey, target string, err error) {
	if m.ctx.Err() != nil || errors.Is(err, errSourceUnavailable) || archiveBusy(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// A first failure retries on the next pass, since transient errors are
	// common. Only repeated failures wait.
	var delay time.Duration
	if state, ok := m.retries[key]; ok && state.target == target {
		delay = max(sourceRetryInitial, min(2*state.delay, sourceRetryMax))
	}
	m.retries[key] = retryState{target: target, message: err.Error(), delay: delay, next: m.now().Add(delay)}
}

func indexProgress(progress syncer.ProgressFunc) func(string) {
	if progress == nil {
		return nil
	}
	return func(detail string) { progress(syncer.Progress{Phase: syncer.PhaseDiscovering, Detail: detail}) }
}
