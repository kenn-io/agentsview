package rawcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

type captureTestProvider struct {
	parser.ProviderBase
	plan      parser.RawCapturePlan
	planCalls atomic.Int32
	planHook  func(int32)
}

func (p *captureTestProvider) Parse(
	context.Context,
	parser.ParseRequest,
) (parser.ParseOutcome, error) {
	panic("raw capture must not invoke Parse")
}

func (p *captureTestProvider) PlanRawCapture(
	context.Context,
	parser.SourceRef,
) (parser.RawCapturePlan, error) {
	call := p.planCalls.Add(1)
	if p.planHook != nil {
		p.planHook(call)
	}
	return p.plan, nil
}

func openCapturerTestStore(
	t *testing.T,
	maxBytes int64,
) (*rawcheckpoint.Store, string) {
	t.Helper()
	base := t.TempDir()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(),
		filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir:       filepath.Join(base, "spool"),
			MaxOutboxBytes: maxBytes,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, base
}

func captureFileProvider(
	t *testing.T,
	content string,
) (*captureTestProvider, parser.SourceRef, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	var err error
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	path, err = filepath.EvalSymlinks(path)
	require.NoError(t, err)
	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentClaude},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support:  parser.CapabilitySupported,
			Shape:    parser.RawCaptureShapeFiles,
			Append:   parser.RawCaptureAppendOne,
			Snapshot: parser.RawCaptureSnapshotNone,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root,
			CaptureRoot:    root,
			SourceKey:      "source-1",
			Entries: []parser.RawCaptureEntry{{
				Path:       "project/session.jsonl",
				LocalPath:  path,
				Appendable: true,
			}},
		},
	}
	return provider, parser.SourceRef{
		Provider: parser.AgentClaude,
		Key:      "source-1",
	}, path
}

func TestCapturerPersistsStableGenerationWithoutParsing(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, result.Status)
	assert.NotEmpty(t, result.CaptureID)
	queued, ok, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, result.CaptureID, queued.CaptureID)
	assert.Equal(t, source.Key, queued.Source.SourceKey)
	require.Len(t, queued.Entries, 1)
	entry := queued.Entries[0]
	assert.Equal(t, "project/session.jsonl", entry.Path)
	assert.Equal(t, int64(4), entry.Length)
	assert.NotEmpty(t, entry.FileIdentity)
	assert.Equal(t, "2c8b08da5ce60398e1f19af0e5dccc744df274b826abe585eaba68c525434806", entry.PrefixSHA256)
	require.Len(t, entry.Objects, 1)
	assert.Equal(t, entry.PrefixSHA256, entry.Objects[0].SHA256)
	assert.Equal(t, int64(4), entry.Objects[0].Length)
	content, err := os.ReadFile(store.ObjectPath(entry.Objects[0]))
	require.NoError(t, err)
	assert.Equal(t, []byte("one\n"), content)
}

func TestConcurrentCapturesDoNotDiscardAnotherCaptureSharedObject(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	providerA, sourceA, _ := captureFileProvider(t, "shared\n")
	providerB, sourceB, _ := captureFileProvider(t, "shared\n")
	providerB.plan.SourceKey = "source-2"
	sourceB.Key = "source-2"
	digest := sha256.Sum256([]byte("shared\n"))
	ref := rawsync.ObjectRef{SHA256: fmt.Sprintf("%x", digest), Length: 7}
	objectDir := filepath.Dir(store.ObjectPath(ref))
	installedA := make(chan struct{})
	releaseA := make(chan struct{})
	capturerA := New(store)
	capturerA.files.syncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(objectDir) {
			close(installedA)
			<-releaseA
			return errors.New("forced directory sync failure")
		}
		return syncDirectory(path)
	}
	startedB := make(chan struct{})
	beforeCommitB := make(chan struct{})
	releaseB := make(chan struct{})
	providerB.planHook = func(call int32) {
		switch call {
		case 1:
			close(startedB)
		case 2:
			close(beforeCommitB)
			<-releaseB
		}
	}
	type captureOutcome struct {
		result Result
		err    error
	}
	outcomeA := make(chan captureOutcome, 1)
	outcomeB := make(chan captureOutcome, 1)
	go func() {
		result, err := capturerA.Capture(t.Context(), providerA, sourceA)
		outcomeA <- captureOutcome{result: result, err: err}
	}()
	<-installedA
	go func() {
		result, err := New(store).Capture(t.Context(), providerB, sourceB)
		outcomeB <- captureOutcome{result: result, err: err}
	}()
	<-startedB
	select {
	case <-beforeCommitB:
		close(releaseA)
		first := <-outcomeA
		require.Error(t, first.err)
		close(releaseB)
	case <-time.After(500 * time.Millisecond):
		close(releaseA)
		first := <-outcomeA
		require.Error(t, first.err)
		<-beforeCommitB
		close(releaseB)
	}

	second := <-outcomeB

	require.NoError(t, second.err)
	assert.Equal(t, StatusCaptured, second.result.Status)
	assert.FileExists(t, store.ObjectPath(ref))
}

