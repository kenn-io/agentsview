package rawwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

type auditProvider struct {
	parser.ProviderBase
	root                string
	missingPathFallback bool
	streamCalls         int
	streamedSources     int
	examinedEntries     int
	planCalls           int
	planErrorAt         int
}

type partialAuditProvider struct {
	*auditProvider
	roots           []string
	unavailableRoot string
	openedRoots     int
}

type staleAuditProvider struct {
	*auditProvider
}

type duplicateAuditProvider struct {
	*auditProvider
	sources []parser.SourceRef
}

func (p *staleAuditProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	_ func(parser.SourceRef) error,
) (bool, error) {
	if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (p *duplicateAuditProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) (bool, error) {
	for _, source := range p.sources {
		if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return false, err
		}
		if err := yield(source); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (p *duplicateAuditProvider) PlanRawCapture(
	_ context.Context,
	source parser.SourceRef,
) (parser.RawCapturePlan, error) {
	return parser.RawCapturePlan{
		ConfiguredRoot: p.root,
		CaptureRoot:    p.root,
		SourceKey:      source.Key,
		Entries: []parser.RawCaptureEntry{{
			Path:      filepath.Base(source.DisplayPath),
			LocalPath: source.DisplayPath,
		}},
	}, nil
}

func newAuditProvider(root string) *auditProvider {
	return &auditProvider{
		Def: parser.AgentDef{Type: parser.AgentClaude},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support:  parser.CapabilitySupported,
			Shape:    parser.RawCaptureShapeFiles,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotNone,
		}},
		root: root,
	}
}

func (p *auditProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	panic("raw audit must not parse")
}

func (p *auditProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, err
	}
	var sources []parser.SourceRef
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			sources = append(sources, parser.SourceRef{
				Provider: parser.AgentClaude,
				Key:      entry.Name(), DisplayPath: filepath.Join(p.root, entry.Name()),
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Key < sources[j].Key })
	return sources, nil
}

func (p *auditProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) (bool, error) {
	p.streamCalls++
	dir, err := os.Open(p.root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(1)
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		entry := entries[0]
		p.examinedEntries++
		if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return false, err
		}
		if !entry.Type().IsRegular() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.root, entry.Name())); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		p.streamedSources++
		if err := yield(parser.SourceRef{
			Provider: parser.AgentClaude,
			Key:      entry.Name(), DisplayPath: filepath.Join(p.root, entry.Name()),
		}); err != nil {
			return false, err
		}
	}
}

func (p *auditProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.root}}}, nil
}

func (p *auditProvider) PlanRawCapture(
	_ context.Context, source parser.SourceRef,
) (parser.RawCapturePlan, error) {
	p.planCalls++
	if p.planErrorAt == p.planCalls {
		return parser.RawCapturePlan{}, errors.New("injected plan failure")
	}
	return parser.RawCapturePlan{
		ConfiguredRoot: p.root, CaptureRoot: p.root, SourceKey: source.Key,
		Entries: []parser.RawCaptureEntry{{
			Path: source.Key, LocalPath: filepath.Join(p.root, source.Key),
		}},
	}, nil
}

func newPartialAuditProvider(roots ...string) *partialAuditProvider {
	return &partialAuditProvider{
		auditProvider: newAuditProvider(roots[0]),
		roots:         roots,
	}
}

func (p *partialAuditProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	var sources []parser.SourceRef
	for _, root := range p.roots {
		if root == p.unavailableRoot {
			continue
		}
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				sources = append(sources, parser.SourceRef{
					Provider: parser.AgentClaude,
					Key:      entry.Name(), DisplayPath: filepath.Join(root, entry.Name()),
				})
			}
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Key < sources[j].Key })
	return sources, nil
}

func (p *partialAuditProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) (bool, error) {
	p.streamCalls++
	for _, root := range p.roots {
		if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return false, err
		}
		p.openedRoots++
		if root == p.unavailableRoot {
			continue
		}
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
				return false, err
			}
			if !entry.Type().IsRegular() {
				continue
			}
			if err := yield(parser.SourceRef{
				Provider: parser.AgentClaude,
				Key:      entry.Name(), DisplayPath: filepath.Join(root, entry.Name()),
			}); err != nil {
				return false, err
			}
		}
	}
	return p.unavailableRoot == "", nil
}

