package rawsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestServiceConstructorRejectsInvalidDependenciesAndConfiguration(t *testing.T) {
	t.Parallel()

	objects := &recordingObjectStore{}
	metadata := &recordingMetadataStore{}
	limits := DefaultManifestLimits()
	var typedNilObjects *recordingObjectStore
	var typedNilMetadata *recordingMetadataStore

	tests := []struct {
		name       string
		objects    ObjectStore
		metadata   MetadataStore
		limits     ManifestLimits
		processing string
	}{
		{name: "nil object store", metadata: metadata, limits: limits, processing: "parser-data-17"},
		{name: "typed nil object store", objects: typedNilObjects, metadata: metadata, limits: limits, processing: "parser-data-17"},
		{name: "nil metadata store", objects: objects, limits: limits, processing: "parser-data-17"},
		{name: "typed nil metadata store", objects: objects, metadata: typedNilMetadata, limits: limits, processing: "parser-data-17"},
		{name: "invalid limits", objects: objects, metadata: metadata, limits: ManifestLimits{}, processing: "parser-data-17"},
		{name: "blank processing version", objects: objects, metadata: metadata, limits: limits},
		{name: "noncanonical processing version", objects: objects, metadata: metadata, limits: limits, processing: " parser-data-17"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service, err := NewService(tt.objects, tt.metadata, tt.limits, tt.processing)
			assert.Nil(t, service)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestServiceFinalizeObjectOrdersCustodyBeforeRegistration(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		object := serviceObject("body")
		objects := &recordingObjectStore{
			events: &events,
			putObjectResult: PutResult{
				Info: ObjectInfo{Ref: object}, Created: true,
			},
		}
		metadata := &recordingMetadataStore{events: &events}
		service := newTestService(t, objects, metadata)
		identity, _ := validServiceInput(t)

		result, err := service.FinalizeObject(
			t.Context(), identity, parser.AgentCodex, object, bytes.NewBufferString("body"),
		)
		require.NoError(t, err)
		assert.True(t, result.Created)
		assert.Equal(t, []string{"put-object", "record-object"}, events)
		assert.Equal(t, identity.TenantID, objects.putObjectTenant)
		assert.Equal(t, object, metadata.recordedObjects[0])
	})

	t.Run("physical write failure", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		putErr := errors.New("object backend unavailable")
		objects := &recordingObjectStore{events: &events, putObjectErr: putErr}
		metadata := &recordingMetadataStore{events: &events}
		service := newTestService(t, objects, metadata)
		identity, object := validServiceInput(t)

		_, err := service.FinalizeObject(
			t.Context(), identity, parser.AgentCodex, object.Objects[0], bytes.NewBufferString("body"),
		)
		assert.ErrorIs(t, err, putErr)
		assert.Equal(t, []string{"put-object"}, events)
	})

	t.Run("metadata failure leaves an unreferenced immutable object", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		metadataErr := errors.New("metadata backend unavailable")
		objects := &recordingObjectStore{events: &events}
		metadata := &recordingMetadataStore{events: &events, recordErr: metadataErr}
		service := newTestService(t, objects, metadata)
		identity, manifest := validServiceInput(t)

		_, err := service.FinalizeObject(
			t.Context(), identity, parser.AgentCodex, manifest.Objects[0], bytes.NewBufferString("body"),
		)
		assert.ErrorIs(t, err, metadataErr)
		assert.Equal(t, []string{"put-object", "record-object"}, events)
	})
}

func TestServiceRejectsExcludedProvidersBeforeCustody(t *testing.T) {
	t.Parallel()

	for _, provider := range []parser.AgentType{"", "not-an-agent", parser.AgentOmnigent, parser.AgentTrae} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			events := []string{}
			objects := &recordingObjectStore{events: &events}
			service := newTestService(t, objects, &recordingMetadataStore{events: &events})
			identity, manifest := validServiceInput(t)

			_, err := service.FinalizeObject(
				t.Context(), identity, provider, manifest.Objects[0], bytes.NewBufferString("body"),
			)
			assert.ErrorIs(t, err, ErrInvalid)
			_, err = service.MissingObjects(t.Context(), identity, provider, manifest.Objects)
			assert.ErrorIs(t, err, ErrInvalid)
			assert.Empty(t, events, "excluded providers must never reach custody")
		})
	}
}