func TestCapturerReturnsUnchangedWithoutAnotherGeneration(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)

	second, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusUnchanged, second.Status)
	assert.Empty(t, second.CaptureID)
	base, ok, err := store.CaptureBase(t.Context(), rawcheckpoint.SourceIdentity{
		Provider:         source.Provider,
		ConfiguredRootID: first.Source.ConfiguredRootID,
		SourceKey:        source.Key,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, base.CaptureID)
}

func TestCapturerReplacesPermanentFailureWhenSourceIsUnchanged(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "two\n"))
	second, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	_, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, store.RecordGenerationFailure(
		t.Context(), "device-a", first.CaptureID,
		rawcheckpoint.GenerationFailurePermanent, time.Time{},
	))

	base, ok, err := store.CaptureBase(t.Context(), second.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, second.CaptureID, base.CaptureID,
		"replacement must compare the newest rejected suffix content")
	assert.True(t, base.PermanentlyRejected)
	var rejectedRefs []rawsync.ObjectRef
	for _, entry := range base.Entries {
		rejectedRefs = append(rejectedRefs, entry.Objects...)
	}

	replacement, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, replacement.Status)
	assert.NotEqual(t, second.CaptureID, replacement.CaptureID)
	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, replacement.CaptureID, manifest.CaptureID)
	require.Len(t, manifest.Entries, 1)
	assert.Len(t, manifest.Entries[0].Objects, 1,
		"a source revision must replace the rejected generation with a full snapshot")
	replacementRef := manifest.Entries[0].Objects[0]
	for _, ref := range rejectedRefs {
		if ref != replacementRef {
			assert.NoFileExists(t, store.ObjectPath(ref))
		}
	}
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t,
		replacementRef.Length+rawcheckpoint.CaptureMetadataCharge(1, 1),
		usage.UsedBytes,
	)
	status, err := store.ClientStatus(t.Context())
	require.NoError(t, err)
	assert.Zero(t, status.PermanentFailures)
}

func TestCapturerReplacesPermanentFailureWithinOneGenerationCapacity(t *testing.T) {
	store, _ := openCapturerTestStore(t, 2000)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	_, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, store.RecordGenerationFailure(
		t.Context(), "device-a", first.CaptureID,
		rawcheckpoint.GenerationFailurePermanent, time.Time{},
	))
	require.NoError(t, appendFile(sourcePath, "two\n"))

	replacement, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, replacement.Status)
	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, replacement.CaptureID, manifest.CaptureID)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.LessOrEqual(t, usage.UsedBytes, int64(2000))
}

func TestCapturerRejectsSameSizeMutationBeforeUnchangedDecision(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	initialPlanCalls := provider.planCalls.Load()
	provider.planHook = func(call int32) {
		if call == initialPlanCalls+2 {
			require.NoError(t, os.WriteFile(sourcePath, []byte("two\n"), 0o600))
		}
	}

	_, err = capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
}

