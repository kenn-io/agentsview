//go:build pgtest

package postgres

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawIngestStoreObjectRegistry(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	_, err := NewRawIngestStore(nil)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	identity := rawIngestIdentity(t, "tenant-a")
	otherTenant := rawIngestIdentity(t, "tenant-b")
	first := rawIngestObject(t, "a", 7)
	second := rawIngestObject(t, "b", 11)

	missing, err := store.MissingObjects(t.Context(), identity, []rawsync.ObjectRef{
		second, first, second,
	})
	require.NoError(t, err)
	assert.Equal(t, []rawsync.ObjectRef{second, first}, missing)

	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, first))
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, first))
	missing, err = store.MissingObjects(t.Context(), identity, []rawsync.ObjectRef{second, first})
	require.NoError(t, err)
	assert.Equal(t, []rawsync.ObjectRef{second}, missing)
	missing, err = store.MissingObjects(t.Context(), otherTenant, []rawsync.ObjectRef{first})
	require.NoError(t, err)
	assert.Equal(t, []rawsync.ObjectRef{first}, missing,
		"verified-object metadata must never deduplicate across tenants")

	conflictingLength := rawsync.ObjectRef{SHA256: first.SHA256, Length: first.Length + 1}
	err = store.RecordVerifiedObject(t.Context(), identity, conflictingLength)
	assert.ErrorIs(t, err, rawsync.ErrConflict)
	assert.Equal(t, 1, rawIngestTableCount(t, pg, "raw_objects"))
}

func TestRawIngestStoreBatchesVerifiedObjectRegistration(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	objects := make([]rawsync.ObjectRef, 0, rawIngestBatchRows+1)
	for i := range rawIngestBatchRows + 1 {
		sum := sha256.Sum256([]byte(fmt.Sprintf("verified-object-%03d", i)))
		object, err := rawsync.NewObjectRef(hex.EncodeToString(sum[:]), int64(i+1))
		require.NoError(t, err)
		objects = append(objects, object)
	}

	require.NoError(t, store.RecordVerifiedObjects(t.Context(), identity, objects))
	assert.Equal(t, len(objects), rawIngestTableCount(t, pg, "raw_objects"))
	require.NoError(t, store.RecordVerifiedObjects(t.Context(), identity, objects))
	assert.Equal(t, len(objects), rawIngestTableCount(t, pg, "raw_objects"))

	objects[len(objects)-1].Length++
	err := store.RecordVerifiedObjects(t.Context(), identity, objects)
	assert.ErrorIs(t, err, rawsync.ErrConflict)
}