func (p *partialAuditProvider) RawCaptureSourcesForChangedPath(
	context.Context, parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return nil, nil
}

func (p *partialAuditProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	roots := make([]parser.WatchRoot, 0, len(p.roots))
	for _, root := range p.roots {
		roots = append(roots, parser.WatchRoot{Path: root})
	}
	return parser.WatchPlan{Roots: roots}, nil
}

func (p *partialAuditProvider) PlanRawCapture(
	_ context.Context, source parser.SourceRef,
) (parser.RawCapturePlan, error) {
	root := filepath.Dir(source.DisplayPath)
	return parser.RawCapturePlan{
		ConfiguredRoot: root, CaptureRoot: root, SourceKey: source.Key,
		Entries: []parser.RawCaptureEntry{{
			Path: source.Key, LocalPath: source.DisplayPath,
		}},
	}, nil
}

func newRootCoverageFailure(
	t *testing.T, maxOutboxBytes int64,
) (string, *rawcheckpoint.Store, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "session.jsonl"), []byte("session"), 0o600,
	))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: maxOutboxBytes,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	_, err = store.ReserveCapture(
		t.Context(), configured.ID, maxOutboxBytes+1,
	)
	require.ErrorIs(t, err, rawcheckpoint.ErrOutboxFull)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, configured.ID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)
	return root, store, configured.ID
}

func TestAuditorFullReconciliationClearsRootCoverageFailure(t *testing.T) {
	const maxOutboxBytes int64 = 1 << 20
	root, store, rootID := newRootCoverageFailure(t, maxOutboxBytes)

	provider := newAuditProvider(root)
	result, err := NewAuditor(store, rawcapture.New(store), 1).
		AuditProviderFull(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Zero(t, result.Degraded)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, rootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageComplete, coverage.State)
}

func TestAuditorFullReconciliationPreservesIncompleteRootCoverageFailure(t *testing.T) {
	const maxOutboxBytes int64 = 1 << 20
	root, store, rootID := newRootCoverageFailure(t, maxOutboxBytes)
	provider := newPartialAuditProvider(root)
	provider.unavailableRoot = root

	result, err := NewAuditor(store, rawcapture.New(store), 1).
		AuditProviderFull(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, result.Complete)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, rootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)
}

func TestAuditorFullReconciliationPreservesDegradedRootCoverageFailure(t *testing.T) {
	const maxOutboxBytes int64 = 1 << 20
	root, store, rootID := newRootCoverageFailure(t, maxOutboxBytes)
	blocker, err := store.ReserveCapture(t.Context(), rootID, maxOutboxBytes)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.ReleaseReservation(context.Background(), blocker.ID))
	})

	result, err := NewAuditor(store, rawcapture.New(store), 1).
		AuditProviderFull(t.Context(), newAuditProvider(root))
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, 1, result.Degraded)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, rootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)
}