func TestCapturerRejectsMutationWithoutPublishingGeneration(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.capturePhase = func(phase capturePhase, _ string) {
		if phase == capturePhaseAfterRead {
			require.NoError(t, os.WriteFile(sourcePath, []byte("changed\n"), 0o600))
		}
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerRejectsGrowthAfterCapacityReservation(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.capturePhase = func(phase capturePhase, _ string) {
		if phase == capturePhaseBeforeRead {
			require.NoError(t, appendFile(sourcePath, "two\n"))
		}
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerReleasesReservationWhenObjectInstallFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	want := errors.New("rename failed")
	capturer.files.rename = func(string, string) error { return want }

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, want)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func TestCapturerRemovesEarlierObjectsWhenLaterInstallFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, transcript := captureFileProvider(t, "one\n")
	companionPath := filepath.Join(filepath.Dir(transcript), "companion.json")
	require.NoError(t, os.WriteFile(companionPath, []byte("companion\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path: "project/companion.json", LocalPath: companionPath,
	})
	capturer := New(store)
	renames := 0
	capturer.files.rename = func(source, destination string) error {
		renames++
		if renames == 2 {
			return errors.New("second rename failed")
		}
		return os.Rename(source, destination)
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorContains(t, err, "second rename failed")
	firstRef := rawsync.ObjectRef{
		SHA256: "2c8b08da5ce60398e1f19af0e5dccc744df274b826abe585eaba68c525434806",
		Length: 4,
	}
	assert.NoFileExists(t, store.ObjectPath(firstRef))
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerRejectsEarlierEntryMutationDuringLaterCapture(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, transcript := captureFileProvider(t, "one\n")
	provider.plan.Entries[0].Path = "a-transcript.jsonl"
	companionPath := filepath.Join(filepath.Dir(transcript), "b-companion.json")
	require.NoError(t, os.WriteFile(companionPath, []byte("companion\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path: "b-companion.json", LocalPath: companionPath,
	})
	capturer := New(store)
	capturer.capturePhase = func(phase capturePhase, path string) {
		if phase == capturePhaseAfterRead && path == companionPath {
			require.NoError(t, os.WriteFile(transcript, []byte("changed\n"), 0o600))
		}
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerRejectsEntryThatEscapesRootAfterPlanValidation(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "safe\n")
	projectDir := filepath.Dir(sourcePath)
	originalDir := projectDir + "-original"
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(outsideDir, filepath.Base(sourcePath)), []byte("leak\n"), 0o600,
	))
	capturer := New(store)
	originalStat := capturer.files.stat
	swapped := false
	capturer.files.stat = func(path string) (os.FileInfo, error) {
		if path == sourcePath && !swapped {
			swapped = true
			require.NoError(t, os.Rename(projectDir, originalDir))
			if err := os.Symlink(outsideDir, projectDir); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
		}
		return originalStat(path)
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func TestCapturerRejectsCompanionAddedDuringCapture(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	companionPath := filepath.Join(filepath.Dir(sourcePath), "session_index.jsonl")
	capturer := New(store)
	capturer.capturePhase = func(phase capturePhase, _ string) {
		if phase != capturePhaseAfterRead || len(provider.plan.Entries) != 1 {
			return
		}
		require.NoError(t, os.WriteFile(companionPath, []byte("index\n"), 0o600))
		provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
			Path: "project/session_index.jsonl", LocalPath: companionPath,
		})
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func TestCapturerRejectsCompanionAddedBeforeUnchangedDecision(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	companionPath := filepath.Join(filepath.Dir(sourcePath), "session_index.jsonl")
	originalStat := capturer.files.stat
	added := false
	capturer.files.stat = func(path string) (os.FileInfo, error) {
		if path == sourcePath && !added {
			added = true
			require.NoError(t, os.WriteFile(companionPath, []byte("index\n"), 0o600))
			provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
				Path: "project/session_index.jsonl", LocalPath: companionPath,
			})
		}
		return originalStat(path)
	}

	_, err = capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	base, ok, readErr := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, readErr)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, base.CaptureID)
}

func TestCapturerRetainsReservationWhenFailedObjectCleanupFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, transcript := captureFileProvider(t, "one\n")
	companionPath := filepath.Join(filepath.Dir(transcript), "companion.json")
	require.NoError(t, os.WriteFile(companionPath, []byte("companion\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path: "project/companion.json", LocalPath: companionPath,
	})
	capturer := New(store)
	renames := 0
	capturer.files.rename = func(source, destination string) error {
		renames++
		if renames == 2 {
			return errors.New("second rename failed")
		}
		return os.Rename(source, destination)
	}
	wantCleanup := errors.New("cleanup failed")
	capturer.discardObjects = func(context.Context, []rawsync.ObjectRef) error {
		return wantCleanup
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, wantCleanup)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Positive(t, usage.ReservedBytes)
}

func TestCapturerDiscardsRenamedObjectWhenDirectorySyncFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	objectDir := filepath.Dir(store.ObjectPath(rawsync.ObjectRef{
		SHA256: "2c8b08da5ce60398e1f19af0e5dccc744df274b826abe585eaba68c525434806",
		Length: 4,
	}))
	capturer.files.syncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(objectDir) {
			return errors.New("sync failed")
		}
		return syncDirectory(path)
	}
	var discarded []rawsync.ObjectRef
	capturer.discardObjects = func(_ context.Context, refs []rawsync.ObjectRef) error {
		discarded = append(discarded, refs...)
		for _, ref := range refs {
			require.NoError(t, os.Remove(store.ObjectPath(ref)))
		}
		return nil
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorContains(t, err, "sync object directory")
	require.Len(t, discarded, 1)
	assert.NoFileExists(t, store.ObjectPath(discarded[0]))
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerSyncsEveryObjectDirectoryEntryBeforeCommit(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	var synced []string
	capturer.files.syncDir = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return syncDirectory(path)
	}

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	objectDir := filepath.Dir(store.ObjectPath(base.Entries[0].Objects[0]))
	shaDir := filepath.Dir(objectDir)
	objectsDir := filepath.Dir(shaDir)
	spoolDir := filepath.Dir(objectsDir)
	assert.Subset(t, synced, []string{
		filepath.Dir(spoolDir), spoolDir, objectsDir, shaDir, objectDir,
	})
}

func TestCapturerRetainsReservationWhenTemporaryCleanupFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.files.rename = func(string, string) error { return errors.New("rename failed") }
	wantCleanup := errors.New("temporary cleanup failed")
	capturer.files.remove = func(string) error { return wantCleanup }

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, wantCleanup)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Positive(t, usage.ReservedBytes)
}

