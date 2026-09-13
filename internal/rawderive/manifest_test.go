package rawderive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestManifestLoaderReadsAndVerifiesCanonicalEnvelope(t *testing.T) {
	t.Parallel()
	identity, canonical := canonicalTestManifest(t)
	reader := &testVerifiedReader{Reader: bytes.NewReader(canonical.CanonicalJSON)}
	store := manifestStoreFunc(func(
		_ context.Context,
		gotIdentity rawsync.AuthIdentity,
		gotManifestID string,
	) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error) {
		assert.Equal(t, identity, gotIdentity)
		assert.Equal(t, canonical.ManifestID, gotManifestID)
		return rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
			SHA256: canonical.ManifestID,
			Length: int64(len(canonical.CanonicalJSON)),
		}}, reader, nil
	})

	got, err := (ManifestLoader{
		Store:  store,
		Limits: rawsync.DefaultManifestLimits(),
	}).Load(t.Context(), JobLease{Identity: identity, ManifestID: canonical.ManifestID})
	require.NoError(t, err)
	assert.Equal(t, canonical, got)
	assert.True(t, reader.verified)
	assert.True(t, reader.closed)
}

func TestManifestLoaderRejectsUnverifiedOrMismatchedObjects(t *testing.T) {
	t.Parallel()
	identity, canonical := canonicalTestManifest(t)
	verificationFailure := errors.New("digest mismatch")

	for _, tc := range []struct {
		name   string
		info   rawsync.ObjectInfo
		bytes  []byte
		verify error
	}{
		{
			name: "wrong advertised digest",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Length: int64(len(canonical.CanonicalJSON)),
			}},
			bytes: canonical.CanonicalJSON,
		},
		{
			name: "wrong advertised length",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: canonical.ManifestID,
				Length: int64(len(canonical.CanonicalJSON) + 1),
			}},
			bytes: canonical.CanonicalJSON,
		},
		{
			name: "verification failure",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: canonical.ManifestID,
				Length: int64(len(canonical.CanonicalJSON)),
			}},
			bytes:  canonical.CanonicalJSON,
			verify: verificationFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &testVerifiedReader{
				Reader:    bytes.NewReader(tc.bytes),
				verifyErr: tc.verify,
			}
			store := manifestStoreFunc(func(
				context.Context,
				rawsync.AuthIdentity,
				string,
			) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error) {
				return tc.info, reader, nil
			})

			_, err := (ManifestLoader{
				Store:  store,
				Limits: rawsync.DefaultManifestLimits(),
			}).Load(t.Context(), JobLease{
				Identity: identity, ManifestID: canonical.ManifestID,
			})
			require.Error(t, err)
			assert.True(t, reader.closed)
		})
	}
}

func TestManifestLoaderLoadManifestUsesIdenticalTerminalVerification(t *testing.T) {
	t.Parallel()
	identity, canonical := canonicalTestManifest(t)
	verificationFailure := errors.New("digest mismatch")

	tests := []struct {
		name    string
		info    rawsync.ObjectInfo
		payload []byte
		verify  error
		wantErr bool
	}{
		{
			name: "verified",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: canonical.ManifestID, Length: int64(len(canonical.CanonicalJSON)),
			}},
			payload: canonical.CanonicalJSON,
		},
		{
			name: "identity mismatch",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Length: int64(len(canonical.CanonicalJSON)),
			}},
			payload: canonical.CanonicalJSON,
			wantErr: true,
		},
		{
			name: "length mismatch",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: canonical.ManifestID, Length: int64(len(canonical.CanonicalJSON) + 1),
			}},
			payload: canonical.CanonicalJSON,
			wantErr: true,
		},
		{
			name: "hash mismatch",
			info: rawsync.ObjectInfo{Ref: rawsync.ObjectRef{
				SHA256: canonical.ManifestID, Length: int64(len(canonical.CanonicalJSON)),
			}},
			payload: canonical.CanonicalJSON,
			verify:  verificationFailure,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, method := range []string{"Load", "LoadManifest"} {
				t.Run(method, func(t *testing.T) {
					reader := &testVerifiedReader{
						Reader: bytes.NewReader(tc.payload), verifyErr: tc.verify,
					}
					loader := ManifestLoader{
						Store: manifestStoreFunc(func(
							_ context.Context,
							gotIdentity rawsync.AuthIdentity,
							gotManifestID string,
						) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error) {
							assert.Equal(t, identity, gotIdentity)
							assert.Equal(t, canonical.ManifestID, gotManifestID)
							return tc.info, reader, nil
						}),
						Limits: rawsync.DefaultManifestLimits(),
					}
					var got rawsync.CanonicalManifest
					var err error
					if method == "Load" {
						got, err = loader.Load(t.Context(), JobLease{
							Identity: identity, ManifestID: canonical.ManifestID,
						})
					} else {
						got, err = loader.LoadManifest(
							t.Context(), identity, canonical.ManifestID,
						)
					}
					if tc.wantErr {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
						assert.Equal(t, canonical, got)
					}
					assert.True(t, reader.closed)
					assert.Equal(t,
						tc.info.Ref.SHA256 == canonical.ManifestID &&
							tc.info.Ref.Length == int64(len(canonical.CanonicalJSON)),
						reader.verified,
					)
				})
			}
		})
	}
}

type manifestStoreFunc func(
	context.Context,
	rawsync.AuthIdentity,
	string,
) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error)

func (f manifestStoreFunc) OpenManifest(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	manifestID string,
) (rawsync.ObjectInfo, rawsync.VerifiedObjectReader, error) {
	return f(ctx, identity, manifestID)
}

type testVerifiedReader struct {
	io.Reader
	verifyErr error
	verified  bool
	closed    bool
}

func (r *testVerifiedReader) Verify() error {
	r.verified = true
	return r.verifyErr
}

func (r *testVerifiedReader) Close() error {
	r.closed = true
	// Cancellation closes this reader to interrupt in-flight reads, so a
	// close must reach a closable underlying stream too.
	if closer, ok := r.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func canonicalTestManifest(t *testing.T) (rawsync.AuthIdentity, rawsync.CanonicalManifest) {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	object, err := rawsync.NewObjectRef(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 3,
	)
	require.NoError(t, err)
	canonical, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:    rawsync.ManifestSchemaVersion,
		Provider:         "codex",
		ConfiguredRootID: "root-a",
		SourceKey:        "sessions/demo.jsonl#main",
		CaptureID:        "capture-a",
		CapturedAt:       time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		Kind:             rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path: "session.jsonl", Type: "file", Length: 3,
			Objects: []rawsync.ObjectRef{object},
		}},
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return identity, canonical
}