func TestAuditorFullReconciliationIsolatesTombstoneBackpressureByRoot(t *testing.T) {
	const maxOutboxBytes int64 = 1 << 20
	completeRoot := t.TempDir()
	degradedRoot := t.TempDir()
	stalePath := filepath.Join(degradedRoot, "stale.jsonl")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: maxOutboxBytes,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newPartialAuditProvider(completeRoot, degradedRoot)
	capturer := rawcapture.New(store)
	captured, err := capturer.Capture(t.Context(), provider, parser.SourceRef{
		Provider:    parser.AgentClaude,
		Key:         "stale.jsonl",
		DisplayPath: stalePath,
	})
	require.NoError(t, err)
	require.Equal(t, rawcapture.StatusCaptured, captured.Status)
	require.NoError(t, os.Remove(stalePath))

	completeConfigured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, completeRoot,
	)
	require.NoError(t, err)
	degradedConfigured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, degradedRoot,
	)
	require.NoError(t, err)
	for _, rootID := range []string{completeConfigured.ID, degradedConfigured.ID} {
		_, err = store.ReserveCapture(t.Context(), rootID, maxOutboxBytes+1)
		require.ErrorIs(t, err, rawcheckpoint.ErrOutboxFull)
	}
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	remaining := maxOutboxBytes - usage.UsedBytes - usage.ReservedBytes
	require.Positive(t, remaining)
	blocker, err := store.ReserveCapture(
		t.Context(), degradedConfigured.ID, remaining,
	)
	require.NoError(t, err)

	result, err := NewAuditor(store, capturer, 1).
		AuditProviderFull(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, 1, result.Degraded)
	require.Zero(t, result.Tombstoned)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, completeConfigured.ID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageComplete, coverage.State)
	coverage, ok, err = store.Coverage(
		t.Context(), parser.AgentClaude, degradedConfigured.ID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)

	require.NoError(t, store.ReleaseReservation(t.Context(), blocker.ID))
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))
	recaptured, err := capturer.Capture(t.Context(), provider, parser.SourceRef{
		Provider:    parser.AgentClaude,
		Key:         "stale.jsonl",
		DisplayPath: stalePath,
	})
	require.NoError(t, err)
	require.NotEqual(t, rawcapture.StatusDegraded, recaptured.Status)
	coverage, ok, err = store.Coverage(
		t.Context(), parser.AgentClaude, degradedConfigured.ID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State,
		"clearing the source failure must expose the preserved root failure")
}

func TestAuditorRotatesBoundedCapturesAndRepairsDeletion(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newAuditProvider(root)
	auditor := NewAuditor(store, rawcapture.New(store), 2)

	initialCaptured := 0
	initialComplete := false
	for range 8 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		assert.LessOrEqual(t, result.Visited+result.Tombstoned, 2)
		initialCaptured += result.Captured
		if result.Complete {
			initialComplete = true
			break
		}
	}
	require.True(t, initialComplete)
	require.Equal(t, 3, initialCaptured)
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	for _, key := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		base, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
			Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: key,
		})
		require.NoError(t, err)
		require.True(t, ok, key)
		assert.Equal(t, rawsync.ManifestSnapshot, base.Kind)
	}

	require.NoError(t, os.Remove(filepath.Join(root, "b.jsonl")))
	tombstoned := 0
	for range 8 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		assert.LessOrEqual(t, result.Visited+result.Tombstoned, 2)
		if !result.Complete {
			assert.Zero(t, result.Tombstoned,
				"absence is unproven before discovery EOF")
		}
		tombstoned += result.Tombstoned
		if tombstoned != 0 {
			break
		}
	}
	assert.Equal(t, 1, tombstoned)
	baseState, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: "b.jsonl",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestTombstone, baseState.Kind)
}

func TestAuditorDoesNotTombstoneSourceRecapturedDuringResumedScan(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(targetPath, []byte("first"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newAuditProvider(root)
	capturer := rawcapture.New(store)
	target := parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "target.jsonl", DisplayPath: targetPath,
	}
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	require.NoError(t, os.Remove(targetPath))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored.txt"), nil, 0o600))
	auditor := NewAuditor(store, capturer, 1)

	paused, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, paused.Complete)
	require.Zero(t, paused.Tombstoned)
	require.NoError(t, os.WriteFile(targetPath, []byte("second"), 0o600))
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	require.NoError(t, os.Remove(targetPath))

	tombstoned := 0
	for range 4 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		tombstoned += result.Tombstoned
		if result.Complete {
			break
		}
	}
	assert.Zero(t, tombstoned)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        target.Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rawsync.ManifestSnapshot, latest.Kind)
}

func TestAuditorDoesNotTombstoneSourceObservedUnchangedDuringResumedScan(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(targetPath, []byte("first"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := &staleAuditProvider{auditProvider: newAuditProvider(root)}
	capturer := rawcapture.New(store)
	target := parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "target.jsonl", DisplayPath: targetPath,
	}
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	parkedPath := targetPath + ".parked"
	require.NoError(t, os.Rename(targetPath, parkedPath))
	auditor := NewAuditor(store, capturer, 1)

	selected, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, selected.Complete)
	paused, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, paused.Complete)
	require.NoError(t, os.Rename(parkedPath, targetPath))
	observed, err := capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	require.Equal(t, rawcapture.StatusUnchanged, observed.Status)

	tombstoned := 0
	complete := false
	for range 3 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		tombstoned += result.Tombstoned
		complete = result.Complete
		if complete {
			break
		}
	}
	assert.True(t, complete)
	assert.Zero(t, tombstoned)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        target.Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rawsync.ManifestSnapshot, latest.Kind)
}