func TestRawIngestStoreCommitIsAtomicFencedAndIdempotent(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	firstManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)

	first, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
	require.NoError(t, err)
	assert.True(t, first.Created)
	assert.Equal(t, firstManifest.ManifestID, first.ManifestID)
	assert.Equal(t, int64(1), first.Generation)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), first.Receipt)
	assert.Equal(t, rawIngestCounts{Manifests: 1, Entries: 1, Objects: 1, Heads: 1, Jobs: 1},
		readRawIngestCounts(t, pg))

	retried, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
	require.NoError(t, err)
	assert.False(t, retried.Created)
	assert.Equal(t, first.ManifestID, retried.ManifestID)
	assert.Equal(t, first.Receipt, retried.Receipt)
	assert.Equal(t, first.Generation, retried.Generation)
	assert.Equal(t, rawIngestCounts{Manifests: 1, Entries: 1, Objects: 1, Heads: 1, Jobs: 1},
		readRawIngestCounts(t, pg))

	reusedCapture := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt().Add(time.Second), object,
	)
	_, err = store.CommitManifest(t.Context(), reusedCapture, "parser-data-17")
	assert.ErrorIs(t, err, rawsync.ErrConflict)
	assert.Equal(t, rawIngestCounts{Manifests: 1, Entries: 1, Objects: 1, Heads: 1, Jobs: 1},
		readRawIngestCounts(t, pg))

	secondManifest := rawIngestManifest(
		t, identity, "capture-b", first.Receipt, rawIngestCapturedAt().Add(time.Minute), object,
	)
	second, err := store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
	require.NoError(t, err)
	assert.True(t, second.Created)
	assert.Equal(t, int64(2), second.Generation)
	assert.Equal(t, rawIngestCounts{Manifests: 2, Entries: 2, Objects: 2, Heads: 1, Jobs: 2},
		readRawIngestCounts(t, pg))

	staleManifest := rawIngestManifest(
		t, identity, "capture-c", first.Receipt, rawIngestCapturedAt().Add(2*time.Minute), object,
	)
	_, err = store.CommitManifest(t.Context(), staleManifest, "parser-data-17")
	var headConflict *rawsync.HeadConflictError
	require.ErrorAs(t, err, &headConflict)
	assert.ErrorIs(t, err, rawsync.ErrConflict)
	assert.Equal(t, second.ManifestID, headConflict.CurrentManifestID)
	assert.Equal(t, second.Receipt, headConflict.CurrentReceipt)
	assert.Equal(t, int64(2), headConflict.CurrentGeneration)
	assert.Equal(t, rawIngestCounts{Manifests: 2, Entries: 2, Objects: 2, Heads: 1, Jobs: 2},
		readRawIngestCounts(t, pg))

	var headManifest, headReceipt string
	var generation int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT manifest_id, receipt, generation FROM raw_source_heads`,
	).Scan(&headManifest, &headReceipt, &generation))
	assert.Equal(t, second.ManifestID, headManifest)
	assert.Equal(t, second.Receipt, headReceipt)
	assert.Equal(t, int64(2), generation)
}

func TestRawIngestStoreMissingObjectChangesNoAcceptanceState(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), rawIngestObject(t, "a", 7),
	)

	_, err := store.CommitManifest(t.Context(), manifest, " ")
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	assert.Equal(t, rawIngestCounts{}, readRawIngestCounts(t, pg))

	_, err = store.CommitManifest(t.Context(), manifest, "parser-data-17")
	assert.ErrorIs(t, err, rawsync.ErrMissingObject)
	assert.Equal(t, rawIngestCounts{}, readRawIngestCounts(t, pg))
}

func TestRawIngestStoreConcurrentIdenticalRetryConverges(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)

	start := make(chan struct{})
	outcomes := make(chan rawIngestOutcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
			outcomes <- rawIngestOutcome{result: result, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)

	results := make([]rawsync.CommitResult, 0, 2)
	for got := range outcomes {
		require.NoError(t, got.err)
		results = append(results, got.result)
	}
	require.Len(t, results, 2)
	assert.Equal(t, results[0].Receipt, results[1].Receipt)
	assert.Equal(t, int64(1), results[0].Generation)
	assert.Equal(t, int64(1), results[1].Generation)
	assert.NotEqual(t, results[0].Created, results[1].Created)
	assert.Equal(t, rawIngestCounts{Manifests: 1, Entries: 1, Objects: 1, Heads: 1, Jobs: 1},
		readRawIngestCounts(t, pg))
}

func TestRawIngestStoreBatchesManifestReferences(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	const objectCount = rawIngestBatchRows + 1
	objects := make([]rawsync.ObjectRef, 0, objectCount)
	for i := range objectCount {
		sum := sha256.Sum256([]byte(fmt.Sprintf("object-%03d", i)))
		object, err := rawsync.NewObjectRef(hex.EncodeToString(sum[:]), 1)
		require.NoError(t, err)
		require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
		objects = append(objects, object)
	}
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         parser.AgentCodex,
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/batched.jsonl",
		CaptureID:        "capture-a",
		CapturedAt:       rawIngestCapturedAt(),
		Kind:             rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path: "session.jsonl", Type: "file", Length: int64(objectCount), Objects: objects,
		}},
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)

	result, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)
	assert.True(t, result.Created)
	assert.Equal(t, objectCount, rawIngestTableCount(t, pg, "raw_manifest_objects"))
	var first, last string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT
			(SELECT sha256 FROM raw_manifest_objects WHERE object_index = 0),
			(SELECT sha256 FROM raw_manifest_objects WHERE object_index = $1)`,
		objectCount-1,
	).Scan(&first, &last))
	assert.Equal(t, objects[0].SHA256, first)
	assert.Equal(t, objects[objectCount-1].SHA256, last)
}

