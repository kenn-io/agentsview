package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFutureArtifactVersionErrorsIdentifyDependencyKind(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		_, err := decodeManifestWithLimits(
			[]byte(`{"origin":"contract-a1b2c3","v":3}`),
			productionArtifactLimits(),
		)
		require.ErrorIs(t, err, errFutureArtifactVersion)
		var future *futureArtifactVersionError
		require.ErrorAs(t, err, &future)
		assert.Equal(t, Kind(KindManifests), future.Kind)
		assert.Equal(t, 3, future.Version)
	})

	t.Run("segment", func(t *testing.T) {
		_, err := decodeSegmentWithLimits(
			[]byte("{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":2}\n"),
			productionArtifactLimits(),
		)
		require.ErrorIs(t, err, errFutureArtifactVersion)
		var future *futureArtifactVersionError
		require.ErrorAs(t, err, &future)
		assert.Equal(t, Kind(KindSegments), future.Kind)
		assert.Equal(t, 2, future.Version)
	})
}

func TestDecodeImportCheckpointAcceptsSemanticCurrentJSON(t *testing.T) {
	hash := strings.Repeat("a", 64)
	want := importCheckpoint{
		Version:  1,
		Origin:   contractOrigin,
		Sequence: 7,
		Sessions: map[string]string{contractOrigin + "~session": hash},
	}
	tests := []struct {
		name string
		body string
	}{
		{
			name: "canonical",
			body: fmt.Sprintf(
				`{"origin":%q,"seq":7,"sessions":{%q:%q},"v":1}`+"\n",
				contractOrigin, contractOrigin+"~session", hash,
			),
		},
		{
			name: "whitespace and reordered keys",
			body: fmt.Sprintf(
				" { \"v\" : 1, \"sessions\" : {%q : %q}, "+
					"\"seq\" : 7, \"origin\" : %q } \n",
				contractOrigin+"~session", hash, contractOrigin,
			),
		},
		{
			name: "escaped field name",
			body: fmt.Sprintf(
				`{"o\u0072igin":%q,"seq":7,"sessions":{%q:%q},"v":1}`,
				contractOrigin, contractOrigin+"~session", hash,
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeImportCheckpoint(
				[]byte(tc.body), contractOrigin, "cp-0000000007.json",
			)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestDecodeImportCheckpointRejectsInvalidCurrentJSON(t *testing.T) {
	hash := strings.Repeat("a", 64)
	valid := fmt.Sprintf(
		`{"origin":%q,"seq":7,"sessions":{%q:%q},"v":1}`,
		contractOrigin, contractOrigin+"~session", hash,
	)
	tests := []struct {
		name string
		body string
		file string
	}{
		{"trailing JSON", valid + `{}`, "cp-0000000007.json"},
		{
			"unknown field",
			strings.TrimSuffix(valid, "}") + `,"extra":true}`,
			"cp-0000000007.json",
		},
		{
			"wrong origin",
			strings.Replace(valid, contractOrigin, "another-a1b2c3", 1),
			"cp-0000000007.json",
		},
		{"wrong sequence name", valid, "cp-0000000008.json"},
		{
			"malformed GID",
			strings.Replace(valid, contractOrigin+"~session", "session", 1),
			"cp-0000000007.json",
		},
		{
			"invalid manifest hash",
			strings.Replace(valid, hash, "ABC", 1),
			"cp-0000000007.json",
		},
		{
			"duplicate top-level key",
			strings.TrimSuffix(valid, "}") + `,"v":1}`,
			"cp-0000000007.json",
		},
		{
			"duplicate session key",
			fmt.Sprintf(
				`{"origin":%q,"seq":7,"sessions":{%q:%q,%q:%q},"v":1}`,
				contractOrigin,
				contractOrigin+"~session", hash,
				contractOrigin+"~session", hash,
			),
			"cp-0000000007.json",
		},
		{
			"old version",
			strings.Replace(valid, `"v":1`, `"v":0`, 1),
			"cp-0000000007.json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeImportCheckpoint(
				[]byte(tc.body), contractOrigin, tc.file,
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

func TestDecodeImportCheckpointDefersExtensibleFutureJSON(t *testing.T) {
	tests := []string{
		`{"sessions":"opaque","v":2}`,
		`{"new_field":{"codec":"v3"},"sessions":[1,2,3],"v":3}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			_, err := decodeImportCheckpoint(
				[]byte(body), contractOrigin, "cp-0000000007.json",
			)
			require.ErrorIs(t, err, errFutureArtifactVersion)
			var future *futureArtifactVersionError
			require.ErrorAs(t, err, &future)
			assert.Equal(t, Kind(KindCheckpoints), future.Kind)
			assert.Greater(t, future.Version, checkpointFormatVersion)
		})
	}
}

func TestReadVerifiedImportArtifactUsesExactBoundedIdentity(t *testing.T) {
	store := newTestArtifactStore(t)
	ref := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	body := []byte(`{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}`)
	created := createContractArtifact(t, store, ref, body)

	got, err := readVerifiedImportArtifact(
		t.Context(), store, created.Entry, checkpointDecodedLimit,
	)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	wrongEntry := created.Entry
	wrongEntry.Identity.SHA256 = strings.Repeat("b", 64)
	_, err = readVerifiedImportArtifact(
		t.Context(), store, wrongEntry, checkpointDecodedLimit,
	)
	assert.ErrorIs(t, err, ErrArtifactCorrupt)
}

func TestReadVerifiedImportArtifactRejectsOversizeBeforeOpen(t *testing.T) {
	ref := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	store := &countingOpenStore{}
	entry := Entry{
		Ref: ref,
		Identity: Identity{
			SHA256: strings.Repeat("a", 64),
			Size:   checkpointDecodedLimit + 1,
		},
	}

	_, err := readVerifiedImportArtifact(
		t.Context(), store, entry, checkpointDecodedLimit,
	)
	require.ErrorIs(t, err, ErrArtifactInvalid)
	assert.Zero(t, store.opens)
}

func TestReadVerifiedImportArtifactPreservesOperationalReadError(t *testing.T) {
	ref := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	operational := errors.New("storage unavailable")
	body := []byte("partial")
	store := &countingOpenStore{
		entry: Entry{
			Ref: ref,
			Identity: Identity{
				SHA256: strings.Repeat("a", 64),
				Size:   int64(len(body)),
			},
		},
		reader: &testVerifiedReader{
			Reader:  bytes.NewReader(body),
			readErr: operational,
		},
	}

	_, err := readVerifiedImportArtifact(
		t.Context(), store, store.entry, checkpointDecodedLimit,
	)
	assert.ErrorIs(t, err, operational)
	assert.Equal(t, 1, store.opens)
}

type countingOpenStore struct {
	ArtifactStore
	entry  Entry
	reader VerifiedReader
	err    error
	opens  int
}

func (s *countingOpenStore) Open(
	context.Context, Ref,
) (Entry, VerifiedReader, error) {
	s.opens++
	return s.entry, s.reader, s.err
}

type testVerifiedReader struct {
	io.Reader
	readErr   error
	verifyErr error
	closeErr  error
	failed    bool
}

func (r *testVerifiedReader) Read(p []byte) (int, error) {
	if r.readErr != nil && !r.failed {
		r.failed = true
		return 0, r.readErr
	}
	return r.Reader.Read(p)
}

func (r *testVerifiedReader) Verify() error {
	return r.verifyErr
}

func (r *testVerifiedReader) Close() error {
	return r.closeErr
}
