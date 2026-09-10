package rawderive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestMaterializerReconstructsVerifiedReadOnlyTreeAndCleansUp(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	first := []byte("first ")
	second := []byte("second")
	firstRef := objectRefForBytes(t, first)
	secondRef := objectRefForBytes(t, second)
	identity, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path:   "nested/session.jsonl",
		Type:   "file",
		Length: int64(len(first) + len(second)),
		Objects: []rawsync.ObjectRef{
			firstRef,
			secondRef,
		},
	}})
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{
		firstRef:  first,
		secondRef: second,
	}}

	materialized, err := (Materializer{
		Store:         store,
		BaseDir:       baseDir,
		MaxTotalBytes: 1024,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	require.NotEmpty(t, materialized.Root())
	assert.Equal(t, []rawsync.ObjectRef{firstRef, secondRef}, store.opened)
	assert.Equal(t, identity.TenantID, store.tenantID)

	path, err := materialized.EntryPath("nested/session.jsonl")
	require.NoError(t, err)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, append(append([]byte(nil), first...), second...), contents)
	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), fileInfo.Mode().Perm()&0o222,
		"materialized files must not be writable")
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), dirInfo.Mode().Perm()&0o222,
		"materialized directories must not be writable")
	for _, reader := range store.readers {
		assert.True(t, reader.verified)
		assert.True(t, reader.closed)
	}

	require.NoError(t, materialized.Cleanup())
	_, err = os.Stat(materialized.Root())
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, materialized.Cleanup(), "cleanup must be idempotent")
}

func TestMaterializerRestoresSourceModTimeBeforeParsing(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	// Use 100 ns precision so the assertion is portable to Windows filesystems.
	sourceModTime := time.Date(2026, 8, 13, 11, 0, 0, 123456700, time.UTC)
	sourced := []byte("sourced")
	legacy := []byte("legacy")
	sourcedRef := objectRefForBytes(t, sourced)
	legacyRef := objectRefForBytes(t, legacy)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{
		{
			Path: "sourced.jsonl", Type: "file", Length: int64(len(sourced)),
			ModTimeNS: sourceModTime.UnixNano(),
			Objects:   []rawsync.ObjectRef{sourcedRef},
		},
		{
			Path: "legacy.jsonl", Type: "file", Length: int64(len(legacy)),
			Objects: []rawsync.ObjectRef{legacyRef},
		},
	})
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{
		sourcedRef: sourced,
		legacyRef:  legacy,
	}}

	materialized, err := (Materializer{
		Store: store, BaseDir: baseDir, MaxTotalBytes: 1024,
	}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })

	sourcedInfo, err := os.Stat(materialized.entries["sourced.jsonl"])
	require.NoError(t, err)
	assert.True(t, sourcedInfo.ModTime().Equal(sourceModTime),
		"entries with a captured mod time must be restored to it, not the worker clock")
	legacyInfo, err := os.Stat(materialized.entries["legacy.jsonl"])
	require.NoError(t, err)
	assert.True(t, legacyInfo.ModTime().Equal(manifest.Manifest.CapturedAt),
		"legacy entries without a captured mod time must be normalized to the capture time")
}

func TestMaterializerRejectsUnverifiedObjectsAndRemovesPartialTree(t *testing.T) {
	t.Parallel()
	data := []byte("session")
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: int64(len(data)),
		Objects: []rawsync.ObjectRef{object},
	}})
	verificationFailure := errors.New("digest mismatch")

	for _, tc := range []struct {
		name      string
		info      rawsync.ObjectRef
		data      []byte
		verifyErr error
	}{
		{name: "wrong identity", info: rawsync.ObjectRef{
			SHA256: object.SHA256, Length: object.Length + 1,
		}, data: data},
		{name: "short body", info: object, data: data[:len(data)-1]},
		{name: "long body", info: object, data: append(append([]byte(nil), data...), '!')},
		{name: "verification failure", info: object, data: data, verifyErr: verificationFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			baseDir := t.TempDir()
			store := &materializerStore{
				objects: map[rawsync.ObjectRef][]byte{object: tc.data},
				infos:   map[rawsync.ObjectRef]rawsync.ObjectRef{object: tc.info},
				verify:  map[rawsync.ObjectRef]error{object: tc.verifyErr},
			}
			_, err := (Materializer{
				Store: store, BaseDir: baseDir, MaxTotalBytes: 1024,
			}).Materialize(t.Context(), manifest)
			require.Error(t, err)
			entries, readErr := os.ReadDir(baseDir)
			require.NoError(t, readErr)
			assert.Empty(t, entries, "failed materialization must remove its private tree")
		})
	}
}

func TestMaterializerEnforcesAggregateLimitBeforeOpeningObjects(t *testing.T) {
	t.Parallel()
	data := []byte("session")
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: int64(len(data)),
		Objects: []rawsync.ObjectRef{object},
	}})
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: data}}

	_, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: int64(len(data) - 1),
	}).Materialize(t.Context(), manifest)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	assert.Empty(t, store.opened)
}