func TestServiceRejectsObjectsLargerThanAnyManifestFile(t *testing.T) {
	t.Parallel()

	events := []string{}
	objects := &recordingObjectStore{events: &events}
	service := newTestService(t, objects, &recordingMetadataStore{events: &events})
	identity, _ := validServiceInput(t)
	oversized := ObjectRef{
		SHA256: strings.Repeat("c", 64), Length: DefaultManifestLimits().MaxFileBytes + 1,
	}

	_, err := service.FinalizeObject(
		t.Context(), identity, parser.AgentCodex, oversized, bytes.NewBufferString("body"),
	)
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = service.MissingObjects(t.Context(), identity, parser.AgentCodex, []ObjectRef{oversized})
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, events, "unreferenceable objects must never reach custody")
}

func TestServiceMissingObjectsUsesPhysicalCustody(t *testing.T) {
	t.Parallel()

	events := []string{}
	identity, manifest := validServiceInput(t)
	objects := &recordingObjectStore{
		events:  &events,
		missing: []ObjectRef{manifest.Objects[0]},
	}
	service := newTestService(t, objects, &recordingMetadataStore{events: &events})

	missing, err := service.MissingObjects(t.Context(), identity, parser.AgentCodex, manifest.Objects)
	require.NoError(t, err)
	assert.Equal(t, manifest.Objects, missing)
	assert.Equal(t, []string{"missing-objects"}, events)
	assert.Equal(t, identity.TenantID, objects.missingTenant)
}

func TestServiceCommitVerifiesEveryObjectBeforeMetadataCommit(t *testing.T) {
	t.Parallel()

	events := []string{}
	identity, manifest := validServiceInput(t)
	objects := &recordingObjectStore{events: &events}
	metadata := &recordingMetadataStore{
		events: &events,
		commitResult: CommitResult{
			ManifestID: manifest.ManifestID, Receipt: serviceObject("receipt").SHA256,
			Generation: 1, Created: true,
		},
	}
	service := newTestService(t, objects, metadata)

	result, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Receipt)
	assert.Equal(t, []string{
		"verify-objects", "record-objects", "put-manifest", "commit",
	}, events)
	assert.Equal(t, manifest.Objects, objects.verifiedObjects)
	require.Len(t, metadata.committed, 1)
	assert.Equal(t, manifest.ManifestID, metadata.committed[0].ManifestID)
	assert.Equal(t, manifest.CanonicalJSON, metadata.committed[0].CanonicalJSON)
	assert.Equal(t, "parser-data-17", metadata.processingVersion)
}

func TestServiceCommitUsesBoundedCallsAtMaximumObjectCardinality(t *testing.T) {
	t.Parallel()

	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	limits := DefaultManifestLimits()
	limits.MaxCanonicalBytes = 4 << 20
	objects := make([]ObjectRef, 0, limits.MaxObjects)
	for i := range limits.MaxObjects {
		digest := sha256.Sum256([]byte{byte(i >> 8), byte(i)})
		objects = append(objects, ObjectRef{
			SHA256: hex.EncodeToString(digest[:]), Length: 1,
		})
	}
	manifest := Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		Provider:         parser.AgentCodex,
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/maximum.jsonl",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             ManifestSnapshot,
		Entries: []Entry{{
			Path: "session.jsonl", Type: "file",
			Length: int64(len(objects)), Objects: objects,
		}},
	}
	canonical, err := ValidateAndCanonicalize(identity, manifest, limits)
	require.NoError(t, err)
	events := []string{}
	physical := &recordingObjectStore{events: &events}
	metadata := &recordingMetadataStore{events: &events}
	service, err := NewService(physical, metadata, limits, "parser-data-17")
	require.NoError(t, err)

	_, err = service.CommitManifest(t.Context(), identity, manifest)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"verify-objects", "record-objects", "put-manifest", "commit",
	}, events)
	assert.Equal(t, canonical.Objects, physical.verifiedObjects)
	assert.Len(t, metadata.recordedObjects, limits.MaxObjects)
}

