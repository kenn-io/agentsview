package rawwatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/rawupload"
)

// Only the remote metadata seam is in memory. Canonical manifests, object
// custody, digest verification, capture, upload and local receipts are real.
type backfillMetadata struct {
	commits   map[string]rawsync.CommitResult
	manifests map[string]rawsync.CanonicalManifest
	heads     map[string]rawsync.CommitResult
}

func (m *backfillMetadata) RecordVerifiedObject(context.Context, rawsync.AuthIdentity, rawsync.ObjectRef) error {
	return nil
}
func (m *backfillMetadata) RecordVerifiedObjects(context.Context, rawsync.AuthIdentity, []rawsync.ObjectRef) error {
	return nil
}
func (m *backfillMetadata) MissingObjects(context.Context, rawsync.AuthIdentity, []rawsync.ObjectRef) ([]rawsync.ObjectRef, error) {
	return nil, errors.New("metadata must not decide custody")
}
func (m *backfillMetadata) CommitManifest(_ context.Context, manifest rawsync.CanonicalManifest, _ string) (rawsync.CommitResult, error) {
	if commit, ok := m.commits[manifest.ManifestID]; ok {
		return commit, nil
	}
	key := string(manifest.Manifest.Provider) + "/" + manifest.Manifest.ConfiguredRootID + "/" + manifest.Manifest.SourceKey
	head := m.heads[key]
	if manifest.Manifest.ExpectedParentReceipt != head.Receipt {
		return rawsync.CommitResult{}, rawsync.ErrConflict
	}
	commit := rawsync.CommitResult{ManifestID: manifest.ManifestID, Receipt: fmt.Sprintf("%x", sha256.Sum256([]byte("receipt/"+manifest.ManifestID))), Generation: head.Generation + 1, Created: true}
	m.commits[manifest.ManifestID] = commit
	m.manifests[manifest.ManifestID] = manifest
	m.heads[key] = commit
	return commit, nil
}

type backfillCustodyTransport struct {
	service     *rawsync.Service
	objects     rawsync.ObjectStore
	metadata    *backfillMetadata
	afterCommit func()
	uploaded    int
	commitCalls int
}

var backfillIdentity = rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}

func (t *backfillCustodyTransport) MissingObjects(ctx context.Context, p parser.AgentType, refs []rawsync.ObjectRef) ([]rawsync.ObjectRef, error) {
	return t.service.MissingObjects(ctx, backfillIdentity, p, refs)
}
func (t *backfillCustodyTransport) UploadObject(ctx context.Context, p parser.AgentType, ref rawsync.ObjectRef, body io.ReaderAt) error {
	_, err := t.service.FinalizeObject(ctx, backfillIdentity, p, ref, io.NewSectionReader(body, 0, ref.Length))
	if err == nil {
		t.uploaded++
	}
	return err
}
func (t *backfillCustodyTransport) CommitManifest(ctx context.Context, m rawsync.Manifest) (rawsync.CommitResult, error) {
	t.commitCalls++
	commit, err := t.service.CommitManifest(ctx, backfillIdentity, m)
	if err == nil && t.afterCommit != nil {
		t.afterCommit()
	}
	return commit, err
}
func newBackfillCustody(t *testing.T) *backfillCustodyTransport {
	t.Helper()
	repo, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repo.Content())
	require.NoError(t, err)
	metadata := &backfillMetadata{commits: map[string]rawsync.CommitResult{}, manifests: map[string]rawsync.CanonicalManifest{}, heads: map[string]rawsync.CommitResult{}}
	service, err := rawsync.NewService(objects, metadata, rawsync.DefaultManifestLimits(), "raw-test")
	require.NoError(t, err)
	return &backfillCustodyTransport{service: service, objects: objects, metadata: metadata}
}
func TestBackfillCrashAfterServerReceiptPreservesManifestAndGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("first\n"), 0o600))
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := rawcheckpoint.Open(t.Context(), checkpointPath)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	spec := rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}}}
	transport := newBackfillCustody(t)
	ctx, cancel := context.WithCancel(t.Context())
	transport.afterCommit = cancel
	provider := newAuditProvider(root)
	opts := BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1}
	p, err := RunBackfill(ctx, store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), opts)
	require.Error(t, err)
	require.False(t, p.Complete)
	require.Equal(t, int64(1), p.Pending)
	source := rawcheckpoint.SourceIdentity{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID, SourceKey: "a.jsonl"}
	before, found, err := store.BackfillSource(t.Context(), spec.RunID, source)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, transport.metadata.commits, 1)
	var manifestBefore string
	for id := range transport.metadata.commits {
		manifestBefore = id
	}
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0o600))
	store, err = rawcheckpoint.Open(t.Context(), checkpointPath)
	require.NoError(t, err)
	defer store.Close()
	transport.afterCommit = nil
	p, err = RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), opts)
	require.NoError(t, err)
	require.True(t, p.Complete)
	after, found, err := store.BackfillSource(t.Context(), spec.RunID, source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before.CaptureID, after.CaptureID)
	require.Equal(t, manifestBefore, after.ManifestID)
	require.Equal(t, int64(1), after.Generation)
	require.Len(t, transport.metadata.commits, 1)
	require.Equal(t, 2, transport.commitCalls)
	require.Equal(t, 1, transport.uploaded)
	manifest := transport.metadata.manifests[manifestBefore]
	var content bytes.Buffer
	for _, object := range manifest.Manifest.Entries[0].Objects {
		_, err := transport.objects.CopyObject(t.Context(), backfillIdentity.TenantID, object, &content)
		require.NoError(t, err)
	}
	require.Equal(t, "first\n", content.String())
}