func TestCapturerHonorsCancellation(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := New(store).Capture(ctx, provider, source)

	require.ErrorIs(t, err, context.Canceled)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerSnapshotsSQLiteWithOnlineBackup(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	db, err := sql.Open(sqliteSnapshotDriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(2)
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE items (value TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO items (value) VALUES ('committed')`)
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`INSERT INTO items (value) VALUES ('uncommitted')`)
	require.NoError(t, err)

	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support:  parser.CapabilitySupported,
			Shape:    parser.RawCaptureShapeSQLite,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotOnlineBackup,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root,
			CaptureRoot:    root,
			SourceKey:      "live.db",
			Entries: []parser.RawCaptureEntry{{
				Path: "live.db", LocalPath: dbPath,
			}},
		},
	}
	source := parser.SourceRef{Provider: parser.AgentForge, Key: "live.db"}
	provider.Caps.RawCapture.Shape = parser.RawCaptureShapeSQLite
	provider.Caps.RawCapture.Snapshot = parser.RawCaptureSnapshotOnlineBackup

	result, err := New(store).Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, result.Status)
	generation, ok, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, generation.Entries, 1)
	assert.Equal(t, "live.db", generation.Entries[0].Path)
	require.Len(t, generation.Entries[0].Objects, 1)
	snapshot, err := sql.Open(
		sqliteSnapshotDriverName, store.ObjectPath(generation.Entries[0].Objects[0]),
	)
	require.NoError(t, err)
	defer snapshot.Close()
	var values string
	err = snapshot.QueryRow(`SELECT group_concat(value, ',') FROM items`).Scan(&values)
	require.NoError(t, err)
	assert.Equal(t, "committed", values)
	temporary, err := os.ReadDir(store.CaptureTempDir())
	require.NoError(t, err)
	assert.Empty(t, temporary)

	unchanged, err := New(store).Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusUnchanged, unchanged.Status)
	require.NoError(t, tx.Rollback())
	_, err = db.Exec(`INSERT INTO items (value) VALUES ('later')`)
	require.NoError(t, err)
	changed, err := New(store).Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, changed.Status)
	assert.NotEqual(t, result.CaptureID, changed.CaptureID)
	base, ok, err := store.CaptureBase(t.Context(), changed.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, changed.CaptureID, base.CaptureID)
}

func TestCapturerRejectsSQLiteSymlinkSwapAfterPlanValidation(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	writeSQLiteCaptureTestDB(t, dbPath, "validated")
	outOfRootPath := filepath.Join(t.TempDir(), "outside.db")
	writeSQLiteCaptureTestDB(t, outOfRootPath, "outside")
	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLite,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotOnlineBackup,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root, CaptureRoot: root, SourceKey: "live.db",
			Entries: []parser.RawCaptureEntry{{Path: "live.db", LocalPath: dbPath}},
		},
	}
	source := parser.SourceRef{Provider: parser.AgentForge, Key: "live.db"}
	capturer := New(store)
	swapped := false
	capturer.capturePhase = func(phase capturePhase, path string) {
		if swapped || phase != capturePhaseBeforeRead || path == "" {
			return
		}
		swapped = true
		relocated := path + ".validated"
		require.NoError(t, os.Rename(path, relocated))
		if err := os.Symlink(outOfRootPath, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.True(t, swapped)
	require.ErrorIs(t, err, ErrSourceChanged)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerReservesSQLiteSnapshotCapacityBeforeBackup(t *testing.T) {
	store, _ := openCapturerTestStore(t, 2048)
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	db, err := sql.Open(sqliteSnapshotDriverName, dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE items (value BLOB NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO items (value) VALUES (zeroblob(16384))`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLite,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotOnlineBackup,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root, CaptureRoot: root, SourceKey: "live.db",
			Entries: []parser.RawCaptureEntry{{Path: "live.db", LocalPath: dbPath}},
		},
	}
	source := parser.SourceRef{Provider: parser.AgentForge, Key: "live.db"}
	capturer := New(store)
	backupStarted := false
	capturer.sqliteBackup = func(context.Context, *sql.Conn, string, int64) error {
		backupStarted = true
		return errors.New("backup must not start")
	}

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, result.Status)
	assert.False(t, backupStarted)
	temporary, err := os.ReadDir(store.CaptureTempDir())
	require.NoError(t, err)
	assert.Empty(t, temporary)
}