func TestServiceCommitRejectsBeforeAcceptanceBoundary(t *testing.T) {
	t.Parallel()

	t.Run("invalid manifest is rejected before custody access", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		manifest.Manifest.Entries = nil
		service := newTestService(
			t, &recordingObjectStore{events: &events}, &recordingMetadataStore{events: &events},
		)

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, ErrInvalid)
		assert.Empty(t, events)
	})

	t.Run("missing object never finalizes or commits manifest", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		objects := &recordingObjectStore{events: &events, verifyErr: ErrMissingObject}
		service := newTestService(t, objects, &recordingMetadataStore{events: &events})

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, ErrMissingObject)
		assert.Equal(t, []string{"verify-objects"}, events)
	})

	t.Run("physical verification conflict never registers object", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		objects := &recordingObjectStore{events: &events, verifyErr: ErrConflict}
		service := newTestService(t, objects, &recordingMetadataStore{events: &events})

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, ErrConflict)
		assert.Equal(t, []string{"verify-objects"}, events)
	})

	t.Run("physical verification failure never registers object", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		verifyErr := errors.New("custody backend unavailable")
		objects := &recordingObjectStore{events: &events, verifyErr: verifyErr}
		service := newTestService(t, objects, &recordingMetadataStore{events: &events})

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, verifyErr)
		assert.Equal(t, []string{"verify-objects"}, events)
	})

	t.Run("manifest custody failure never commits metadata", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		putErr := errors.New("manifest backend unavailable")
		objects := &recordingObjectStore{events: &events, putManifestErr: putErr}
		service := newTestService(t, objects, &recordingMetadataStore{events: &events})

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, putErr)
		assert.Equal(t, []string{
			"verify-objects", "record-objects", "put-manifest",
		}, events)
	})

	t.Run("manifest semantic mismatch never commits metadata", func(t *testing.T) {
		t.Parallel()
		events := []string{}
		identity, manifest := validServiceInput(t)
		objects := &recordingObjectStore{
			events: &events,
			putManifestResult: PutResult{
				Info: ObjectInfo{Ref: serviceObject("different manifest")},
			},
		}
		service := newTestService(t, objects, &recordingMetadataStore{events: &events})

		_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
		assert.ErrorIs(t, err, ErrConflict)
		assert.Equal(t, []string{
			"verify-objects", "record-objects", "put-manifest",
		}, events)
	})
}

func TestServiceCommitPreservesMetadataConflictAfterManifestCustody(t *testing.T) {
	t.Parallel()

	events := []string{}
	identity, manifest := validServiceInput(t)
	conflict := &HeadConflictError{CurrentGeneration: 4}
	objects := &recordingObjectStore{events: &events}
	metadata := &recordingMetadataStore{events: &events, commitErr: conflict}
	service := newTestService(t, objects, metadata)

	_, err := service.CommitManifest(t.Context(), identity, manifest.Manifest)
	assert.ErrorIs(t, err, ErrConflict)
	var headConflict *HeadConflictError
	assert.ErrorAs(t, err, &headConflict)
	assert.Equal(t, int64(4), headConflict.CurrentGeneration)
	assert.Equal(t, []string{
		"verify-objects", "record-objects", "put-manifest", "commit",
	}, events)
}

func TestServiceCommitStopsBetweenBoundedObjectChecks(t *testing.T) {
	t.Parallel()

	events := []string{}
	identity, manifest := validServiceInput(t)
	second := serviceObject("second")
	manifest.Manifest.Entries[0].Objects = append(manifest.Manifest.Entries[0].Objects, second)
	manifest.Manifest.Entries[0].Length += second.Length
	canonical, err := ValidateAndCanonicalize(identity, manifest.Manifest, DefaultManifestLimits())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	objects := &recordingObjectStore{events: &events}
	metadata := &recordingMetadataStore{events: &events, afterRecordObjects: cancel}
	service := newTestService(t, objects, metadata)

	_, err = service.CommitManifest(ctx, identity, canonical.Manifest)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"verify-objects", "record-objects"}, events)
}

func newTestService(
	t *testing.T,
	objects ObjectStore,
	metadata MetadataStore,
) *Service {
	t.Helper()
	service, err := NewService(objects, metadata, DefaultManifestLimits(), "parser-data-17")
	require.NoError(t, err)
	return service
}

func validServiceInput(t *testing.T) (AuthIdentity, CanonicalManifest) {
	t.Helper()
	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	object := serviceObject("body")
	manifest, err := ValidateAndCanonicalize(identity, Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		Provider:         parser.AgentCodex,
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/session.jsonl",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             ManifestSnapshot,
		Entries: []Entry{{
			Path: "session.jsonl", Type: "file", Length: object.Length,
			Objects: []ObjectRef{object},
		}},
	}, DefaultManifestLimits())
	require.NoError(t, err)
	return identity, manifest
}

func serviceObject(body string) ObjectRef {
	digest := sha256.Sum256([]byte(body))
	return ObjectRef{SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body))}
}