func TestAuditorDoesNotTombstoneSourceObservedDegradedDuringResumedScan(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(targetPath, []byte("first"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := &staleAuditProvider{auditProvider: newAuditProvider(root)}
	capturer := rawcapture.New(store)
	target := parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "target.jsonl", DisplayPath: targetPath,
	}
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	const tombstoneOnlyCapacity int64 = 1200
	blocker, err := store.ReserveCapture(
		t.Context(), configured.ID,
		usage.LimitBytes-usage.UsedBytes-usage.ReservedBytes-tombstoneOnlyCapacity,
	)
	require.NoError(t, err)
	require.NotEmpty(t, blocker.ID)
	auditor := NewAuditor(store, capturer, 1)

	selected, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, selected.Complete)
	paused, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, paused.Complete)
	observed, err := capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	require.Equal(t, rawcapture.StatusDegraded, observed.Status)

	tombstoned := 0
	complete := false
	for range 3 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		tombstoned += result.Tombstoned
		complete = result.Complete
		if complete {
			break
		}
	}
	assert.True(t, complete)
	assert.Zero(t, tombstoned)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        target.Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rawsync.ManifestSnapshot, latest.Kind)
}

func TestAuditorDoesNotTombstoneSourceObservedBeforeCaptureFailure(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(targetPath, []byte("first"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := &staleAuditProvider{auditProvider: newAuditProvider(root)}
	capturer := rawcapture.New(store)
	target := parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "target.jsonl", DisplayPath: targetPath,
	}
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	auditor := NewAuditor(store, capturer, 1)

	selected, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, selected.Complete)
	paused, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, paused.Complete)
	provider.planErrorAt = provider.planCalls + 2
	_, err = capturer.Capture(t.Context(), provider, target)
	require.ErrorContains(t, err, "injected plan failure")

	tombstoned := 0
	complete := false
	for range 3 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		tombstoned += result.Tombstoned
		complete = result.Complete
		if complete {
			break
		}
	}
	assert.True(t, complete)
	assert.Zero(t, tombstoned)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        target.Key,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, rawsync.ManifestSnapshot, latest.Kind)
}

