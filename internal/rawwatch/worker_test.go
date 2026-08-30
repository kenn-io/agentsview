package rawwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawupload"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type recordingRawUploadTransport struct {
	commits    int
	commitErrs []error
}

func (t *recordingRawUploadTransport) MissingObjects(
	context.Context,
	parser.AgentType,
	[]rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	return nil, nil
}

func (t *recordingRawUploadTransport) UploadObject(
	context.Context,
	parser.AgentType,
	rawsync.ObjectRef,
	io.ReaderAt,
) error {
	return nil
}

func (t *recordingRawUploadTransport) CommitManifest(
	_ context.Context,
	_ rawsync.Manifest,
) (rawsync.CommitResult, error) {
	t.commits++
	if t.commits <= len(t.commitErrs) && t.commitErrs[t.commits-1] != nil {
		return rawsync.CommitResult{}, t.commitErrs[t.commits-1]
	}
	return rawsync.CommitResult{
		ManifestID: fmt.Sprintf("%064x", t.commits),
		Receipt:    fmt.Sprintf("%064x", t.commits+100),
		Generation: 1,
		Created:    true,
	}, nil
}

func TestWorkerDrainSkipsPermanentlyRejectedGeneration(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first.jsonl", "second.jsonl"} {
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
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider := newAuditProvider(root)
	capturer := rawcapture.New(store)
	transport := &recordingRawUploadTransport{commitErrs: []error{
		&rawclient.APIError{
			Status: http.StatusBadRequest, Code: rawclient.CodeInvalidRequest,
		},
	}}
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 16), rawupload.New(store, transport, "device-a"),
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{FullSync: true})

	require.NoError(t, err)
	assert.Equal(t, 2, transport.commits,
		"a rejected source must not prevent another source from uploading")
	status, err := store.ClientStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.PermanentFailures)
}

func (p *auditProvider) SourcesForChangedPath(
	_ context.Context,
	req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	if _, err := os.Stat(req.Path); err != nil {
		if os.IsNotExist(err) {
			if !p.missingPathFallback {
				return nil, nil
			}
			return []parser.SourceRef{{
				Provider: parser.AgentClaude,
				Key:      filepath.Base(req.Path), DisplayPath: req.Path,
			}}, nil
		}
		return nil, err
	}
	return []parser.SourceRef{{
		Provider: parser.AgentClaude,
		Key:      filepath.Base(req.Path), DisplayPath: req.Path,
	}}, nil
}

type collidingRootProvider struct {
	parser.ProviderBase
	roots []string
}

func newCollidingRootProvider(roots ...string) *collidingRootProvider {
	return &collidingRootProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotNone,
		}},
		roots: roots,
	}
}

func (p *collidingRootProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	panic("raw watch must not parse")
}

func (p *collidingRootProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	return nil, nil
}

func (p *collidingRootProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	plan := parser.WatchPlan{}
	for _, root := range p.roots {
		plan.Roots = append(plan.Roots, parser.WatchRoot{Path: root})
	}
	return plan, nil
}

func (p *collidingRootProvider) SourcesForChangedPath(
	_ context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{{
		Provider: parser.AgentForge, Key: "state.db", DisplayPath: req.Path,
		FingerprintKey: req.Path,
	}}, nil
}

func (p *collidingRootProvider) PlanRawCapture(
	_ context.Context, source parser.SourceRef,
) (parser.RawCapturePlan, error) {
	root := filepath.Dir(source.DisplayPath)
	return parser.RawCapturePlan{
		ConfiguredRoot: root, CaptureRoot: root, SourceKey: source.Key,
		Entries: []parser.RawCaptureEntry{{
			Path: "state.db", LocalPath: source.DisplayPath,
		}},
	}, nil
}

func TestWorkerCapturesCollidingSourceKeysFromSeparateRoots(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	firstPath := filepath.Join(firstRoot, "state.db")
	secondPath := filepath.Join(secondRoot, "state.db")
	require.NoError(t, os.WriteFile(firstPath, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("second"), 0o600))
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newCollidingRootProvider(firstRoot, secondRoot)
	capturer := rawcapture.New(store)
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 16), nil,
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{
		Paths: []string{firstPath, secondPath},
	})

	require.NoError(t, err)
	for _, rootPath := range []string{firstRoot, secondRoot} {
		root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentForge, rootPath)
		require.NoError(t, err)
		_, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
			Provider: parser.AgentForge, ConfiguredRootID: root.ID, SourceKey: "state.db",
		})
		require.NoError(t, err)
		assert.True(t, ok, rootPath)
	}
}

func TestWorkerFullSyncAuditsBeyondBoundedLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first.jsonl", "second.jsonl", "third.jsonl"} {
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
	capturer := rawcapture.New(store)
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 1), nil,
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{FullSync: true})
	require.NoError(t, err)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	for _, name := range []string{"first.jsonl", "second.jsonl", "third.jsonl"} {
		baseState, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
			Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: name,
		})
		require.NoError(t, err)
		require.True(t, ok, name)
		assert.Equal(t, rawsync.ManifestSnapshot, baseState.Kind)
	}
}