func TestBackfillBoundReceiptReplaysWithoutTransport(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("first\n"), 0o600))
	store, spec := backfillFixture(t, root)
	provider := newAuditProvider(root)
	_, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	captured, err := rawcapture.New(store).CaptureForBackfill(t.Context(), provider, parser.SourceRef{Provider: parser.AgentClaude, Key: "a.jsonl", DisplayPath: filepath.Join(root, "a.jsonl")}, spec.RunID)
	require.NoError(t, err)
	manifest, found, err := store.FinalizeNextManifestForBackfill(t.Context(), "device-a", spec.RunID)
	require.NoError(t, err)
	require.True(t, found)
	transport := newBackfillCustody(t)
	for _, object := range manifest.Entries[0].Objects {
		f, err := os.Open(store.ObjectPath(object))
		require.NoError(t, err)
		require.NoError(t, transport.UploadObject(t.Context(), parser.AgentClaude, object, f))
		require.NoError(t, f.Close())
	}
	receipt, err := transport.CommitManifest(t.Context(), manifest)
	require.NoError(t, err)
	require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", captured.CaptureID, receipt))
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.NoError(t, err)
	require.True(t, p.Complete)
	require.Equal(t, 1, transport.commitCalls)
	require.Equal(t, 1, transport.uploaded)
}

type backfillShapeProvider struct {
	parser.ProviderBase
	root, path string
	appendable bool
	parses     int
	streams    int
}

func (p *backfillShapeProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.root}}}, nil
}
func (p *backfillShapeProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	p.parses++
	return parser.ParseOutcome{}, errors.New("normalized parsing forbidden")
}
func (p *backfillShapeProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(parser.SourceRef) error) (bool, error) {
	p.streams++
	if err := parser.ReportRawCaptureDiscoveryProgress(ctx); err != nil {
		return false, err
	}
	err := yield(parser.SourceRef{Provider: p.Def.Type, Key: filepath.Base(p.path), DisplayPath: p.path})
	return err == nil, err
}
func (p *backfillShapeProvider) PlanRawCapture(context.Context, parser.SourceRef) (parser.RawCapturePlan, error) {
	return parser.RawCapturePlan{ConfiguredRoot: p.root, CaptureRoot: p.root, SourceKey: filepath.Base(p.path), Entries: []parser.RawCaptureEntry{{Path: filepath.Base(p.path), LocalPath: p.path, Appendable: p.appendable}}}, nil
}