func TestAuditorChargesResumedRootWorkToCurrentTraversalBudget(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.jsonl")
	require.NoError(t, os.WriteFile(targetPath, []byte("first"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newPartialAuditProvider(root)
	capturer := rawcapture.New(store)
	target := parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "target.jsonl", DisplayPath: targetPath,
	}
	_, err = capturer.Capture(t.Context(), provider, target)
	require.NoError(t, err)
	require.NoError(t, os.Remove(targetPath))
	auditor := NewAuditor(store, capturer, 1)

	first, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, first.Complete)
	second, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.False(t, second.Complete)
	resumed, err := auditor.AuditProvider(t.Context(), provider)
	require.NoError(t, err)

	assert.False(t, resumed.Complete,
		"resumed root work must leave terminal validation for a later call")
	assert.Zero(t, resumed.Tombstoned)
}

func TestAuditorPeriodicPassStreamsLargeDiscoveryWithBoundedCaptureWork(t *testing.T) {
	root := t.TempDir()
	for index := range 257 {
		name := fmt.Sprintf("session-%03d.jsonl", index)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newAuditProvider(root)
	auditor := NewAuditor(store, rawcapture.New(store), 3)

	result, err := auditor.AuditProvider(t.Context(), provider)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Visited)
	assert.False(t, result.Complete)
	assert.Equal(t, 1, provider.streamCalls)
	assert.Equal(t, 1, provider.streamedSources)
	assert.Equal(t, 2, provider.examinedEntries)
}

func TestAuditorPeriodicPassBoundsSparseDirectoryTraversal(t *testing.T) {
	root := t.TempDir()
	for index := range 257 {
		name := fmt.Sprintf("ignored-%03d.txt", index)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	auditor := NewAuditor(store, rawcapture.New(store), 3)

	result, err := auditor.AuditProvider(t.Context(), provider)

	require.NoError(t, err)
	assert.Zero(t, result.Visited)
	assert.False(t, result.Complete)
}

func TestAuditorPeriodicPassBoundsManyEmptyRoots(t *testing.T) {
	roots := make([]string, 0, 257)
	for range 257 {
		roots = append(roots, t.TempDir())
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newPartialAuditProvider(roots...)

	result, err := NewAuditor(store, rawcapture.New(store), 3).
		AuditProvider(t.Context(), provider)

	require.NoError(t, err)
	assert.Zero(t, result.Visited)
	assert.False(t, result.Complete)
	assert.Zero(t, provider.openedRoots,
		"known-root probes must consume the same traversal budget")
}

func TestAuditorDoesNotTombstoneWhenDiscoverySkipsUnavailableRoot(t *testing.T) {
	unavailableRoot := t.TempDir()
	healthyRoot := t.TempDir()
	unavailableSource := filepath.Join(unavailableRoot, "unavailable.jsonl")
	healthySource := filepath.Join(healthyRoot, "healthy.jsonl")
	require.NoError(t, os.WriteFile(unavailableSource, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(healthySource, []byte("first"), 0o600))

	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newPartialAuditProvider(unavailableRoot, healthyRoot)
	auditor := NewAuditor(store, rawcapture.New(store), 4)

	initialCaptured := 0
	initialComplete := false
	for range 4 {
		first, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		initialCaptured += first.Captured
		if first.Complete {
			initialComplete = true
			break
		}
	}
	require.True(t, initialComplete)
	require.Equal(t, 2, initialCaptured)
	unavailableConfigured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, unavailableRoot,
	)
	require.NoError(t, err)

	provider.unavailableRoot = unavailableRoot
	require.NoError(t, os.WriteFile(healthySource, []byte("second"), 0o600))
	secondCaptured := 0
	for range 4 {
		second, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		assert.Zero(t, second.Tombstoned)
		secondCaptured += second.Captured
		if second.Complete || secondCaptured != 0 {
			break
		}
	}
	assert.Equal(t, 1, secondCaptured)

	provider.unavailableRoot = ""
	require.NoError(t, os.RemoveAll(unavailableRoot))
	require.NoError(t, os.WriteFile(healthySource, []byte("third"), 0o600))
	thirdCaptured := 0
	for range 4 {
		third, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		assert.Zero(t, third.Tombstoned)
		thirdCaptured += third.Captured
		if thirdCaptured != 0 {
			break
		}
	}
	assert.Equal(t, 1, thirdCaptured)

	baseState, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: unavailableConfigured.ID,
		SourceKey:        filepath.Base(unavailableSource),
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestSnapshot, baseState.Kind)
}

func TestAuditorClaudePartialDiscoveryDoesNotTombstone(t *testing.T) {
	root := t.TempDir()
	healthyRoot := t.TempDir()
	unavailableProject := filepath.Join(root, "unavailable-project")
	healthyProject := filepath.Join(healthyRoot, "healthy-project")
	unavailableSource := filepath.Join(unavailableProject, "unavailable.jsonl")
	healthySource := filepath.Join(healthyProject, "healthy.jsonl")
	require.NoError(t, os.MkdirAll(unavailableProject, 0o755))
	require.NoError(t, os.MkdirAll(healthyProject, 0o755))
	require.NoError(t, os.WriteFile(unavailableSource, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(healthySource, []byte("{}\n"), 0o600))

	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{
		Roots: []string{root, healthyRoot},
	})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	var unavailableKey string
	for _, source := range discovery.Sources {
		if source.DisplayPath == unavailableSource {
			unavailableKey = source.Key
		}
	}
	require.NotEmpty(t, unavailableKey)
	auditor := NewAuditor(store, rawcapture.New(store), 4)

	initialCaptured := 0
	initialComplete := false
	for range 4 {
		first, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		initialCaptured += first.Captured
		if first.Complete {
			initialComplete = true
			break
		}
	}
	require.True(t, initialComplete)
	require.Equal(t, 2, initialCaptured)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)

	parkedProject := filepath.Join(t.TempDir(), "unavailable-project")
	require.NoError(t, os.Rename(unavailableProject, parkedProject))
	danglingTarget := filepath.Join(t.TempDir(), "missing-project")
	if err := os.Symlink(danglingTarget, unavailableProject); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	require.NoError(t, os.WriteFile(healthySource, []byte("{\"changed\":true}\n"), 0o600))
	partialCaptured := 0
	for range 2 {
		partial, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		assert.Zero(t, partial.Tombstoned)
		partialCaptured += partial.Captured
	}
	assert.Equal(t, 1, partialCaptured)

	baseState, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        unavailableKey,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestSnapshot, baseState.Kind)

	require.NoError(t, os.Remove(unavailableProject))
	tombstoned := 0
	for range 8 {
		deleted, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		tombstoned += deleted.Tombstoned
		if tombstoned != 0 {
			break
		}
	}
	assert.Equal(t, 1, tombstoned)
}

