package rawupload

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

type uploadTransportStub struct {
	missingBatches [][]rawsync.ObjectRef
	uploaded       map[rawsync.ObjectRef]string
	commit         rawsync.CommitResult
	commitErr      error
	commitFunc     func(rawsync.Manifest) (rawsync.CommitResult, error)
	commitCalls    int
}

func (s *uploadTransportStub) MissingObjects(
	_ context.Context,
	_ parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	s.missingBatches = append(s.missingBatches, append([]rawsync.ObjectRef(nil), objects...))
	return append([]rawsync.ObjectRef(nil), objects...), nil
}

func (s *uploadTransportStub) UploadObject(
	_ context.Context,
	_ parser.AgentType,
	object rawsync.ObjectRef,
	content io.ReaderAt,
) error {
	data := make([]byte, object.Length)
	_, err := content.ReadAt(data, 0)
	if err != nil && err != io.EOF {
		return err
	}
	s.uploaded[object] = string(data)
	return nil
}

func (s *uploadTransportStub) CommitManifest(
	_ context.Context,
	manifest rawsync.Manifest,
) (rawsync.CommitResult, error) {
	s.commitCalls++
	if s.commitFunc != nil {
		return s.commitFunc(manifest)
	}
	return s.commit, s.commitErr
}

func TestUploaderRetriesPostCommitAcknowledgementWithoutRecommitting(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	base := t.TempDir()
	store, generation := queuedUploadTestGenerationAt(t, base, func() time.Time { return now })
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	commit := rawsync.CommitResult{
		ManifestID: strings.Repeat("a", 64),
		Receipt:    strings.Repeat("b", 64),
		Generation: 1,
		Created:    true,
	}
	checkpoint, err := sql.Open("sqlite3", filepath.Join(base, "checkpoint.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
	_, err = checkpoint.Exec(`CREATE TRIGGER fail_acknowledgement
		BEFORE UPDATE OF head_capture_id ON raw_sources
		BEGIN SELECT RAISE(FAIL, 'forced acknowledgement failure'); END`)
	require.NoError(t, err)

	transport := &uploadTransportStub{uploaded: make(map[rawsync.ObjectRef]string), commit: commit}
	uploader := New(store, transport, "device-a")
	uploader.now = func() time.Time { return now }
	_, uploaded, err := uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, uploaded)
	assert.Equal(t, 1, transport.commitCalls)

	_, uploaded, err = uploader.UploadNext(t.Context())
	require.NoError(t, err)
	assert.False(t, uploaded, "post-commit acknowledgement failures must honor backoff")
	assert.Equal(t, 1, transport.commitCalls)
	_, err = checkpoint.Exec(`DROP TRIGGER fail_acknowledgement`)
	require.NoError(t, err)

	now = now.Add(transientRetryDelay)
	result, uploaded, err := uploader.UploadNext(t.Context())

	require.NoError(t, err)
	require.True(t, uploaded)
	assert.Equal(t, 1, transport.commitCalls, "retry must use the durable commit result")
	assert.Equal(t, generation.CaptureID, result.CaptureID)
	assert.Equal(t, commit.ManifestID, result.ManifestID)
	assert.Equal(t, commit.Receipt, result.Receipt)
	assert.Equal(t, commit.Generation, result.Generation)
	head, ok, err := store.SourceHead(
		t.Context(), generation.Source.Provider, generation.Source.ConfiguredRootID,
		generation.Source.SourceKey,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, commit.ManifestID, head.ManifestID)
	assert.Equal(t, commit.Receipt, head.Receipt)
	assert.Equal(t, commit.Generation, head.Generation)
}

func TestUploaderClassifiesGatewayTimeoutByHTTPStatus(t *testing.T) {
	store, generation := queuedUploadTestGeneration(t)
	transport := &uploadTransportStub{
		uploaded: make(map[rawsync.ObjectRef]string),
		commitErr: &rawclient.APIError{
			Status:  http.StatusGatewayTimeout,
			Code:    rawclient.CodeInternal,
			Message: "request timed out",
		},
	}
	uploader := New(store, transport, "device-a")

	result, found, err := uploader.UploadNext(t.Context())

	require.Error(t, err)
	require.True(t, found)
	assert.Zero(t, result)
	_, found, retryErr := uploader.UploadNext(t.Context())
	require.NoError(t, retryErr)
	assert.False(t, found, "durable retry time must delay the finalized generation")
	base, ok, readErr := store.CaptureBase(t.Context(), generation.Source)
	require.NoError(t, readErr)
	require.True(t, ok)
	assert.Equal(t, generation.CaptureID, base.CaptureID)
}

func TestUploaderRetriesRecoverableClientErrors(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store, _ := queuedUploadTestGenerationWithNow(t, func() time.Time { return now })
	transport := &uploadTransportStub{
		uploaded: make(map[rawsync.ObjectRef]string),
		commitErr: &rawclient.APIError{
			Status:  http.StatusUnauthorized,
			Code:    rawclient.CodeUnauthorized,
			Message: "credential expired",
		},
	}
	uploader := New(store, transport, "device-a")
	uploader.now = func() time.Time { return now }

	_, found, err := uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)
	now = now.Add(2 * transientRetryDelay)
	transport.commitErr = nil
	transport.commit = rawsync.CommitResult{
		ManifestID: strings.Repeat("c", 64), Receipt: strings.Repeat("d", 64),
		Generation: 1, Created: true,
	}
	_, found, err = uploader.UploadNext(t.Context())
	require.NoError(t, err)
	assert.True(t, found, "corrected credentials must resume the source chain")
}

