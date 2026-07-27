package parser

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const streamingDirectoryBatchSize = 64

var errStopStreamingDiscovery = errors.New("stop streaming discovery")

// DiscoveryIncompleteError reports a bounded traversal that stopped before it
// could authoritatively enumerate its configured scope. Callers must retain the
// watcher reconciliation marker and retry rather than acknowledging a partial
// result.
type DiscoveryIncompleteError struct {
	Provider AgentType
	Reason   string
}

func (err DiscoveryIncompleteError) Error() string {
	if err.Provider == "" {
		return "discovery incomplete: " + err.Reason
	}
	return string(err.Provider) + " discovery incomplete: " + err.Reason
}

type discoveryIncompleteCause struct {
	incomplete DiscoveryIncompleteError
	cause      error
}

func (err discoveryIncompleteCause) Error() string { return err.incomplete.Error() }
func (err discoveryIncompleteCause) Unwrap() []error {
	return []error{err.incomplete, err.cause}
}

type discoveryYieldError struct{ cause error }

func (err discoveryYieldError) Error() string { return err.cause.Error() }
func (err discoveryYieldError) Unwrap() error { return err.cause }

func incompleteDiscoveryError(
	provider AgentType, reason string, cause error,
) error {
	var incomplete DiscoveryIncompleteError
	if errors.As(cause, &incomplete) {
		return cause
	}
	return discoveryIncompleteCause{
		incomplete: DiscoveryIncompleteError{
			Provider: provider,
			Reason:   reason + ": " + cause.Error(),
		},
		cause: cause,
	}
}

func discoveryYieldCause(err error) (error, bool) {
	var yieldErr discoveryYieldError
	if !errors.As(err, &yieldErr) {
		return nil, false
	}
	return yieldErr.cause, true
}

type discoveryTraversalLimits struct {
	maxDirs  int
	maxFiles int
	expired  func() bool
}

type discoveryTraversalLimitsContextKey struct{}

func withDiscoveryTraversalLimits(
	ctx context.Context, limits discoveryTraversalLimits,
) context.Context {
	return context.WithValue(ctx, discoveryTraversalLimitsContextKey{}, limits)
}

func discoveryTraversalLimitsFor(ctx context.Context) discoveryTraversalLimits {
	limits, _ := ctx.Value(discoveryTraversalLimitsContextKey{}).(discoveryTraversalLimits)
	return limits
}

type streamingDirectoryReader func(
	context.Context, string, func(os.DirEntry) error,
) error

type streamingDirectoryReaderContextKey struct{}

func withStreamingDirectoryReader(
	ctx context.Context, reader streamingDirectoryReader,
) context.Context {
	return context.WithValue(ctx, streamingDirectoryReaderContextKey{}, reader)
}

type streamingDiscoveryBufferObserverKey struct{}
type streamingRetainedBytesObserverKey struct{}
type sharedContainerScanObserverKey struct{}
type reconciliationRetainedMemberObserverKey struct{}

// WithStreamingDiscoveryBufferObserver attaches instrumentation to direct
// provider discovery. Providers report actual bounded read/source batches at
// the allocation boundary rather than letting the sync engine infer them from
// its downstream spool page size.
func WithStreamingDiscoveryBufferObserver(
	ctx context.Context, observe func(int),
) context.Context {
	return context.WithValue(ctx, streamingDiscoveryBufferObserverKey{}, observe)
}

func observeStreamingDiscoveryBuffer(ctx context.Context, buffered int) {
	if buffered <= 0 {
		return
	}
	if observe, ok := ctx.Value(streamingDiscoveryBufferObserverKey{}).(func(int)); ok {
		observe(buffered)
	}
}

// WithStreamingRetainedBytesObserver records provider-owned byte buffers by
// lifetime. Providers call the observer with a positive delta when a bounded
// chunk or decoded member becomes live and the matching negative delta when it
// is released. The sync engine aggregates concurrent workers into a real peak,
// rather than treating a row count as a proxy for memory.
func WithStreamingRetainedBytesObserver(
	ctx context.Context, observe func(int64),
) context.Context {
	return context.WithValue(ctx, streamingRetainedBytesObserverKey{}, observe)
}

func observeStreamingRetainedBytes(ctx context.Context, delta int64) {
	if delta == 0 {
		return
	}
	if observe, ok := ctx.Value(streamingRetainedBytesObserverKey{}).(func(int64)); ok {
		observe(delta)
	}
}

// conservativeDecodedRetainedBytes charges enough space for the encoded
// member, transient encoded copies, decoded strings/slices/maps, and fixed Go
// object overhead. It intentionally overestimates provider-owned live bytes;
// reconciliation metrics must never present payload length as actual retained
// memory when decoded builders and copies coexist with it.
func conservativeDecodedRetainedBytes(encoded int64) int64 {
	if encoded <= 0 {
		return 0
	}
	return encoded*4 + 4*1024
}

// WithReconciliationRetainedMemberObserver observes a worker after it has
// charged one cached shared-container member and before it decodes that member.
// Tests use this operation boundary to prove concurrent aggregate retention;
// production callers normally leave it unset.
func WithReconciliationRetainedMemberObserver(
	ctx context.Context, observe func(AgentType, int64),
) context.Context {
	return context.WithValue(ctx, reconciliationRetainedMemberObserverKey{}, observe)
}