func TestBackfillMultipleShapesCapturesSQLiteAndFilesWithoutParsing(t *testing.T) {
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	spec := rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example"}
	var providers []parser.Provider
	for i, typ := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentOpenCode} {
		root := t.TempDir()
		path := filepath.Join(root, "source.jsonl")
		caps := parser.RawCaptureCapabilities{Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles, Append: parser.RawCaptureAppendOne, Snapshot: parser.RawCaptureSnapshotNone}
		if i == 1 {
			caps.Append = parser.RawCaptureAppendReplaceOnly
		}
		if i == 2 {
			path = filepath.Join(root, "source.db")
			caps.Shape = parser.RawCaptureShapeSQLite
			caps.Snapshot = parser.RawCaptureSnapshotOnlineBackup
			caps.Append = parser.RawCaptureAppendReplaceOnly
			db, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			defer db.Close()
			_, err = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE source(value TEXT); INSERT INTO source VALUES('sqlite fixture')`)
			require.NoError(t, err)
		} else {
			require.NoError(t, os.WriteFile(path, []byte("synthetic raw fixture\n"), 0o600))
		}
		p := &backfillShapeProvider{Def: parser.AgentDef{Type: typ}, Caps: parser.Capabilities{RawCapture: caps}, root: root, path: path, appendable: i == 0}
		providers = append(providers, p)
		configured, err := store.ResolveConfiguredRoot(t.Context(), typ, root)
		require.NoError(t, err)
		spec.Providers = append(spec.Providers, typ)
		spec.Roots = append(spec.Roots, rawcheckpoint.BackfillSelection{Provider: typ, ConfiguredRootID: configured.ID})
	}
	transport := newBackfillCustody(t)
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), BackfillOptions{Spec: spec, Providers: providers, BatchSize: 1})
	require.NoError(t, err, "progress: %+v", p)
	require.True(t, p.Complete)
	require.Equal(t, int64(3), p.Captured)
	require.Len(t, transport.metadata.manifests, 3)
	for _, provider := range providers {
		require.Zero(t, provider.(*backfillShapeProvider).parses)
	}
	for _, manifest := range transport.metadata.manifests {
		var data bytes.Buffer
		for _, ref := range manifest.Manifest.Entries[0].Objects {
			_, err := transport.objects.CopyObject(t.Context(), backfillIdentity.TenantID, ref, &data)
			require.NoError(t, err)
		}
		if manifest.Manifest.Provider == parser.AgentOpenCode {
			snapshot := filepath.Join(t.TempDir(), "snapshot.db")
			require.NoError(t, os.WriteFile(snapshot, data.Bytes(), 0o600))
			db, err := sql.Open("sqlite3", snapshot)
			require.NoError(t, err)
			var value string
			require.NoError(t, db.QueryRow(`SELECT value FROM source`).Scan(&value))
			require.Equal(t, "sqlite fixture", value)
			require.NoError(t, db.Close())
		} else {
			require.Equal(t, "synthetic raw fixture\n", data.String())
		}
	}
}

func TestBackfillDeferredUploadAndCancelledBeforeCaptureAreIncomplete(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("first\n"), 0o600))
	store, spec := backfillFixture(t, root)
	provider := newAuditProvider(root)
	_, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p, err := RunBackfill(ctx, store, rawcapture.New(store), rawupload.New(store, newBackfillCustody(t), "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	p, err = store.BackfillProgress(t.Context(), spec.RunID)
	require.NoError(t, err)
	require.Zero(t, p.Captured)
	source := parser.SourceRef{Provider: parser.AgentClaude, Key: "a.jsonl", DisplayPath: filepath.Join(root, "a.jsonl")}
	capture, err := rawcapture.New(store).CaptureForBackfill(t.Context(), provider, source, spec.RunID)
	require.NoError(t, err)
	_, _, err = store.FinalizeNextManifestForBackfill(t.Context(), "device-a", spec.RunID)
	require.NoError(t, err)
	require.NoError(t, store.RecordGenerationFailure(t.Context(), "device-a", capture.CaptureID, rawcheckpoint.GenerationFailureTransient, time.Now().Add(time.Hour)))
	transport := newBackfillCustody(t)
	p, err = RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	require.Equal(t, int64(1), p.Pending)
	require.Equal(t, int64(1), p.Failures["deferred"])
	require.Zero(t, transport.commitCalls)
}

func TestBackfillCompletedProviderPassAndReceiptSurviveResumeAndWatch(t *testing.T) {
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	spec := rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example"}
	var providers []parser.Provider
	for _, typ := range []parser.AgentType{parser.AgentClaude, parser.AgentCodex} {
		root := t.TempDir()
		path := filepath.Join(root, "source.jsonl")
		require.NoError(t, os.WriteFile(path, []byte("first\n"), 0o600))
		provider := &backfillShapeProvider{Def: parser.AgentDef{Type: typ}, Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles}}, root: root, path: path}
		providers = append(providers, provider)
		configured, err := store.ResolveConfiguredRoot(t.Context(), typ, root)
		require.NoError(t, err)
		spec.Providers = append(spec.Providers, typ)
		spec.Roots = append(spec.Roots, rawcheckpoint.BackfillSelection{Provider: typ, ConfiguredRootID: configured.ID})
	}
	transport := newBackfillCustody(t)
	ctx, cancel := context.WithCancel(t.Context())
	transport.afterCommit = func() {
		if transport.commitCalls == 2 {
			cancel()
		}
	}
	opts := BackfillOptions{Spec: spec, Providers: providers, BatchSize: 1}
	p, err := RunBackfill(ctx, store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), opts)
	require.Error(t, err)
	require.False(t, p.Complete)
	done, err := store.BackfillProviderComplete(t.Context(), spec.RunID, parser.AgentClaude)
	require.NoError(t, err)
	require.True(t, done)
	first := providers[0].(*backfillShapeProvider)
	firstCalls := first.streams
	transport.afterCommit = nil
	p, err = RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), opts)
	require.NoError(t, err)
	require.True(t, p.Complete)
	require.Equal(t, firstCalls, first.streams)
	require.NoError(t, os.WriteFile(first.path, []byte("first\nlater\n"), 0o600))
	captured, err := rawcapture.New(store).Capture(t.Context(), first, parser.SourceRef{Provider: parser.AgentClaude, Key: filepath.Base(first.path), DisplayPath: first.path})
	require.NoError(t, err)
	uploaded, found, err := rawupload.New(store, transport, "device-a").UploadNext(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, captured.CaptureID, uploaded.CaptureID)
	require.Equal(t, int64(2), uploaded.Generation)
	after, err := store.BackfillProgress(t.Context(), spec.RunID)
	require.NoError(t, err)
	require.Equal(t, p, after)
	member, found, err := store.BackfillSource(t.Context(), spec.RunID, captured.Source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(1), member.Generation)
	require.NotEqual(t, captured.CaptureID, member.CaptureID)
}

func TestBackfillNoEligibleUploadStopsBeforeDiscovery(t *testing.T) {
	for _, state := range []string{"deferred", "blocked", "invalidated"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"bound.jsonl", "new-a.jsonl", "new-b.jsonl"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("synthetic fixture\n"), 0o600))
			}
			store, spec := backfillFixture(t, root)
			provider := newAuditProvider(root)
			_, err := store.BeginBackfill(t.Context(), spec)
			require.NoError(t, err)
			captured, err := rawcapture.New(store).CaptureForBackfill(t.Context(), provider, parser.SourceRef{Provider: parser.AgentClaude, Key: "bound.jsonl", DisplayPath: filepath.Join(root, "bound.jsonl")}, spec.RunID)
			require.NoError(t, err)
			manifest, found, err := store.FinalizeNextManifestForBackfill(t.Context(), "device-a", spec.RunID)
			require.NoError(t, err)
			require.True(t, found)
			switch state {
			case "deferred":
				require.NoError(t, store.RecordGenerationFailure(t.Context(), "device-a", captured.CaptureID, rawcheckpoint.GenerationFailureTransient, time.Now().Add(time.Hour)))
			case "blocked":
				require.NoError(t, store.RecordGenerationFailure(t.Context(), "device-a", captured.CaptureID, rawcheckpoint.GenerationFailurePermanent, time.Time{}))
			case "invalidated":
				require.NoError(t, os.Remove(store.ObjectPath(manifest.Entries[0].Objects[0])))
				_, err = store.Recover(t.Context())
				require.NoError(t, err)
			}
			before, err := store.BackfillProgress(t.Context(), spec.RunID)
			require.NoError(t, err)
			require.Equal(t, int64(1), before.Pending)
			planCalls := provider.planCalls
			transport := newBackfillCustody(t)
			progress, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
			require.ErrorIs(t, err, rawcheckpoint.ErrBackfillIncomplete)
			require.False(t, progress.Complete)
			require.Zero(t, provider.streamCalls, "unavailable upload work must stop before discovery starts")
			require.Equal(t, planCalls, provider.planCalls, "uncaptured files must not be examined")
			require.Equal(t, before.Captured, progress.Captured)
			require.Equal(t, before.Pending, progress.Pending)
			require.Equal(t, int64(1), progress.Failures["deferred"])
			durable, err := store.BackfillProgress(t.Context(), spec.RunID)
			require.NoError(t, err)
			require.Equal(t, progress, durable)
			require.Zero(t, transport.commitCalls)
			if state == "invalidated" {
				require.Equal(t, int64(1), durable.Failures["capture_lost"])
			}
		})
	}
}