func TestWorkerPromotesUncertainWatchBatchesToAudit(t *testing.T) {
	tests := map[string]syncpkg.WatchBatch{
		"reconcile root": {ReconcileRoots: []string{"lost-event-root"}},
		"rename":         {Renames: []syncpkg.WatchRename{{Path: "renamed-source"}}},
	}
	for name, batch := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "session.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("session\n"), 0o600))
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
			worker := NewWorker(
				[]parser.Provider{provider}, capturer,
				NewAuditor(store, capturer, 16), nil,
			)

			require.NoError(t, worker.HandleBatch(t.Context(), batch))

			configured, err := store.ResolveConfiguredRoot(
				t.Context(), parser.AgentClaude, root,
			)
			require.NoError(t, err)
			_, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
				Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
				SourceKey: "session.jsonl",
			})
			require.NoError(t, err)
			assert.True(t, ok, "the audit must capture sources absent from batch.Paths")
		})
	}
}

func TestWorkerFullSyncReportsOutboxBackpressure(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first.jsonl", "second.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 2000,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider := newAuditProvider(root)
	capturer := rawcapture.New(store)
	transport := &recordingRawUploadTransport{}
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 1), rawupload.New(store, transport, "device-a"),
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{FullSync: true})

	require.ErrorIs(t, err, ErrFullSyncIncomplete)
	assert.Equal(t, 1, transport.commits, "queued work must drain before the retry")
	require.NoError(t, worker.HandleBatch(t.Context(), syncpkg.WatchBatch{FullSync: true}))
	assert.Equal(t, 2, transport.commits, "the retry must capture and drain deferred work")
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	for _, name := range []string{"first.jsonl", "second.jsonl"} {
		_, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
			Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: name,
		})
		require.NoError(t, err)
		assert.True(t, ok, name)
	}
}

func TestWorkerFullSyncReportsIncompleteDiscovery(t *testing.T) {
	healthyRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte("healthy"), 0o600))
	unavailableRoot := filepath.Join(t.TempDir(), "unavailable")
	provider := newPartialAuditProvider(healthyRoot, unavailableRoot)
	provider.unavailableRoot = unavailableRoot
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	capturer := rawcapture.New(store)
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 16), nil,
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{FullSync: true})

	require.ErrorIs(t, err, ErrFullSyncIncomplete)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, healthyRoot,
	)
	require.NoError(t, err)
	_, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		SourceKey: "session.jsonl",
	})
	require.NoError(t, err)
	assert.True(t, ok, "healthy sources must still be captured")
}

func TestWorkerAuditsMissingFallbackAndContinuesBatch(t *testing.T) {
	root := t.TempDir()
	deletedPath := filepath.Join(root, "deleted.jsonl")
	changedPath := filepath.Join(root, "changed.jsonl")
	require.NoError(t, os.WriteFile(deletedPath, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(changedPath, []byte("one\n"), 0o600))
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
	provider.missingPathFallback = true
	capturer := rawcapture.New(store)
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 16), nil,
	)
	require.NoError(t, worker.AuditAll(t.Context()))
	require.NoError(t, os.Remove(deletedPath))
	require.NoError(t, os.WriteFile(changedPath, []byte("two\n"), 0o600))

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{
		Paths: []string{deletedPath, changedPath},
	})

	require.NoError(t, err)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, root,
	)
	require.NoError(t, err)
	deleted, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		SourceKey: "deleted.jsonl",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestTombstone, deleted.Kind)
	changed, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		SourceKey: "changed.jsonl",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestSnapshot, changed.Kind)
	assert.Equal(t, int64(4), changed.Entries[0].Length)
}

func TestWorkerCapturesChangedPathAndAuditsDeletionWithoutParsing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))
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
	worker := NewWorker(
		[]parser.Provider{provider}, capturer,
		NewAuditor(store, capturer, 16), nil,
	)

	err = worker.HandleBatch(t.Context(), syncpkg.WatchBatch{Paths: []string{path}})
	require.NoError(t, err)
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	identity := rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		SourceKey: "session.jsonl",
	}
	first, ok, err := store.CaptureBase(t.Context(), identity)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestSnapshot, first.Kind)

	require.NoError(t, os.WriteFile(path, []byte("two\n"), 0o600))
	require.NoError(t, worker.HandleBatch(
		t.Context(), syncpkg.WatchBatch{Paths: []string{path}},
	))
	second, ok, err := store.CaptureBase(t.Context(), identity)
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEqual(t, first.CaptureID, second.CaptureID)

	require.NoError(t, os.Remove(path))
	require.NoError(t, worker.HandleBatch(
		t.Context(), syncpkg.WatchBatch{Paths: []string{path}},
	))
	deleted, ok, err := store.CaptureBase(t.Context(), identity)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawsync.ManifestTombstone, deleted.Kind)
}

func TestWorkerAuditSkipsMissingProviderRootAndContinues(t *testing.T) {
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	healthyRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte("healthy\n"), 0o600))
	missing := newAuditProvider(filepath.Join(t.TempDir(), "missing"))
	healthy := newAuditProvider(healthyRoot)
	capturer := rawcapture.New(store)
	worker := NewWorker(
		[]parser.Provider{missing, healthy}, capturer,
		NewAuditor(store, capturer, 16), nil,
	)

	err = worker.AuditAll(t.Context())

	require.NoError(t, err)
	configured, err := store.ResolveConfiguredRoot(
		t.Context(), parser.AgentClaude, healthyRoot,
	)
	require.NoError(t, err)
	_, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: configured.ID,
		SourceKey: "session.jsonl",
	})
	require.NoError(t, err)
	assert.True(t, ok)
}