func TestAuditorDoesNotLetOldTombstonesStarveNewSources(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newAuditProvider(root)
	auditor := NewAuditor(store, rawcapture.New(store), 2)
	for range 8 {
		result, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
		if result.Complete {
			break
		}
	}
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.Remove(filepath.Join(root, name)))
	}
	for range 8 {
		_, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "d.jsonl"), []byte("new"), 0o600))
	for range 8 {
		_, err := auditor.AuditProvider(t.Context(), provider)
		require.NoError(t, err)
	}
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	captured, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: "d.jsonl",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestSnapshot, captured.Kind)
}

func TestAuditorBoundedCapturesPhysicalDuplicates(t *testing.T) {
	root := t.TempDir()
	stalePath := filepath.Join(root, "stale.jsonl")
	preferredPath := filepath.Join(root, "preferred.jsonl")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(preferredPath, []byte("preferred"), 0o600))
	stale := parser.SourceRef{
		Provider: parser.AgentClaude, Key: "duplicate", DisplayPath: stalePath,
	}
	preferred := parser.SourceRef{
		Provider: parser.AgentClaude, Key: "duplicate", DisplayPath: preferredPath,
	}
	provider := &duplicateAuditProvider{
		auditProvider: newAuditProvider(root),
		sources:       []parser.SourceRef{stale, preferred},
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	result, err := NewAuditor(store, rawcapture.New(store), 8).
		AuditProvider(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, 2, result.Visited)
	require.Equal(t, 2, result.Captured)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentClaude,
		ConfiguredRootID: configured.ID,
		SourceKey:        "duplicate",
	})
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, latest.Entries, 1)
	assert.Equal(t, filepath.Base(preferredPath), latest.Entries[0].Path)
}

func TestAuditorFullCapturesPhysicalCodexDuplicatesWithinRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	metadata := `{"timestamp":"2026-06-11T12:44:06Z","type":"session_meta","payload":{"id":"` +
		uuid + `"}}` + "\n"
	archivedPath := filepath.Join(
		root, "rollout-2026-06-12T08-00-00-"+uuid+".jsonl",
	)
	livePath := filepath.Join(
		root, "2026", "06", "11",
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(livePath), 0o755))
	require.NoError(t, os.WriteFile(
		archivedPath, []byte(metadata+`{"copy":"archived"}`+"\n"), 0o600,
	))
	require.NoError(t, os.WriteFile(
		livePath, []byte(metadata+`{"copy":"live"}`+"\n"), 0o600,
	))
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	checkpointDir := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(checkpointDir, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(checkpointDir, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	result, err := NewAuditor(store, rawcapture.New(store), 8).
		AuditProviderFull(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, 2, result.Visited)
	require.Equal(t, 2, result.Captured)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentCodex, root,
	)
	require.NoError(t, err)
	latest, found, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         parser.AgentCodex,
		ConfiguredRootID: configured.ID,
		SourceKey:        parser.CodexSourceKey(parser.AgentCodex, uuid),
	})
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, latest.Entries, 1)
	assert.NotEmpty(t, latest.Entries[0].Path)
}