func TestCapturerKeepsCommittedSQLiteCaptureWhenSnapshotCleanupFails(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	writeSQLiteCaptureTestDB(t, dbPath, "captured")
	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLite,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotOnlineBackup,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root, CaptureRoot: root, SourceKey: "live.db",
			Entries: []parser.RawCaptureEntry{{Path: "live.db", LocalPath: dbPath}},
		},
	}
	capturer := New(store)
	capturer.files.removeAll = func(string) error { return errors.New("cleanup failed") }

	result, err := capturer.Capture(
		t.Context(), provider,
		parser.SourceRef{Provider: parser.AgentForge, Key: "live.db"},
	)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, result.Status)
	generation, ok, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, result.CaptureID, generation.CaptureID)
}

func writeSQLiteCaptureTestDB(t *testing.T, path, value string) {
	t.Helper()
	database, err := sql.Open(sqliteSnapshotDriverName, path)
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TABLE items (value TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = database.Exec(`INSERT INTO items (value) VALUES (?)`, value)
	require.NoError(t, err)
	require.NoError(t, database.Close())
}

func TestCapturerStoresOnlySuffixObjectForVerifiedAppend(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "two\n"))

	second, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, second.Status)
	assert.NotEqual(t, first.CaptureID, second.CaptureID)
	base, ok, err := store.CaptureBase(t.Context(), second.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	entry := base.Entries[0]
	assert.Equal(t, int64(8), entry.Length)
	assert.Equal(t, "c3f9c8c283a2b1f2f1896f27a01cbe3cddc0c9d93f752e4639035a0f5b36f6e8", entry.PrefixSHA256)
	require.Len(t, entry.Objects, 2)
	assert.Equal(t, "2c8b08da5ce60398e1f19af0e5dccc744df274b826abe585eaba68c525434806", entry.Objects[0].SHA256)
	assert.Equal(t, int64(4), entry.Objects[0].Length)
	assert.Equal(t, "27dd8ed44a83ff94d557f9fd0412ed5a8cbca69ea04922d88c01184a07300a5a", entry.Objects[1].SHA256)
	assert.Equal(t, int64(4), entry.Objects[1].Length)
	suffix, err := os.ReadFile(store.ObjectPath(entry.Objects[1]))
	require.NoError(t, err)
	assert.Equal(t, []byte("two\n"), suffix)
}

func TestCapturerRefreshesReusedCompanionMetadataForAppend(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, transcriptPath := captureFileProvider(t, "one\n")
	companionPath := filepath.Join(filepath.Dir(transcriptPath), "session_index.jsonl")
	require.NoError(t, os.WriteFile(companionPath, []byte("index\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path: "project/session_index.jsonl", LocalPath: companionPath,
	})
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	initialInfo, err := os.Stat(companionPath)
	require.NoError(t, err)
	touchedAt := initialInfo.ModTime().Add(time.Hour)
	require.NoError(t, os.Chtimes(companionPath, touchedAt, touchedAt))
	currentInfo, err := os.Stat(companionPath)
	require.NoError(t, err)
	require.NotEqual(t, initialInfo.ModTime(), currentInfo.ModTime())
	require.NoError(t, appendFile(transcriptPath, "two\n"))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, result.Status)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 2)
	assert.Equal(t, currentInfo.ModTime().UnixNano(), base.Entries[1].ModTimeNS)
}

func TestCapturerAppendsFromAcknowledgedBaseAfterLocalObjectGC(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, ok)
	commit := rawsync.CommitResult{
		ManifestID: strings.Repeat("a", 64),
		Receipt:    strings.Repeat("b", 64),
		Generation: 1,
		Created:    true,
	}
	require.NoError(t, store.BindFinalizedCommit(
		t.Context(), "device-a", first.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", first.CaptureID, commit)
	require.NoError(t, err)
	firstRef := rawsync.ObjectRef{
		SHA256: "2c8b08da5ce60398e1f19af0e5dccc744df274b826abe585eaba68c525434806",
		Length: 4,
	}
	assert.NoFileExists(t, store.ObjectPath(firstRef))
	require.NoError(t, appendFile(sourcePath, "two\n"))

	second, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, second.Status)
	base, ok, err := store.CaptureBase(t.Context(), second.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	assert.Equal(t, []rawsync.ObjectRef{
		firstRef,
		{SHA256: "27dd8ed44a83ff94d557f9fd0412ed5a8cbca69ea04922d88c01184a07300a5a", Length: 4},
	}, base.Entries[0].Objects)
}

func TestCapturerFallsBackToFullObjectAfterTruncation(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\ntwo\n")
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sourcePath, []byte("x\n"), 0o600))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	require.Len(t, base.Entries[0].Objects, 1)
	assert.Equal(t, "73cb3858a687a8494ca3323053016282f3dad39d42cf62ca4e79dda2aac7d9ac", base.Entries[0].Objects[0].SHA256)
	assert.Equal(t, int64(2), base.Entries[0].Objects[0].Length)
}

