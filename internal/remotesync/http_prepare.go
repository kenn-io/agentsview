package remotesync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"

	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

var (
	ErrDuplicateMirrorLock = errors.New("duplicate mirror lock identity")
	ErrPreparedInUse       = errors.New("prepared HTTP sources are in use")
	ErrPreparedClosed      = errors.New("prepared HTTP sources are closing or closed")
)

// HostError attributes an HTTP preparation failure to its configured host.
// The cause remains available to errors.Is and errors.As callers.
type HostError struct {
	Host      string
	Operation string
	Err       error
}

func (e *HostError) Error() string {
	if e.Operation == "" {
		return fmt.Sprintf("HTTP host %q: %v", e.Host, e.Err)
	}
	return fmt.Sprintf("HTTP host %q %s: %v", e.Host, e.Operation, e.Err)
}

func (e *HostError) Unwrap() error { return e.Err }

// PreparedHTTPSyncs owns a deterministically prepared group of HTTP sources.
// Source ownership stays internal so locks and temporary roots can only be
// released as a group.
type PreparedHTTPSyncs struct {
	mu      sync.Mutex
	sources []*PreparedHTTP
	closing bool
	borrows int
}

// PrepareHTTPSyncs prepares HTTP hosts in deterministic order, using canonical
// mirror-lock paths for persistent sources and host/URL keys for legacy
// temporary sources. It copies the input so callers retain their original
// ordering. When both the preparation and its unwind fail, the returned set is
// nonnil and callers must retry Close even though err is also nonnil.
func PrepareHTTPSyncs(
	ctx context.Context, syncs []HTTPSync,
) (*PreparedHTTPSyncs, error) {
	return prepareHTTPSyncs(ctx, syncs, func(
		ctx context.Context, hs HTTPSync,
	) (*PreparedHTTP, error) {
		return hs.Prepare(ctx)
	})
}

func prepareHTTPSyncs(
	ctx context.Context,
	syncs []HTTPSync,
	prepare func(context.Context, HTTPSync) (*PreparedHTTP, error),
) (*PreparedHTTPSyncs, error) {
	type orderedSync struct {
		sync    HTTPSync
		sortKey string
	}
	ordered := make([]orderedSync, 0, len(syncs))
	seenLocks := make(map[string]HTTPSync, len(syncs))
	for _, hs := range syncs {
		if hs.DataDir == "" {
			// Legacy sources use isolated temporary roots and never acquire a
			// mirror lock. Give them a stable ordering without resolving a
			// nonexistent lock path (which would create cwd artifacts).
			ordered = append(ordered, orderedSync{
				sync: hs, sortKey: "legacy\x00" + hs.Host + "\x00" + hs.URL,
			})
			continue
		}
		lockPath, err := canonicalMirrorLockPath(MirrorDir(hs.DataDir, hs.Host))
		if err != nil {
			return nil, &HostError{
				Host: hs.Host, Operation: "resolve mirror lock", Err: err,
			}
		}
		if previous, exists := seenLocks[lockPath]; exists {
			cause := fmt.Errorf(
				"%w: hosts %q and %q use %q",
				ErrDuplicateMirrorLock, previous.Host, hs.Host, lockPath,
			)
			return nil, &HostError{
				Host: hs.Host, Operation: "resolve mirror lock", Err: cause,
			}
		}
		seenLocks[lockPath] = hs
		ordered = append(ordered, orderedSync{
			sync: hs, sortKey: "mirror\x00" + lockPath,
		})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].sortKey < ordered[j].sortKey
	})

	prepared := &PreparedHTTPSyncs{
		sources: make([]*PreparedHTTP, 0, len(ordered)),
	}
	for _, input := range ordered {
		hs := input.sync
		source, err := prepare(ctx, hs)
		if err != nil {
			if source != nil {
				prepared.sources = append(prepared.sources, source)
			}
			primary := &HostError{Host: hs.Host, Operation: "prepare", Err: err}
			cleanupErr := prepared.Close()
			if cleanupErr != nil {
				return prepared, errors.Join(primary, cleanupErr)
			}
			return nil, primary
		}
		prepared.sources = append(prepared.sources, source)
	}
	return prepared, nil
}