func TestUploaderExponentiallyBacksOffRepeatedTransientFailures(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store, _ := queuedUploadTestGenerationWithNow(t, func() time.Time { return now })
	transport := &uploadTransportStub{
		uploaded: make(map[rawsync.ObjectRef]string),
		commitErr: &rawclient.APIError{
			Status: http.StatusServiceUnavailable, Code: rawclient.CodeInternal,
		},
	}
	uploader := New(store, transport, "device-a")
	uploader.now = func() time.Time { return now }

	_, found, err := uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)
	now = now.Add(transientRetryDelay)
	_, found, err = uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)
	transport.commitErr = nil
	transport.commit = rawsync.CommitResult{
		ManifestID: strings.Repeat("c", 64), Receipt: strings.Repeat("d", 64),
		Generation: 1, Created: true,
	}
	now = now.Add(transientRetryDelay)
	_, found, err = uploader.UploadNext(t.Context())
	require.NoError(t, err)
	assert.False(t, found, "second transient failure must wait longer than the first")
	now = now.Add(transientRetryDelay)
	_, found, err = uploader.UploadNext(t.Context())
	require.NoError(t, err)
	assert.True(t, found)
}

func TestUploaderRecoversHeadConflictAfterTransientFailure(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store, generation := queuedUploadTestGenerationWithNow(t, func() time.Time { return now })
	reconciled := rawsync.CommitResult{
		ManifestID: strings.Repeat("a", 64), Receipt: strings.Repeat("b", 64),
		Generation: 1,
	}
	committed := rawsync.CommitResult{
		ManifestID: strings.Repeat("c", 64), Receipt: strings.Repeat("d", 64),
		Generation: 2, Created: true,
	}
	transport := &uploadTransportStub{uploaded: make(map[rawsync.ObjectRef]string)}
	transport.commitFunc = func(manifest rawsync.Manifest) (rawsync.CommitResult, error) {
		switch transport.commitCalls {
		case 1:
			return rawsync.CommitResult{}, &rawclient.APIError{
				Status: http.StatusServiceUnavailable, Code: rawclient.CodeInternal,
			}
		case 2:
			return rawsync.CommitResult{}, &rawclient.APIError{
				Status: http.StatusConflict, Code: rawclient.CodeHeadConflict,
				CurrentManifestID: reconciled.ManifestID,
				CurrentReceipt:    reconciled.Receipt,
				CurrentGeneration: reconciled.Generation,
			}
		default:
			assert.Equal(t, reconciled.Receipt, manifest.ExpectedParentReceipt)
			return committed, nil
		}
	}
	uploader := New(store, transport, "device-a")
	uploader.now = func() time.Time { return now }

	_, found, err := uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)
	now = now.Add(transientRetryDelay)
	_, found, err = uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)

	result, found, err := uploader.UploadNext(t.Context())

	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, generation.CaptureID, result.CaptureID)
	assert.Equal(t, committed.Receipt, result.Receipt)
	assert.Equal(t, 3, transport.commitCalls)
}