func observeReconciliationRetainedMember(
	ctx context.Context, provider AgentType, retained int64,
) {
	if observe, ok := ctx.Value(reconciliationRetainedMemberObserverKey{}).(func(AgentType, int64)); ok {
		observe(provider, retained)
	}
}

func streamingRegularFileCandidate(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, lstatErr := os.Lstat(path); lstatErr == nil {
			return false, err
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return false, lstatErr
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func streamingDirOrSymlinkCandidate(
	entry os.DirEntry, parentDir string,
) (bool, error) {
	if entry.IsDir() {
		return true, nil
	}
	if entry.Type()&os.ModeSymlink == 0 {
		return false, nil
	}
	info, err := os.Stat(filepath.Join(parentDir, entry.Name()))
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// streamingDirCandidateOrIncomplete resolves whether entry is a directory or
// a followed directory symlink during streaming discovery. A followed symlink
// whose target cannot be resolved (dangling link, unreadable parent) surfaces
// DiscoveryIncompleteError instead of reading as absent: reconciliation
// treats a clean streaming discovery as authoritative and would tombstone
// every session beneath the symlink.
func streamingDirCandidateOrIncomplete(
	provider AgentType, what string, entry os.DirEntry, parentDir string,
) (bool, error) {
	ok, err := streamingDirOrSymlinkCandidate(entry, parentDir)
	if err != nil {
		return false, incompleteDiscoveryError(
			provider,
			"resolve "+what+" "+filepath.Join(parentDir, entry.Name()),
			err,
		)
	}
	return ok, nil
}

// WithSharedContainerScanObserver counts physical shared-container traversals.
// Tests use it to distinguish one discovery scan from per-member rescans.
func WithSharedContainerScanObserver(
	ctx context.Context, observe func(),
) context.Context {
	return context.WithValue(ctx, sharedContainerScanObserverKey{}, observe)
}

func observeSharedContainerScan(ctx context.Context) {
	if observe, ok := ctx.Value(sharedContainerScanObserverKey{}).(func()); ok {
		observe()
	}
}

// streamDirectoryEntries reads a directory in fixed-size batches. os.ReadDir
// and filepath.WalkDir retain every entry in one directory so a single flat
// archive can defeat an otherwise streaming provider walker.
func streamDirectoryEntries(
	ctx context.Context, dir string, yield func(os.DirEntry) error,
) error {
	if reader, ok := ctx.Value(streamingDirectoryReaderContextKey{}).(streamingDirectoryReader); ok {
		return reader(ctx, dir, yield)
	}
	return streamDirectoryEntriesDirect(ctx, dir, yield)
}

func streamDirectoryEntriesDirect(
	ctx context.Context, dir string, yield func(os.DirEntry) error,
) error {
	file, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := file.ReadDir(streamingDirectoryBatchSize)
		observeStreamingDiscoveryBuffer(ctx, len(entries))
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(entry); err != nil {
				return err
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return nil
		default:
			return readErr
		}
	}
}

// streamDirectoryTree walks a directory without filepath.WalkDir's
// whole-directory name sort. Only one fixed-size ReadDir batch per active
// directory and the directory-depth recursion stack are retained.
func streamDirectoryTree(
	ctx context.Context,
	root string,
	yield func(path string, entry os.DirEntry) error,
) error {
	err := streamDirectoryTreeRecursive(ctx, root, yield)
	if cause, ok := discoveryYieldCause(err); ok {
		return cause
	}
	return err
}

func streamDirectoryTreeRecursive(
	ctx context.Context,
	root string,
	yield func(path string, entry os.DirEntry) error,
) error {
	var incomplete error
	err := streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			err := streamDirectoryTreeRecursive(ctx, path, yield)
			if err == nil {
				return nil
			}
			if _, ok := discoveryYieldCause(err); ok || ctx.Err() != nil {
				return err
			}
			incomplete = errors.Join(incomplete, err)
			return nil
		}
		if err := yield(path, entry); err != nil {
			return discoveryYieldError{cause: err}
		}
		return nil
	})
	if err != nil {
		if _, ok := discoveryYieldCause(err); ok || ctx.Err() != nil {
			return err
		}
		incomplete = errors.Join(incomplete, incompleteDiscoveryError(
			"", "read directory "+root, err,
		))
	}
	return incomplete
}

// StreamingDiscoverer incrementally yields source references without retaining
// the provider's full discovery result in memory. Implementations must walk
// their source directly; calling Discover from DiscoverEach defeats the
// capability's bounded-memory contract.
type StreamingDiscoverer interface {
	DiscoverEach(context.Context, func(SourceRef) error) error
}

func collectDiscoveredSources(
	ctx context.Context,
	discover func(context.Context, func(SourceRef) error) error,
) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	err := discover(ctx, func(source SourceRef) error {
		addJSONLSource(source, &sources, seen)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortJSONLSources(sources)
	return sources, nil
}