func TestMaterializeObjectCopyObservesCancellationInBoundedChunks(t *testing.T) {
	t.Parallel()
	chunk := bytes.Repeat([]byte("agentsview"), 512) // 4 KiB chunks
	total := int64(len(chunk)) * 48                  // ~192 KiB across many chunks
	data := bytes.Repeat(chunk, 48)
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: total,
		Objects: []rawsync.ObjectRef{object},
	}})
	// The reader ignores the context on purpose: only the copy loop's own
	// bounded-chunk observation may stop it.
	store := &materializerStore{
		objects: map[rawsync.ObjectRef][]byte{object: data},
		readerFactory: func(ctx context.Context, data []byte) io.Reader {
			return &pacedReader{ctx: ctx, data: data, chunkSize: len(chunk), delay: 15 * time.Millisecond}
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(ctx, manifest)

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a stalled object stream must stop materialization at a chunk boundary")
	assert.Less(t, time.Since(started), 2*time.Second,
		"cancellation must be observed between bounded copy chunks")
}

func TestMaterializeTrailingProbeObservesCancellation(t *testing.T) {
	t.Parallel()
	data := []byte("session-body")
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: int64(len(data)),
		Objects: []rawsync.ObjectRef{object},
	}})
	// The body arrives immediately; the first extra read (the one-byte
	// trailing probe) stalls until the context is done. The probe must
	// observe the context instead of blocking on the stalled stream.
	store := &materializerStore{
		objects: map[rawsync.ObjectRef][]byte{object: data},
		readerFactory: func(ctx context.Context, data []byte) io.Reader {
			return &probeStallReader{ctx: ctx, data: data}
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()

	_, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
	}).Materialize(ctx, manifest)

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the trailing length probe must observe cancellation")
	assert.Less(t, time.Since(started), 2*time.Second)
}

// pacedReader yields fixed-size chunks after a delay and deliberately ignores
// its context, so only explicit bounded-chunk context observation stops it.
type pacedReader struct {
	ctx       context.Context
	data      []byte
	chunkSize int
	delay     time.Duration
	offset    int
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	size := min(len(p), r.chunkSize, len(r.data)-r.offset)
	n := copy(p, r.data[r.offset:r.offset+size])
	r.offset += n
	return n, nil
}

// probeStallReader serves its whole body immediately, then blocks the next
// read until the context is done.
type probeStallReader struct {
	ctx    context.Context
	data   []byte
	offset int
}

func (r *probeStallReader) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestMaterializerRejectsConflictingEntryPathsBeforeCreatingFiles(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	data := []byte("session")
	object := objectRefForBytes(t, data)
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)

	// Forge the exact envelope a binary without the path-collision check
	// would have persisted: "a" is a file while "a/b" needs "a" as a
	// directory, so the manifest is internally consistent yet unmaterializable.
	captiveManifest := rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         "codex",
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/demo.jsonl#main",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{
			{Path: "a", Type: "file", Length: int64(len(data)),
				Objects: []rawsync.ObjectRef{object}},
			{Path: "a/b", Type: "file", Length: int64(len(data)),
				Objects: []rawsync.ObjectRef{object}},
		},
	}
	canonicalJSON := []byte(fmt.Sprintf(
		`{"schema_version":1,"tenant_id":"tenant-a","device_id":"device-a","provider":"codex","configured_root_id":"root-a","source_key":"sessions/demo.jsonl#main","capture_id":"capture-a","captured_at":"2026-08-13T12:00:00Z","kind":"snapshot","entries":[{"path":"a","type":"file","length":7,"objects":[{"sha256":%q,"length":7}]},{"path":"a/b","type":"file","length":7,"objects":[{"sha256":%q,"length":7}]}]}`+"\n",
		object.SHA256, object.SHA256,
	))
	digest := sha256.Sum256(canonicalJSON)
	conflicted := rawsync.CanonicalManifest{
		Identity:      identity,
		Manifest:      captiveManifest,
		ManifestID:    hex.EncodeToString(digest[:]),
		CanonicalJSON: canonicalJSON,
		Objects:       []rawsync.ObjectRef{object},
	}
	store := &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: data}}

	materialized, err := (Materializer{
		Store: store, BaseDir: baseDir, MaxTotalBytes: 1024,
	}).Materialize(t.Context(), conflicted)

	require.Error(t, err)
	assert.ErrorIs(t, err, rawsync.ErrInvalid,
		"conflicting paths must fail validation, not filesystem materialization")
	assert.Nil(t, materialized)
	assert.Empty(t, store.opened,
		"conflicting manifests must be rejected before any object is opened")
	entries, readErr := os.ReadDir(baseDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries,
		"no materialization tree may be created for conflicting paths")
}