// BorrowRebuildContributors borrows rebuild descriptions while the prepared
// set retains cleanup ownership. Callers must invoke the idempotent release
// function after the rebuild stops using every contributor. Close refuses to
// release any source while a borrow remains active.
func (p *PreparedHTTPSyncs) BorrowRebuildContributors() (
	contributors []syncpkg.RebuildContributor,
	release func(),
	err error,
) {
	if p == nil {
		return nil, nil, ErrPreparedClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return nil, nil, ErrPreparedClosed
	}
	contributors = make([]syncpkg.RebuildContributor, 0, len(p.sources))
	for _, source := range p.sources {
		if source == nil {
			continue
		}
		contributor, err := source.RebuildContributor()
		if err != nil {
			return nil, nil, &HostError{
				Host: source.sync.Host, Operation: "build rebuild contributor", Err: err,
			}
		}
		contributors = append(contributors, contributor)
	}
	p.borrows++
	var once sync.Once
	release = func() {
		once.Do(func() {
			p.mu.Lock()
			p.borrows--
			p.mu.Unlock()
		})
	}
	return contributors, release, nil
}

// Close releases prepared sources in reverse order. Successfully closed
// sources are forgotten; failed sources remain owned so cleanup can be retried.
func (p *PreparedHTTPSyncs) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closing = true
	if p.borrows > 0 {
		return ErrPreparedInUse
	}
	var cleanupErr error
	for i := range slices.Backward(p.sources) {
		source := p.sources[i]
		if source == nil {
			continue
		}
		if err := source.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, &HostError{
				Host: source.sync.Host, Operation: "cleanup prepared source", Err: err,
			})
			continue
		}
		p.sources[i] = nil
	}
	return cleanupErr
}

// PreparedHTTP is a downloaded remote source ready for active import. A
// persistent mirror remains locked until Close so another process cannot mutate
// files while the importer reads them. Legacy sources own their temporary root.
type PreparedHTTP struct {
	sync        HTTPSync
	targets     TargetSet
	root        string
	lock        *MirrorLockHandle
	cleanupRoot bool
	closing     bool
	closed      bool
	removeRoot  func(string) error
	releaseLock func(*MirrorLockHandle) error
}

// Prepare resolves the remote targets and prepares either the persistent
// manifest mirror or an isolated legacy archive without importing sessions.
// When preparation and automatic cleanup both fail, it returns a nonnil source
// with the error; callers must retry Close on every nonnil result. If cleanup
// succeeds, preparation failures retain the ordinary (nil, err) shape.
func (hs HTTPSync) Prepare(ctx context.Context) (*PreparedHTTP, error) {
	return hs.prepare(ctx, nil)
}