type recordingObjectStore struct {
	events            *[]string
	putObjectResult   PutResult
	putObjectErr      error
	putObjectTenant   string
	statInfo          ObjectInfo
	statErr           error
	missing           []ObjectRef
	missingErr        error
	missingTenant     string
	verifyErr         error
	verifiedObjects   []ObjectRef
	putManifestResult PutResult
	putManifestErr    error
}

func (s *recordingObjectStore) VerifyObjects(
	_ context.Context,
	_ string,
	objects []ObjectRef,
) error {
	s.event("verify-objects")
	s.verifiedObjects = append([]ObjectRef(nil), objects...)
	return s.verifyErr
}

func (s *recordingObjectStore) event(value string) {
	if s.events != nil {
		*s.events = append(*s.events, value)
	}
}

func (s *recordingObjectStore) PutObject(
	_ context.Context,
	tenantID string,
	object ObjectRef,
	_ io.Reader,
) (PutResult, error) {
	s.event("put-object")
	s.putObjectTenant = tenantID
	if s.putObjectResult.Info.Ref == (ObjectRef{}) && s.putObjectErr == nil {
		s.putObjectResult.Info.Ref = object
	}
	return s.putObjectResult, s.putObjectErr
}

func (s *recordingObjectStore) StatObject(
	_ context.Context,
	_ string,
	object ObjectRef,
) (ObjectInfo, error) {
	s.event("stat-object")
	if s.statInfo == (ObjectInfo{}) {
		return ObjectInfo{Ref: object}, s.statErr
	}
	return s.statInfo, s.statErr
}

func (s *recordingObjectStore) OpenObject(
	context.Context,
	string,
	ObjectRef,
) (ObjectInfo, VerifiedObjectReader, error) {
	return ObjectInfo{}, nil, errors.New("unexpected OpenObject call")
}

func (s *recordingObjectStore) MissingObjects(
	_ context.Context,
	tenantID string,
	_ []ObjectRef,
) ([]ObjectRef, error) {
	s.event("missing-objects")
	s.missingTenant = tenantID
	return append([]ObjectRef(nil), s.missing...), s.missingErr
}

func (s *recordingObjectStore) PutManifest(
	_ context.Context,
	manifest CanonicalManifest,
) (PutResult, error) {
	s.event("put-manifest")
	if s.putManifestResult.Info.Ref == (ObjectRef{}) && s.putManifestErr == nil {
		s.putManifestResult.Info.Ref = ObjectRef{
			SHA256: manifest.ManifestID, Length: int64(len(manifest.CanonicalJSON)),
		}
	}
	return s.putManifestResult, s.putManifestErr
}

func (s *recordingObjectStore) OpenManifest(
	context.Context,
	AuthIdentity,
	string,
) (ObjectInfo, VerifiedObjectReader, error) {
	return ObjectInfo{}, nil, errors.New("unexpected OpenManifest call")
}

type recordingMetadataStore struct {
	events             *[]string
	recordErr          error
	recordedObjects    []ObjectRef
	afterRecord        func()
	afterRecordObjects func()
	commitResult       CommitResult
	commitErr          error
	committed          []CanonicalManifest
	processingVersion  string
}

func (s *recordingMetadataStore) RecordVerifiedObjects(
	_ context.Context,
	_ AuthIdentity,
	objects []ObjectRef,
) error {
	s.event("record-objects")
	s.recordedObjects = append(s.recordedObjects, objects...)
	if s.afterRecordObjects != nil {
		s.afterRecordObjects()
	}
	return s.recordErr
}

func (s *recordingMetadataStore) event(value string) {
	if s.events != nil {
		*s.events = append(*s.events, value)
	}
}

func (s *recordingMetadataStore) RecordVerifiedObject(
	_ context.Context,
	_ AuthIdentity,
	object ObjectRef,
) error {
	s.event("record-object")
	s.recordedObjects = append(s.recordedObjects, object)
	if s.afterRecord != nil {
		s.afterRecord()
	}
	return s.recordErr
}

func (s *recordingMetadataStore) MissingObjects(
	context.Context,
	AuthIdentity,
	[]ObjectRef,
) ([]ObjectRef, error) {
	return nil, errors.New("metadata MissingObjects must not be called")
}

func (s *recordingMetadataStore) CommitManifest(
	_ context.Context,
	manifest CanonicalManifest,
	processingVersion string,
) (CommitResult, error) {
	s.event("commit")
	s.committed = append(s.committed, manifest)
	s.processingVersion = processingVersion
	return s.commitResult, s.commitErr
}