func TestUploaderBacksOffMissingLocalSpoolObject(t *testing.T) {
	store, generation := queuedUploadTestGeneration(t)
	ref := generation.Entries[0].Objects[0]
	require.NoError(t, os.Remove(store.ObjectPath(ref)))
	uploader := New(store, &uploadTransportStub{}, "device-a")

	_, found, err := uploader.UploadNext(t.Context())
	require.Error(t, err)
	require.True(t, found)
	_, found, err = uploader.UploadNext(t.Context())
	require.NoError(t, err)
	assert.False(t, found, "persistent local failures must not hot-loop")
}

func TestUploaderPersistsPermanentRejectionAndSuppressesRetry(t *testing.T) {
	baseDir := t.TempDir()
	store, generation := queuedUploadTestGenerationAt(t, baseDir, nil)
	transport := &uploadTransportStub{
		uploaded: make(map[rawsync.ObjectRef]string),
		commitErr: &rawclient.APIError{
			Status: http.StatusBadRequest, Code: rawclient.CodeInvalidRequest,
		},
	}

	_, found, err := New(store, transport, "device-a").UploadNext(t.Context())

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermanentFailure)
	require.True(t, found)
	require.NoError(t, store.Close())
	store, err = rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(baseDir, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(baseDir, "spool"), MaxOutboxBytes: 1 << 20,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	base, ok, err := store.CaptureBase(t.Context(), generation.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, generation.CaptureID, base.CaptureID)
	_, found, err = New(store, transport, "device-a").UploadNext(t.Context())
	require.NoError(t, err)
	assert.False(t, found)
	status, err := store.ClientStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.PermanentFailures)
}

func TestUploaderUploadsMissingObjectsBeforeAcknowledgingManifest(t *testing.T) {
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
	rootPath := t.TempDir()
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, rootPath)
	require.NoError(t, err)
	contents := []string{"first", "second"}
	refs := make([]rawsync.ObjectRef, 0, len(contents))
	for _, content := range contents {
		ref := objectRefFor(content)
		path := store.ObjectPath(ref)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		refs = append(refs, ref)
	}
	source := rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: root.ID, SourceKey: "source-1",
	}
	reservation, err := store.ReserveSourceCapture(
		t.Context(), source, int64(len(contents[0])+len(contents[1]))+
			rawcheckpoint.CaptureMetadataCharge(1, len(refs)),
	)
	require.NoError(t, err)
	generation := rawcheckpoint.CapturedGeneration{
		CaptureID: strings.Repeat("1", 32), Source: source,
		CapturedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		Kind:       rawsync.ManifestSnapshot,
		Entries: []rawcheckpoint.CapturedEntry{{
			Path: "session.jsonl", Length: int64(len(contents[0]) + len(contents[1])),
			FileIdentity: "device:1:inode:2", PrefixSHA256: refs[1].SHA256,
			Appendable: true, Objects: refs,
		}},
	}
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	transport := &uploadTransportStub{
		uploaded: make(map[rawsync.ObjectRef]string),
		commit: rawsync.CommitResult{
			ManifestID: strings.Repeat("a", 64),
			Receipt:    strings.Repeat("b", 64),
			Generation: 1,
			Created:    true,
		},
	}

	result, uploaded, err := New(store, transport, "device-a").UploadNext(t.Context())

	require.NoError(t, err)
	require.True(t, uploaded)
	assert.Equal(t, generation.CaptureID, result.CaptureID)
	assert.Equal(t, int64(len(contents[0])+len(contents[1])), result.UploadedBytes)
	assert.Equal(t, contents[0], transport.uploaded[refs[0]])
	assert.Equal(t, contents[1], transport.uploaded[refs[1]])
	head, ok, err := store.SourceHead(
		t.Context(), source.Provider, source.ConfiguredRootID, source.SourceKey,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, transport.commit.Receipt, head.Receipt)
	assert.NoFileExists(t, store.ObjectPath(refs[0]))
	assert.NoFileExists(t, store.ObjectPath(refs[1]))
}