func TestRawIngestStoreJobFailureRollsBackManifestAndHead(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	_, err := pg.ExecContext(t.Context(), `
		CREATE FUNCTION reject_raw_ingest_job() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected raw ingest job failure';
		END;
		$$;
		CREATE TRIGGER reject_raw_ingest_job
		BEFORE INSERT ON raw_ingest_jobs
		FOR EACH ROW EXECUTE FUNCTION reject_raw_ingest_job()`)
	require.NoError(t, err)
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)

	_, err = store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.Error(t, err)
	assert.False(t, errors.Is(err, rawsync.ErrConflict))
	assert.Equal(t, rawIngestCounts{}, readRawIngestCounts(t, pg))
}

func TestRawIngestStoreConcurrentHeadAdvance(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	initialManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	initial, err := store.CommitManifest(t.Context(), initialManifest, "parser-data-17")
	require.NoError(t, err)

	candidates := []rawsync.CanonicalManifest{
		rawIngestManifest(t, identity, "capture-b", initial.Receipt, rawIngestCapturedAt().Add(time.Minute), object),
		rawIngestManifest(t, identity, "capture-c", initial.Receipt, rawIngestCapturedAt().Add(2*time.Minute), object),
	}
	start := make(chan struct{})
	outcomes := make(chan rawIngestOutcome, len(candidates))
	var workers sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := store.CommitManifest(t.Context(), candidate, "parser-data-17")
			outcomes <- rawIngestOutcome{result: result, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)

	var winners, conflicts int
	for got := range outcomes {
		if got.err == nil {
			winners++
			assert.Equal(t, int64(2), got.result.Generation)
			continue
		}
		var conflict *rawsync.HeadConflictError
		require.ErrorAs(t, got.err, &conflict)
		assert.Equal(t, int64(2), conflict.CurrentGeneration)
		conflicts++
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, rawIngestCounts{Manifests: 2, Entries: 2, Objects: 2, Heads: 1, Jobs: 2},
		readRawIngestCounts(t, pg))
}

type rawIngestOutcome struct {
	result rawsync.CommitResult
	err    error
}

func newRawIngestTestStore(t *testing.T) (*sql.DB, *RawIngestStore) {
	t.Helper()
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	store, err := NewRawIngestStore(pg)
	require.NoError(t, err)
	return pg, store
}

func rawIngestIdentity(t *testing.T, tenant string) rawsync.AuthIdentity {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity(tenant, "device-a")
	require.NoError(t, err)
	return identity
}

func rawIngestObject(t *testing.T, digit string, length int64) rawsync.ObjectRef {
	t.Helper()
	object, err := rawsync.NewObjectRef(repeatedHex(digit), length)
	require.NoError(t, err)
	return object
}

func rawIngestManifest(
	t *testing.T,
	identity rawsync.AuthIdentity,
	captureID string,
	parentReceipt string,
	capturedAt time.Time,
	object rawsync.ObjectRef,
) rawsync.CanonicalManifest {
	t.Helper()
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:         rawsync.ManifestSchemaVersion,
		Provider:              parser.AgentCodex,
		ConfiguredRootID:      "root-a",
		SourceKey:             "sessions/demo.jsonl#main",
		ExpectedParentReceipt: parentReceipt,
		CaptureID:             captureID,
		CapturedAt:            capturedAt,
		Kind:                  rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path:    "session.jsonl",
			Type:    "file",
			Length:  object.Length,
			Objects: []rawsync.ObjectRef{object},
		}},
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return manifest
}