func TestCapturerFallsBackToFullObjectAfterSameSizeRewrite(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sourcePath, []byte("two\n"), 0o600))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	require.Len(t, base.Entries[0].Objects, 1)
	assert.Equal(t, "27dd8ed44a83ff94d557f9fd0412ed5a8cbca69ea04922d88c01184a07300a5a", base.Entries[0].Objects[0].SHA256)
}

func TestCapturerFallsBackWhenEarlyPrefixWasRewrittenBeforeAppend(t *testing.T) {
	store, _ := openCapturerTestStore(t, 2<<20)
	initial := bytes.Repeat([]byte("a"), 128<<10)
	provider, source, sourcePath := captureFileProvider(t, string(initial))
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	file, err := os.OpenFile(sourcePath, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte("b"), 0)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, appendFile(sourcePath, "tail\n"))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	assert.Len(t, base.Entries[0].Objects, 1,
		"a prefix rewrite outside the old 64 KiB boundary must force replacement")
	assert.Equal(t, int64(len(initial)+5), base.Entries[0].Objects[0].Length)
}

func TestCapturerFallsBackWhenSamePathWasReplacedBeforeAppend(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, os.Rename(sourcePath, sourcePath+".old"))
	require.NoError(t, os.WriteFile(sourcePath, []byte("one\ntwo\n"), 0o600))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	assert.Len(t, base.Entries[0].Objects, 1)
	assert.Equal(t, int64(8), base.Entries[0].Objects[0].Length)
}

func TestCapturerReplacesWholeSourceWhenCompanionChanges(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, transcript := captureFileProvider(t, "one\n")
	indexPath := filepath.Join(filepath.Dir(filepath.Dir(transcript)), "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte("index\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path:      "index.jsonl",
		LocalPath: indexPath,
	})
	capturer := New(store)
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(indexPath, []byte("index two\n"), 0o600))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 2)
	for _, entry := range base.Entries {
		assert.Len(t, entry.Objects, 1)
	}
}

func TestCapturerDegradesBeforeInstallingSuffixWhenMetadataDoesNotFit(t *testing.T) {
	store, _ := openCapturerTestStore(t, 3847)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "two\n"))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, result.Status)
	base, ok, err := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, base.CaptureID)
	suffixRef := rawsync.ObjectRef{
		SHA256: "27dd8ed44a83ff94d557f9fd0412ed5a8cbca69ea04922d88c01184a07300a5a",
		Length: 4,
	}
	assert.NoFileExists(t, store.ObjectPath(suffixRef))
}

func TestSuccessfulSourceDoesNotClearAnotherSourceCoverageGap(t *testing.T) {
	store, _ := openCapturerTestStore(t, 2000)
	root := t.TempDir()
	largePath := filepath.Join(root, "large.jsonl")
	smallPath := filepath.Join(root, "small.jsonl")
	require.NoError(t, os.WriteFile(largePath, bytes.Repeat([]byte("x"), 1000), 0o600))
	require.NoError(t, os.WriteFile(smallPath, []byte("x"), 0o600))
	newProvider := func(sourceKey, path string) *captureTestProvider {
		return &captureTestProvider{
			Def: parser.AgentDef{Type: parser.AgentClaude},
			Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
				Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles,
				Append: parser.RawCaptureAppendOne, Snapshot: parser.RawCaptureSnapshotNone,
			}},
			plan: parser.RawCapturePlan{
				ConfiguredRoot: root, CaptureRoot: root, SourceKey: sourceKey,
				Entries: []parser.RawCaptureEntry{{
					Path: filepath.Base(path), LocalPath: path, Appendable: true,
				}},
			},
		}
	}
	largeSource := parser.SourceRef{Provider: parser.AgentClaude, Key: "large"}
	smallSource := parser.SourceRef{Provider: parser.AgentClaude, Key: "small"}

	degraded, err := New(store).Capture(
		t.Context(), newProvider("large", largePath), largeSource,
	)
	require.NoError(t, err)
	require.Equal(t, StatusDegraded, degraded.Status)
	captured, err := New(store).Capture(
		t.Context(), newProvider("small", smallPath), smallSource,
	)
	require.NoError(t, err)
	require.Equal(t, StatusCaptured, captured.Status)

	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, degraded.Source.ConfiguredRootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)
	assert.Equal(t, "outbox_full", coverage.Reason)
}

