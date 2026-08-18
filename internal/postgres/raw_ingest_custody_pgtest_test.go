//go:build pgtest

package postgres

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawCustodyEndToEnd(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	metadata, err := NewRawIngestStore(pg)
	require.NoError(t, err)
	service, err := rawsync.NewService(
		objects, metadata, rawsync.DefaultManifestLimits(), "parser-data-17",
	)
	require.NoError(t, err)

	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	firstBody := []byte("{\"type\":\"user\"}\n")
	secondBody := []byte("{\"type\":\"assistant\"}\n")
	firstRef := rawCustodyObjectRef(t, firstBody)
	secondRef := rawCustodyObjectRef(t, secondBody)
	missingManifest := rawCustodyManifest(
		"capture-missing", "", rawIngestCapturedAt(), firstRef,
	)
	_, err = service.CommitManifest(t.Context(), identity, missingManifest)
	assert.ErrorIs(t, err, rawsync.ErrMissingObject)
	assert.Equal(t, rawIngestCounts{}, readRawIngestCounts(t, pg))

	missing, err := service.MissingObjects(
		t.Context(), identity, parser.AgentCodex, []rawsync.ObjectRef{firstRef, secondRef},
	)
	require.NoError(t, err)
	assert.Equal(t, []rawsync.ObjectRef{firstRef, secondRef}, missing)
	firstPut, err := service.FinalizeObject(
		t.Context(), identity, parser.AgentCodex, firstRef, bytes.NewReader(firstBody),
	)
	require.NoError(t, err)
	assert.True(t, firstPut.Created)
	firstRetry, err := service.FinalizeObject(
		t.Context(), identity, parser.AgentCodex, firstRef, bytes.NewReader(firstBody),
	)
	require.NoError(t, err)
	assert.False(t, firstRetry.Created)

	firstManifest := rawCustodyManifest(
		"capture-a", "", rawIngestCapturedAt(), firstRef,
	)
	firstCanonical, err := rawsync.ValidateAndCanonicalize(
		identity, firstManifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	firstAccepted, err := service.CommitManifest(t.Context(), identity, firstManifest)
	require.NoError(t, err)
	assert.True(t, firstAccepted.Created)
	assert.Equal(t, int64(1), firstAccepted.Generation)

	firstAcceptedRetry, err := service.CommitManifest(t.Context(), identity, firstManifest)
	require.NoError(t, err)
	assert.False(t, firstAcceptedRetry.Created)
	assert.Equal(t, firstAccepted.Receipt, firstAcceptedRetry.Receipt)
	assert.Equal(t, firstAccepted.Generation, firstAcceptedRetry.Generation)

	_, err = service.FinalizeObject(
		t.Context(), identity, parser.AgentCodex, secondRef, bytes.NewReader(secondBody),
	)
	require.NoError(t, err)
	secondManifest := rawCustodyManifest(
		"capture-b", firstAccepted.Receipt,
		rawIngestCapturedAt().Add(time.Minute), firstRef, secondRef,
	)
	secondCanonical, err := rawsync.ValidateAndCanonicalize(
		identity, secondManifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	secondAccepted, err := service.CommitManifest(t.Context(), identity, secondManifest)
	require.NoError(t, err)
	assert.True(t, secondAccepted.Created)
	assert.Equal(t, int64(2), secondAccepted.Generation)

	assert.Equal(t,
		rawIngestCounts{Manifests: 2, Entries: 2, Objects: 3, Heads: 1, Jobs: 2},
		readRawIngestCounts(t, pg),
	)
	assert.Equal(t, 2, rawIngestTableCount(t, pg, "raw_objects"))
	var storedCanonical []byte
	var storedParent, storedCapture, storedKind string
	var storedGeneration int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT canonical_json, parent_receipt, capture_id, kind, generation
		FROM raw_manifests
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, secondCanonical.ManifestID,
	).Scan(
		&storedCanonical, &storedParent, &storedCapture, &storedKind, &storedGeneration,
	))
	assert.Equal(t, secondCanonical.CanonicalJSON, storedCanonical)
	assert.Equal(t, firstAccepted.Receipt, storedParent)
	assert.Equal(t, "capture-b", storedCapture)
	assert.Equal(t, "snapshot", storedKind)
	assert.Equal(t, int64(2), storedGeneration)

	var exactJobs int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT count(*) FROM raw_ingest_jobs
		WHERE stage = 'parse' AND state = 'ready'
			AND processing_version = 'parser-data-17'`,
	).Scan(&exactJobs))
	assert.Equal(t, 2, exactJobs)

	var storedPath, storedEntryType string
	var storedLength int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT path, entry_type, size_bytes
		FROM raw_manifest_entries
		WHERE tenant_id = $1 AND manifest_id = $2 AND entry_index = 0`,
		identity.TenantID, secondCanonical.ManifestID,
	).Scan(&storedPath, &storedEntryType, &storedLength))
	assert.Equal(t, "session.jsonl", storedPath)
	assert.Equal(t, "file", storedEntryType)
	assert.Equal(t, firstRef.Length+secondRef.Length, storedLength)

	rows, err := pg.QueryContext(t.Context(), `
		SELECT sha256, size_bytes FROM raw_manifest_objects
		WHERE tenant_id = $1 AND manifest_id = $2
		ORDER BY entry_index, object_index`,
		identity.TenantID, secondCanonical.ManifestID,
	)
	require.NoError(t, err)
	storedObjects := make([]rawsync.ObjectRef, 0, 2)
	for rows.Next() {
		var object rawsync.ObjectRef
		require.NoError(t, rows.Scan(&object.SHA256, &object.Length))
		storedObjects = append(storedObjects, object)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, []rawsync.ObjectRef{firstRef, secondRef}, storedObjects)

	assert.Equal(t, firstBody, readRawCustodyObject(t, objects, identity, firstRef))
	assert.Equal(t, secondBody, readRawCustodyObject(t, objects, identity, secondRef))
	assert.Equal(t, firstCanonical.CanonicalJSON,
		readRawCustodyManifest(t, objects, identity, firstCanonical.ManifestID))
	assert.Equal(t, secondCanonical.CanonicalJSON,
		readRawCustodyManifest(t, objects, identity, secondCanonical.ManifestID))

	staleManifest := rawCustodyManifest(
		"capture-c", firstAccepted.Receipt,
		rawIngestCapturedAt().Add(2*time.Minute), secondRef,
	)
	staleCanonical, err := rawsync.ValidateAndCanonicalize(
		identity, staleManifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	_, err = service.CommitManifest(t.Context(), identity, staleManifest)
	var headConflict *rawsync.HeadConflictError
	require.ErrorAs(t, err, &headConflict)
	assert.ErrorIs(t, err, rawsync.ErrConflict)
	assert.Equal(t, secondAccepted.Receipt, headConflict.CurrentReceipt)
	assert.Equal(t, int64(2), headConflict.CurrentGeneration)
	assert.Equal(t,
		rawIngestCounts{Manifests: 2, Entries: 2, Objects: 3, Heads: 1, Jobs: 2},
		readRawIngestCounts(t, pg),
	)
	assert.Equal(t, staleCanonical.CanonicalJSON,
		readRawCustodyManifest(t, objects, identity, staleCanonical.ManifestID),
		"a finalized manifest remains available for reconciliation after a head conflict")

	var headManifest, headReceipt string
	var headGeneration int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT manifest_id, receipt, generation FROM raw_source_heads`,
	).Scan(&headManifest, &headReceipt, &headGeneration))
	assert.Equal(t, secondCanonical.ManifestID, headManifest)
	assert.Equal(t, secondAccepted.Receipt, headReceipt)
	assert.Equal(t, int64(2), headGeneration)
}

func rawCustodyObjectRef(t *testing.T, body []byte) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256(body)
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(body)))
	require.NoError(t, err)
	return object
}

func rawCustodyManifest(
	captureID string,
	parentReceipt string,
	capturedAt time.Time,
	objects ...rawsync.ObjectRef,
) rawsync.Manifest {
	var length int64
	for _, object := range objects {
		length += object.Length
	}
	return rawsync.Manifest{
		SchemaVersion:         rawsync.ManifestSchemaVersion,
		Provider:              parser.AgentCodex,
		ConfiguredRootID:      "root-a",
		SourceKey:             "sessions/demo.jsonl#main",
		ExpectedParentReceipt: parentReceipt,
		CaptureID:             captureID,
		CapturedAt:            capturedAt,
		Kind:                  rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path: "session.jsonl", Type: "file", Length: length, Objects: objects,
		}},
	}
}

func readRawCustodyObject(
	t *testing.T,
	objects rawsync.ObjectStore,
	identity rawsync.AuthIdentity,
	object rawsync.ObjectRef,
) []byte {
	t.Helper()
	info, reader, err := objects.OpenObject(t.Context(), identity.TenantID, object)
	require.NoError(t, err)
	require.Equal(t, object, info.Ref)
	return readVerifiedRawCustodyBytes(t, reader)
}

func readRawCustodyManifest(
	t *testing.T,
	objects rawsync.ObjectStore,
	identity rawsync.AuthIdentity,
	manifestID string,
) []byte {
	t.Helper()
	info, reader, err := objects.OpenManifest(t.Context(), identity, manifestID)
	require.NoError(t, err)
	require.Equal(t, manifestID, info.Ref.SHA256)
	return readVerifiedRawCustodyBytes(t, reader)
}

func readVerifiedRawCustodyBytes(
	t *testing.T,
	reader rawsync.VerifiedObjectReader,
) []byte {
	t.Helper()
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	return body
}