func rawIngestCapturedAt() time.Time {
	return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
}

type rawIngestCounts struct {
	Manifests int
	Entries   int
	Objects   int
	Heads     int
	Jobs      int
}

func readRawIngestCounts(t *testing.T, pg *sql.DB) rawIngestCounts {
	t.Helper()
	var counts rawIngestCounts
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT
			(SELECT count(*) FROM raw_manifests),
			(SELECT count(*) FROM raw_manifest_entries),
			(SELECT count(*) FROM raw_manifest_objects),
			(SELECT count(*) FROM raw_source_heads),
			(SELECT count(*) FROM raw_ingest_jobs)`,
	).Scan(&counts.Manifests, &counts.Entries, &counts.Objects, &counts.Heads, &counts.Jobs))
	return counts
}

func rawIngestTableCount(t *testing.T, pg *sql.DB, table string) int {
	t.Helper()
	var count int
	require.NoError(t, pg.QueryRowContext(t.Context(),
		"SELECT count(*) FROM "+table,
	).Scan(&count))
	return count
}

func TestRawIngestStoreAcceptsMaximumLengthIncompressibleKeys(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	limits := rawsync.DefaultManifestLimits()
	sourceKey := rawIngestIncompressibleText(t, 1, 4096, sourceKeyAlphabet)
	entryPath := rawIngestIncompressibleText(t, 2, limits.MaxPathBytes, entryPathAlphabet)
	object := rawIngestObject(t, "a", 7)
	commit := func(identity rawsync.AuthIdentity, captureID, parent string) rawsync.CommitResult {
		t.Helper()
		require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
		manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
			SchemaVersion:         rawsync.ManifestSchemaVersion,
			Provider:              parser.AgentCodex,
			ConfiguredRootID:      "root-a",
			SourceKey:             sourceKey,
			ExpectedParentReceipt: parent,
			CaptureID:             captureID,
			CapturedAt:            rawIngestCapturedAt(),
			Kind:                  rawsync.ManifestSnapshot,
			Entries: []rawsync.Entry{{
				Path:    entryPath,
				Type:    "file",
				Length:  object.Length,
				Objects: []rawsync.ObjectRef{object},
			}},
		}, limits)
		require.NoError(t, err)
		result, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
		require.NoError(t, err)
		return result
	}

	identity := rawIngestIdentity(t, "tenant-a")
	first := commit(identity, "capture-a", "")
	assert.True(t, first.Created)
	second := commit(identity, "capture-b", first.Receipt)
	assert.Equal(t, int64(2), second.Generation)
	otherTenant := commit(rawIngestIdentity(t, "tenant-b"), "capture-a", "")
	assert.Equal(t, int64(1), otherTenant.Generation,
		"identical long source keys must stay independent across tenants")
	assert.Equal(t, rawIngestCounts{Manifests: 3, Entries: 3, Objects: 3, Heads: 2, Jobs: 3},
		readRawIngestCounts(t, pg))

	var storedSourceKey, storedPath string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT m.source_key, e.path
		FROM raw_manifests m
		JOIN raw_manifest_entries e USING (tenant_id, manifest_id)
		WHERE m.manifest_id = $1`, first.ManifestID,
	).Scan(&storedSourceKey, &storedPath))
	assert.Equal(t, sourceKey, storedSourceKey)
	assert.Equal(t, entryPath, storedPath)
}

const (
	sourceKeyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~ "
	entryPathAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"!\"#$%&'()*+,-;<=>?@[]^_`{|}~"
)

// rawIngestIncompressibleText returns deterministic pseudo-random text that
// pglz cannot shrink, so index entries carry its full byte length.
func rawIngestIncompressibleText(t *testing.T, seed uint64, length int, alphabet string) string {
	t.Helper()
	source := rand.New(rand.NewPCG(seed, seed+1))
	text := make([]byte, length)
	for i := range text {
		text[i] = alphabet[source.IntN(len(alphabet))]
	}
	return string(text)
}