func TestUnchangedSourcesClearOnlyTheirOwnCoverageFailures(t *testing.T) {
	const maxBytes = int64(1 << 20)
	store, _ := openCapturerTestStore(t, maxBytes)
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.jsonl")
	secondPath := filepath.Join(root, "second.jsonl")
	require.NoError(t, os.WriteFile(firstPath, []byte("first\n"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("second\n"), 0o600))
	newProvider := func(sourceKey, path string) *captureTestProvider {
		return &captureTestProvider{
			Def: parser.AgentDef{Type: parser.AgentClaude},
			Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
				Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeFiles,
				Append: parser.RawCaptureAppendOne, Snapshot: parser.RawCaptureSnapshotNone,
			}},
			plan: parser.RawCapturePlan{
				ConfiguredRoot: root, CaptureRoot: root, SourceKey: sourceKey,
				Entries: []parser.RawCaptureEntry{{
					Path: filepath.Base(path), LocalPath: path, Appendable: true,
				}},
			},
		}
	}
	firstProvider := newProvider("first", firstPath)
	secondProvider := newProvider("second", secondPath)
	firstSource := parser.SourceRef{Provider: parser.AgentClaude, Key: "first"}
	secondSource := parser.SourceRef{Provider: parser.AgentClaude, Key: "second"}
	first, err := New(store).Capture(t.Context(), firstProvider, firstSource)
	require.NoError(t, err)
	second, err := New(store).Capture(t.Context(), secondProvider, secondSource)
	require.NoError(t, err)
	_, err = store.ReserveSourceCapture(t.Context(), first.Source, maxBytes)
	require.ErrorIs(t, err, rawcheckpoint.ErrOutboxFull)
	_, err = store.ReserveSourceCapture(t.Context(), second.Source, maxBytes)
	require.ErrorIs(t, err, rawcheckpoint.ErrOutboxFull)

	firstRetry, err := New(store).Capture(t.Context(), firstProvider, firstSource)

	require.NoError(t, err)
	require.Equal(t, StatusUnchanged, firstRetry.Status)
	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, first.Source.ConfiguredRootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageDegraded, coverage.State)

	secondRetry, err := New(store).Capture(t.Context(), secondProvider, secondSource)

	require.NoError(t, err)
	require.Equal(t, StatusUnchanged, secondRetry.Status)
	coverage, ok, err = store.Coverage(
		t.Context(), parser.AgentClaude, second.Source.ConfiguredRootID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rawcheckpoint.CoverageComplete, coverage.State)
}

func TestCapturerReservesOnlyVerifiedSuffixForAcknowledgedLargeSource(t *testing.T) {
	baseDir := t.TempDir()
	checkpointPath := filepath.Join(baseDir, "checkpoint.db")
	spoolDir := filepath.Join(baseDir, "spool")
	store, err := rawcheckpoint.OpenWithOptions(t.Context(), checkpointPath,
		rawcheckpoint.Options{SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider, source, sourcePath := captureFileProvider(t, strings.Repeat("x", 4096))
	first, err := New(store).Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	manifest, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, ok)
	commit := rawsync.CommitResult{
		ManifestID: strings.Repeat("a", 64), Receipt: strings.Repeat("b", 64),
		Generation: 1, Created: true,
	}
	require.NoError(t, store.BindFinalizedCommit(
		t.Context(), "device-a", manifest.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", manifest.CaptureID, commit)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = rawcheckpoint.OpenWithOptions(t.Context(), checkpointPath,
		rawcheckpoint.Options{SpoolDir: spoolDir, MaxOutboxBytes: 2049})
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "y"))

	result, err := New(store).Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, result.Status)
	assert.NotEqual(t, first.CaptureID, result.CaptureID)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2049), usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerRequiresFullReservationAfterAppendVerificationFallsBack(t *testing.T) {
	const maxBytes = int64(1 << 20)
	store, _ := openCapturerTestStore(t, maxBytes)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	const provisionalBytes = int64(1792)
	_, err = store.ReserveCapture(
		t.Context(), first.Source.ConfiguredRootID,
		maxBytes-usage.UsedBytes-provisionalBytes,
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sourcePath, []byte("two\n"), 0o600))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, result.Status)
	base, ok, err := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, base.CaptureID)
}

func TestCapturerDoesNotReturnDegradedStatusWithCleanupError(t *testing.T) {
	const maxBytes = int64(1 << 20)
	store, _ := openCapturerTestStore(t, maxBytes)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	const provisionalBytes = int64(1792)
	_, err = store.ReserveCapture(
		t.Context(), first.Source.ConfiguredRootID,
		maxBytes-usage.UsedBytes-provisionalBytes,
	)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sourcePath, []byte("two\n"), 0o600))
	cleanupErr := errors.New("injected cleanup failure")
	capturer.discardObjects = func(context.Context, []rawsync.ObjectRef) error {
		return cleanupErr
	}

	result, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, cleanupErr)
	assert.Empty(t, result)
}

func TestCapturerPreservesAndSanitizesObservationIOErrors(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.files.stat = func(path string) (os.FileInfo, error) {
		if path == sourcePath {
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.EIO}
		}
		return os.Stat(path)
	}

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, syscall.EIO)
	assert.NotErrorIs(t, err, ErrSourceChanged)
	assert.NotContains(t, err.Error(), sourcePath)
	assert.Contains(t, err.Error(), `project/session.jsonl`)
}

func TestAssessCaptureSanitizesOpenFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))
	file, err := os.Open(path)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	identity := stableFileIdentity(file, info)
	require.NotEmpty(t, identity)
	require.NoError(t, file.Close())
	observed := []observedCaptureEntry{{
		planned: parser.RawCaptureEntry{Path: "session.jsonl", Appendable: true},
		file:    file, info: info, identity: identity,
	}}
	base := rawcheckpoint.CaptureBaseState{Entries: []rawcheckpoint.CapturedEntry{{
		Path: "session.jsonl", Length: info.Size(), FileIdentity: identity,
		PrefixSHA256: strings.Repeat("a", 64), Appendable: true,
	}}}

	_, err = (&Capturer{}).assessCapture(t.Context(), observed, info.Size(), base, true)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), path)
	assert.Contains(t, err.Error(), `session.jsonl`)
}

func TestCapturerRejectsMutationDuringSuffixRead(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "two\n"))
	capturer.capturePhase = func(phase capturePhase, _ string) {
		if phase == capturePhaseAfterRead {
			require.NoError(t, appendFile(sourcePath, "three\n"))
		}
	}

	_, err = capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, ErrSourceChanged)
	base, ok, readErr := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, readErr)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, base.CaptureID)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCapturerRollsOverBeforeProspectiveObjectLimit(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.manifestLimits = rawsync.DefaultManifestLimits()
	capturer.manifestLimits.MaxObjects = 2
	_, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.NoError(t, appendFile(sourcePath, "two\n"))
	second, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	secondBase, ok, err := store.CaptureBase(t.Context(), second.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, secondBase.Entries, 1)
	assert.Len(t, secondBase.Entries[0].Objects, 2)
	require.NoError(t, appendFile(sourcePath, "three\n"))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	assert.Len(t, base.Entries[0].Objects, 1)
	assert.Equal(t, int64(14), base.Entries[0].Objects[0].Length)
}

func TestCapturerRollsOverBeforeProspectiveCanonicalLimit(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, sourcePath := captureFileProvider(t, "one\n")
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, err)
	require.True(t, ok)
	fullEntry := cloneCapturedEntry(base.Entries[0])
	fullEntry.Length = 8
	fullEntry.Objects = []rawsync.ObjectRef{placeholderObjectRef(0, 8)}
	appendEntry := cloneCapturedEntry(fullEntry)
	appendEntry.Objects = []rawsync.ObjectRef{
		base.Entries[0].Objects[0], placeholderObjectRef(1, 4),
	}
	limits := rawsync.DefaultManifestLimits()
	limits.MaxCanonicalBytes = uploadCanonicalBytes(
		t, first.Source, []rawcheckpoint.CapturedEntry{fullEntry},
	) + len(".999999999")
	require.Greater(t,
		uploadCanonicalBytes(t, first.Source, []rawcheckpoint.CapturedEntry{appendEntry}),
		limits.MaxCanonicalBytes,
	)
	capturer.manifestLimits = limits
	require.NoError(t, appendFile(sourcePath, "two\n"))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	base, ok, err = store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	assert.Len(t, base.Entries[0].Objects, 1)
	assert.Equal(t, int64(8), base.Entries[0].Objects[0].Length)
}

func TestCapturerRejectsFullReplacementOutsideManifestLimits(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	provider, source, _ := captureFileProvider(t, "one\n")
	capturer := New(store)
	capturer.manifestLimits = rawsync.DefaultManifestLimits()
	capturer.manifestLimits.MaxFileBytes = 3

	_, err := capturer.Capture(t.Context(), provider, source)

	require.ErrorIs(t, err, rawsync.ErrInvalid)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func uploadCanonicalBytes(
	t *testing.T,
	source rawcheckpoint.SourceIdentity,
	entries []rawcheckpoint.CapturedEntry,
) int {
	t.Helper()
	manifestEntries := make([]rawsync.Entry, 0, len(entries))
	for _, entry := range entries {
		manifestEntries = append(manifestEntries, rawsync.Entry{
			Path: entry.Path, Type: "file", Length: entry.Length,
			Objects: append([]rawsync.ObjectRef(nil), entry.Objects...),
		})
	}
	identity, err := rawsync.NewAuthIdentity(strings.Repeat(`"`, 128), strings.Repeat(`"`, 128))
	require.NoError(t, err)
	canonical, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:         rawsync.ManifestSchemaVersion,
		Provider:              source.Provider,
		ConfiguredRootID:      source.ConfiguredRootID,
		SourceKey:             source.SourceKey,
		ExpectedParentReceipt: strings.Repeat("0", 64),
		CaptureID:             strings.Repeat("c", 32),
		CapturedAt:            time.Date(2026, 8, 25, 12, 0, 0, 999999999, time.UTC),
		Kind:                  rawsync.ManifestSnapshot,
		Entries:               manifestEntries,
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return len(canonical.CanonicalJSON)
}

func appendFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