func TestMissingObjectNegotiationUsesBoundedBatches(t *testing.T) {
	transport := &uploadTransportStub{}
	objects := make([]rawsync.ObjectRef, 5000)
	for i := range objects {
		objects[i] = rawsync.ObjectRef{SHA256: fmt.Sprintf("%064x", i+1), Length: int64(i)}
	}

	missing, err := missingObjects(
		t.Context(), transport, parser.AgentClaude, objects,
	)

	require.NoError(t, err)
	assert.Equal(t, objects, missing)
	require.Greater(t, len(transport.missingBatches), 1)
	for _, batch := range transport.missingBatches {
		assert.LessOrEqual(t, len(batch), missingObjectBatchSize)
	}
}

func objectRefFor(content string) rawsync.ObjectRef {
	digest := sha256.Sum256([]byte(content))
	ref, err := rawsync.NewObjectRef(fmt.Sprintf("%x", digest), int64(len(content)))
	if err != nil {
		panic(err)
	}
	return ref
}

func queuedUploadTestGeneration(
	t *testing.T,
) (*rawcheckpoint.Store, rawcheckpoint.CapturedGeneration) {
	return queuedUploadTestGenerationWithNow(t, nil)
}

func queuedUploadTestGenerationWithNow(
	t *testing.T,
	now func() time.Time,
) (*rawcheckpoint.Store, rawcheckpoint.CapturedGeneration) {
	t.Helper()
	base := t.TempDir()
	store, generation := queuedUploadTestGenerationAt(t, base, now)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, generation
}

func queuedUploadTestGenerationAt(
	t *testing.T,
	base string,
	now func() time.Time,
) (*rawcheckpoint.Store, rawcheckpoint.CapturedGeneration) {
	t.Helper()
	store, err := rawcheckpoint.OpenWithOptions(
		t.Context(), filepath.Join(base, "checkpoint.db"),
		rawcheckpoint.Options{
			SpoolDir: filepath.Join(base, "spool"), MaxOutboxBytes: 1 << 20, Now: now,
		},
	)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, t.TempDir())
	require.NoError(t, err)
	ref := objectRefFor("first")
	path := store.ObjectPath(ref)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
	source := rawcheckpoint.SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: root.ID, SourceKey: "source-1",
	}
	reservation, err := store.ReserveSourceCapture(
		t.Context(), source, ref.Length+rawcheckpoint.CaptureMetadataCharge(1, 1),
	)
	require.NoError(t, err)
	generation := rawcheckpoint.CapturedGeneration{
		CaptureID: strings.Repeat("2", 32), Source: source,
		CapturedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		Kind:       rawsync.ManifestSnapshot,
		Entries: []rawcheckpoint.CapturedEntry{{
			Path: "session.jsonl", Length: ref.Length,
			FileIdentity: "device:1:inode:2", PrefixSHA256: ref.SHA256,
			Appendable: true, Objects: []rawsync.ObjectRef{ref},
		}},
	}
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	return store, generation
}