func (hs HTTPSync) prepare(
	ctx context.Context, configure func(*PreparedHTTP),
) (result *PreparedHTTP, err error) {
	client := hs.Client
	if client == nil {
		client = http.DefaultClient
	}
	hs.reportProgressResolvingTargets()
	targets, err := hs.fetchTargets(ctx, client)
	if err != nil {
		return nil, err
	}
	if err := validateTargetSetPaths(targets); err != nil {
		return nil, err
	}

	prepared := &PreparedHTTP{
		sync:        hs,
		targets:     targets,
		removeRoot:  os.RemoveAll,
		releaseLock: func(lock *MirrorLockHandle) error { return lock.Close() },
	}
	if configure != nil {
		configure(prepared)
	}
	defer func() {
		if err != nil {
			if cleanupErr := prepared.Close(); cleanupErr != nil {
				result = prepared
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	if hs.DataDir == "" {
		prepared.root, err = hs.downloadAndExtract(ctx, client, targets)
		if err != nil {
			return nil, err
		}
		prepared.cleanupRoot = true
		return prepared, nil
	}

	mirrorRoot := MirrorDir(hs.DataDir, hs.Host)
	prepared.lock, err = AcquireMirrorLock(ctx, mirrorRoot)
	if err != nil {
		return nil, err
	}
	dirScoped, fileScoped := targets.SplitFileScoped()
	hs.reportProgressDetail(fmt.Sprintf(
		"Fetching session manifest from %s", hs.Host,
	))
	manifest, supported, err := hs.fetchManifest(ctx, client, dirScoped)
	if err != nil {
		return nil, err
	}
	if !supported {
		hs.reportLegacyFallback()
		prepared.root, err = hs.downloadAndExtract(ctx, client, targets)
		if err != nil {
			return nil, err
		}
		prepared.cleanupRoot = true
		return prepared, nil
	}

	prepared.root = mirrorRoot
	err = hs.prepareMirror(ctx, client, splitTargets{
		dirScoped:  dirScoped,
		fileScoped: fileScoped,
	}, manifest, mirrorRoot)
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

// Root returns the prepared source root.
func (p *PreparedHTTP) Root() string { return p.root }

// Targets returns the target set represented by Root.
func (p *PreparedHTTP) Targets() TargetSet { return p.targets }

// ImportActive imports the prepared source into the active database.
func (p *PreparedHTTP) ImportActive(ctx context.Context) (SyncStats, error) {
	if p == nil || p.closing || p.closed {
		return SyncStats{}, fmt.Errorf("prepared HTTP source is closed")
	}
	return p.sync.importRoot(ctx, p.targets, p.root)
}

// RebuildContributor converts this prepared source into an atomic rebuild
// contributor. PreparedHTTP retains ownership of its root and mirror lock;
// callers must Close it after the rebuild finishes.
func (p *PreparedHTTP) RebuildContributor() (syncpkg.RebuildContributor, error) {
	if p == nil || p.closing || p.closed {
		return syncpkg.RebuildContributor{}, fmt.Errorf(
			"prepared HTTP source is closed",
		)
	}
	layout, config, err := newImportInputs(
		p.sync.Host, p.sync.BlockedResultCategories, p.targets, p.root,
	)
	if err != nil {
		return syncpkg.RebuildContributor{}, err
	}
	return syncpkg.RebuildContributor{
		Name:   p.sync.Host,
		Config: config,
		Progress: func(progress syncpkg.Progress) syncpkg.Progress {
			return transformHostProgress(p.sync.Host, progress)
		},
		AfterSync: func(engine *syncpkg.Engine, database *db.DB) error {
			return saveEngineSkipCache(database, engine, layout.paths)
		},
	}, nil
}

// Close releases the mirror lock and removes an owned legacy root. It is safe
// to call more than once.
func (p *PreparedHTTP) Close() error {
	if p == nil || p.closed {
		return nil
	}
	p.closing = true
	var cleanupErr, lockErr error
	if p.cleanupRoot && p.root != "" {
		if err := p.removeRoot(p.root); err != nil {
			cleanupErr = fmt.Errorf("remove prepared HTTP root: %w", err)
		} else {
			p.cleanupRoot = false
		}
	}
	if p.lock != nil {
		if err := p.releaseLock(p.lock); err != nil {
			lockErr = err
		} else {
			p.lock = nil
		}
	}
	p.closed = !p.cleanupRoot && p.lock == nil
	return errors.Join(cleanupErr, lockErr)
}

func (hs HTTPSync) reportProgressResolvingTargets() {
	hs.reportProgressDetail(fmt.Sprintf("Resolving agent directories on %s", hs.Host))
}

func (hs HTTPSync) reportLegacyFallback() {
	hs.reportProgressDetail(fmt.Sprintf(
		"Remote %s does not support incremental sync; downloading full archive",
		hs.Host,
	))
}

// splitTargets carries a target set alongside its SplitFileScoped partition so
// mirror preparation addresses each half without re-deriving it.
type splitTargets struct {
	dirScoped  TargetSet
	fileScoped TargetSet
}

// prepareMirror applies the manifest delta to the persistent mirror. The
// caller holds the mirror lock from before the manifest fetch until the
// prepared source is closed.
func (hs HTTPSync) prepareMirror(
	ctx context.Context,
	client *http.Client,
	targets splitTargets,
	manifest Manifest,
	mirrorRoot string,
) error {
	delta, err := MirrorDiff(mirrorRoot, manifest)
	if err != nil {
		return err
	}
	hs.reportProgressDetail(fmt.Sprintf(
		"Compared session manifest from %s: %d total, %d changed, %d deleted",
		hs.Host, delta.Total, len(delta.Fetch), len(delta.Deletions),
	))
	mutatesMirror := len(delta.Deletions) > 0 || len(delta.Fetch) > 0 ||
		!targets.fileScoped.IsEmpty()
	if !mutatesMirror {
		return nil
	}
	if err := hs.DB.ClearRemoteSkippedFiles(hs.Host); err != nil {
		return fmt.Errorf("clear remote skip cache before mirror mutation: %w", err)
	}

	// Deletions precede extraction so remote file/directory type changes cannot
	// wedge the mirror. File-scoped content is absent from the manifest and is
	// re-populated by its separate full archive below.
	if err := ApplyMirrorDeletions(mirrorRoot, delta.Deletions); err != nil {
		return err
	}
	if err := RemoveMirrorTypeConflicts(mirrorRoot, delta.Fetch); err != nil {
		return err
	}
	if len(delta.Fetch) > 0 {
		full := len(delta.Fetch)*2 >= delta.Total
		err := hs.downloadIntoMirror(
			ctx, client, targets.dirScoped, delta.Fetch, full, mirrorRoot,
		)
		var statusErr *StatusError
		if err != nil && !full && errors.As(err, &statusErr) {
			err = hs.downloadIntoMirror(
				ctx, client, targets.dirScoped, delta.Fetch, true, mirrorRoot,
			)
		}
		if err != nil {
			return err
		}
	}
	if !targets.fileScoped.IsEmpty() {
		if err := hs.downloadIntoMirror(
			ctx, client, targets.fileScoped, nil, true, mirrorRoot,
		); err != nil {
			return err
		}
	}
	return nil
}