func TestMaterializeDelegatesInFlightCancellationToObjectStore(t *testing.T) {
	t.Parallel()
	data := []byte("session-body")
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: int64(len(data)),
		Objects: []rawsync.ObjectRef{object},
	}})
	copyStarted := make(chan struct{})
	store := &materializerStore{
		objects: map[rawsync.ObjectRef][]byte{object: data},
		copyObject: func(ctx context.Context, _ []byte, _ io.Writer) error {
			close(copyStarted)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (Materializer{
			Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
		}).Materialize(ctx, manifest)
		done <- err
	}()
	select {
	case <-copyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the materializer never started copying the object")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled,
			"the materializer must pass attempt cancellation into the store-owned copy")
	case <-time.After(5 * time.Second):
		t.Fatal("the materialized copy never observed cancellation")
	}
}

func TestMaterializationCleanupIsRetryableAfterTransientFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	transient := errors.New("transient removal failure")
	attempts := 0
	materialized := &Materialization{
		root:    root,
		entries: map[string]string{},
		removeTree: func(path string) error {
			attempts++
			if attempts == 1 {
				return transient
			}
			return os.RemoveAll(path)
		},
	}

	firstErr := materialized.Cleanup()

	require.ErrorIs(t, firstErr, transient,
		"a failed cleanup must stay reportable")
	assert.NotContains(t, firstErr.Error(), root,
		"cleanup errors must not expose the raw tree path")
	_, statErr := os.Stat(root)
	require.NoError(t, statErr, "a failed removal must leave the tree in place")

	require.NoError(t, materialized.Cleanup(),
		"a transient cleanup failure must be retryable, not latched")
	_, statErr = os.Stat(root)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	require.NoError(t, materialized.Cleanup(), "successful cleanup stays idempotent")
}

func TestMaterializeJoinsPartialCleanupFailureIntoOperationError(t *testing.T) {
	t.Parallel()
	data := []byte("session")
	object := objectRefForBytes(t, data)
	_, manifest := materializerTestManifest(t, []rawsync.Entry{{
		Path: "session.jsonl", Type: "file", Length: int64(len(data)),
		Objects: []rawsync.ObjectRef{object},
	}})
	cleanupFailure := errors.New("scratch removal failed")
	store := &materializerStore{
		objects: map[rawsync.ObjectRef][]byte{object: data},
		verify:  map[rawsync.ObjectRef]error{object: errors.New("digest mismatch")},
	}

	_, err := (Materializer{
		Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1024,
		removeTree: func(string) error { return cleanupFailure },
	}).Materialize(t.Context(), manifest)

	require.Error(t, err)
	assert.ErrorIs(t, err, cleanupFailure,
		"partial-materialization cleanup failures must join the operation error")
	assert.NotContains(t, err.Error(), "agentsview-raw",
		"the joined error must not expose the raw tree path")
}

type materializerStore struct {
	objects       map[rawsync.ObjectRef][]byte
	infos         map[rawsync.ObjectRef]rawsync.ObjectRef
	verify        map[rawsync.ObjectRef]error
	readerFactory func(context.Context, []byte) io.Reader
	copyObject    func(context.Context, []byte, io.Writer) error
	tenantID      string
	opened        []rawsync.ObjectRef
	readers       []*testVerifiedReader
}

func (s *materializerStore) CopyObject(
	ctx context.Context,
	tenantID string,
	object rawsync.ObjectRef,
	destination io.Writer,
) (rawsync.ObjectInfo, error) {
	s.tenantID = tenantID
	s.opened = append(s.opened, object)
	data, ok := s.objects[object]
	if !ok {
		return rawsync.ObjectInfo{}, rawsync.ErrNotFound
	}
	info := object
	if replacement, ok := s.infos[object]; ok {
		info = replacement
	}
	reader := &testVerifiedReader{
		Reader:    bytes.NewReader(data),
		verifyErr: s.verify[object],
	}
	if s.readerFactory != nil {
		reader = &testVerifiedReader{
			Reader:    s.readerFactory(ctx, data),
			verifyErr: s.verify[object],
		}
	}
	s.readers = append(s.readers, reader)
	defer func() { _ = reader.Close() }()
	if s.copyObject != nil {
		if err := s.copyObject(ctx, data, destination); err != nil {
			return rawsync.ObjectInfo{}, err
		}
		return rawsync.ObjectInfo{Ref: info}, nil
	}
	stream := materializerContextReader{ctx: ctx, reader: reader}
	if _, err := io.CopyN(destination, stream, object.Length); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	extra, err := io.Copy(io.Discard, io.LimitReader(stream, 1))
	if err != nil {
		return rawsync.ObjectInfo{}, err
	}
	if extra != 0 {
		return rawsync.ObjectInfo{}, rawsync.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	if err := reader.Verify(); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	return rawsync.ObjectInfo{Ref: info}, nil
}

type materializerContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r materializerContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := r.reader.Read(p)
	if err != nil && r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	return n, err
}

func objectRefForBytes(t *testing.T, data []byte) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256(data)
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(data)))
	require.NoError(t, err)
	return object
}

func materializerTestManifest(
	t *testing.T,
	entries []rawsync.Entry,
) (rawsync.AuthIdentity, rawsync.CanonicalManifest) {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	canonical, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         "codex",
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/demo.jsonl#main",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             rawsync.ManifestSnapshot,
		Entries:          entries,
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return identity, canonical
}
