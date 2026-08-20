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
	"go.kenn.io/agentsview/internal/db"
)

func TestReadVerifiedImportArtifactByteBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		kind      Kind
		extension string
		limit     int64
	}{
		{
			name: "checkpoint", kind: KindCheckpoints,
			extension: ".json", limit: checkpointDecodedLimit,
		},
		{
			name: "manifest", kind: KindManifests,
			extension: ".json", limit: manifestDecodedLimit,
		},
		{
			name: "segment", kind: KindSegments,
			extension: ".ndjson", limit: segmentDecodedLimit,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := make([]byte, tc.limit)
			identity := Identity{
				SHA256: hashHex(body),
				Size:   int64(len(body)),
			}
			name := identity.SHA256 + tc.extension
			if tc.kind == KindCheckpoints {
				name = "cp-0000000001.json"
			}
			ref := requireContractRef(t, contractOrigin, tc.kind, name)
			store := &countingOpenStore{
				entry:  Entry{Ref: ref, Identity: identity},
				reader: &testVerifiedReader{Reader: bytes.NewReader(body)},
			}

			got, err := readVerifiedImportArtifact(
				t.Context(), store, store.entry, tc.limit,
			)
			require.NoError(t, err)
			assert.Len(t, got, int(tc.limit))
			assert.Equal(t, 1, store.opens)

			oversize := store.entry
			oversize.Identity.Size++
			_, err = readVerifiedImportArtifact(
				t.Context(), store, oversize, tc.limit,
			)
			require.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Equal(t, 1, store.opens)
		})
	}
}

func TestFutureArtifactVersionErrorsIdentifyDependencyKind(t *testing.T) {
	t.Parallel()

	t.Run("manifest", func(t *testing.T) {
		_, err := decodeManifestWithLimits(
			[]byte(`{"origin":"contract-a1b2c3","v":4}`),
			productionArtifactLimits(),
		)
		require.ErrorIs(t, err, errFutureArtifactVersion)
		var future *futureArtifactVersionError
		require.ErrorAs(t, err, &future)
		assert.Equal(t, Kind(KindManifests), future.Kind)
		assert.Equal(t, 4, future.Version)
	})

	t.Run("segment", func(t *testing.T) {
		_, err := decodeSegmentWithLimits(
			[]byte("{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":3}\n"),
			productionArtifactLimits(),
		)
		require.ErrorIs(t, err, errFutureArtifactVersion)
		var future *futureArtifactVersionError
		require.ErrorAs(t, err, &future)
		assert.Equal(t, Kind(KindSegments), future.Kind)
		assert.Equal(t, 3, future.Version)
	})
}

func TestFutureSegmentVersionPrecedesCurrentRecordLimit(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	for range productionArtifactLimits().segmentMessages + 1 {
		body.WriteString(
			"{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":3}\n",
		)
	}

	_, err := decodeSegmentWithLimits(
		[]byte(body.String()), productionArtifactLimits(),
	)
	require.ErrorIs(t, err, errFutureArtifactVersion)
	var future *futureArtifactVersionError
	require.ErrorAs(t, err, &future)
	assert.Equal(t, Kind(KindSegments), future.Kind)
	assert.Equal(t, 3, future.Version)
}

func TestCurrentSegmentRecordLimitPrecedesLaterRecordDecode(t *testing.T) {
	t.Parallel()

	limits := productionArtifactLimits()
	limits.segmentMessages = 1
	body := []byte(
		"{\"content\":\"one\",\"ordinal\":0,\"role\":\"user\",\"v\":1}\n" +
			"{not-json}\n",
	)

	_, err := decodeSegmentWithLimits(body, limits)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message record limit exceeded")
}

func TestImportCollectionBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("manifest usage events", func(t *testing.T) {
		limits := productionArtifactLimits()
		limits.manifestUsageEvents = 2
		m := importTestManifest("session")
		m.UsageEvents = make([]artifactUsageEvent, 2)
		body, err := canonicalJSON(m)
		require.NoError(t, err)
		_, err = decodeManifestWithLimits(body, limits)
		require.NoError(t, err)

		m.UsageEvents = append(m.UsageEvents, artifactUsageEvent{})
		body, err = canonicalJSON(m)
		require.NoError(t, err)
		_, err = decodeManifestWithLimits(body, limits)
		require.Error(t, err)
	})

	t.Run("manifest segments", func(t *testing.T) {
		limits := productionArtifactLimits()
		limits.manifestSegments = 2
		m := importTestManifest("session")
		m.Segments = []string{
			strings.Repeat("a", 64),
			strings.Repeat("b", 64),
		}
		body, err := canonicalJSON(m)
		require.NoError(t, err)
		_, err = decodeManifestWithLimits(body, limits)
		require.NoError(t, err)

		m.Segments = append(m.Segments, strings.Repeat("c", 64))
		body, err = canonicalJSON(m)
		require.NoError(t, err)
		_, err = decodeManifestWithLimits(body, limits)
		require.Error(t, err)
	})

	t.Run("segment messages", func(t *testing.T) {
		limits := productionArtifactLimits()
		limits.segmentMessages = 2
		messages := []db.Message{
			{Ordinal: 0, Role: "user"},
			{Ordinal: 1, Role: "assistant"},
		}
		body, err := encodeSegment(messages)
		require.NoError(t, err)
		_, err = decodeSegmentWithLimits(body, limits)
		require.NoError(t, err)

		messages = append(messages, db.Message{Ordinal: 2, Role: "user"})
		body, err = encodeSegment(messages)
		require.NoError(t, err)
		_, err = decodeSegmentWithLimits(body, limits)
		require.Error(t, err)
	})

	tests := []struct {
		name      string
		configure func(*artifactLimits)
		message   func(int) db.Message
	}{
		{
			name: "message tool calls",
			configure: func(limits *artifactLimits) {
				limits.messageToolCalls = 2
				limits.segmentToolCalls = 3
			},
			message: func(count int) db.Message {
				return db.Message{
					Ordinal: 0, Role: "assistant",
					ToolCalls: make([]db.ToolCall, count),
				}
			},
		},
		{
			name: "tool result events",
			configure: func(limits *artifactLimits) {
				limits.toolResultEvents = 2
				limits.segmentResultEvents = 3
			},
			message: func(count int) db.Message {
				return db.Message{
					Ordinal: 0, Role: "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: make([]db.ToolResultEvent, count),
					}},
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limits := productionArtifactLimits()
			tc.configure(&limits)
			body, err := encodeSegment([]db.Message{tc.message(2)})
			require.NoError(t, err)
			_, err = decodeSegmentWithLimits(body, limits)
			require.NoError(t, err)

			body, err = encodeSegment([]db.Message{tc.message(3)})
			require.NoError(t, err)
			_, err = decodeSegmentWithLimits(body, limits)
			require.Error(t, err)
		})
	}

	aggregateTests := []struct {
		name      string
		configure func(*artifactLimits)
		message   func(int) []db.Message
	}{
		{
			name: "segment tool calls",
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 2
			},
			message: func(count int) []db.Message {
				messages := make([]db.Message, count)
				for ordinal := range messages {
					messages[ordinal] = db.Message{
						Ordinal: ordinal, Role: "assistant",
						ToolCalls: []db.ToolCall{{}},
					}
				}
				return messages
			},
		},
		{
			name: "segment result events",
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 2
			},
			message: func(count int) []db.Message {
				messages := make([]db.Message, count)
				for ordinal := range messages {
					messages[ordinal] = db.Message{
						Ordinal: ordinal, Role: "assistant",
						ToolCalls: []db.ToolCall{{
							ResultEvents: []db.ToolResultEvent{{}},
						}},
					}
				}
				return messages
			},
		},
	}
	for _, tc := range aggregateTests {
		t.Run(tc.name, func(t *testing.T) {
			limits := productionArtifactLimits()
			tc.configure(&limits)
			body, err := encodeSegment(tc.message(2))
			require.NoError(t, err)
			_, err = decodeSegmentWithLimits(body, limits)
			require.NoError(t, err)

			body, err = encodeSegment(tc.message(3))
			require.NoError(t, err)
			_, err = decodeSegmentWithLimits(body, limits)
			require.Error(t, err)
		})
	}
}

func TestDecodeImportCheckpointAcceptsSemanticCurrentJSON(t *testing.T) {
	t.Parallel()

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

func TestDecodeImportCheckpointStreamsBoundedSessionPages(t *testing.T) {
	t.Parallel()

	const sessionCount = 300
	var body strings.Builder
	fmt.Fprintf(&body, `{"origin":%q,"seq":7,"sessions":{`, contractOrigin)
	for i := range sessionCount {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, "%q:%q",
			fmt.Sprintf("%s~session-%03d", contractOrigin, i),
			fmt.Sprintf("%064x", i+1))
	}
	body.WriteString(`},"v":1}`)

	header, sessions, err := decodeImportCheckpointHeader(
		[]byte(body.String()), contractOrigin, "cp-0000000007.json",
	)
	require.NoError(t, err)
	assert.Equal(t, 7, header.Sequence)
	var pageSizes []int
	count, err := streamImportCheckpointSessions(
		sessions, contractOrigin, 128,
		func(page []importCheckpointSession) error {
			pageSizes = append(pageSizes, len(page))
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, sessionCount, count)
	assert.Equal(t, []int{128, 128, 44}, pageSizes)
}

func TestDecodeImportCheckpointDefersSessionValidationToPages(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	fmt.Fprintf(&body, `{"origin":%q,"seq":7,"sessions":{`, contractOrigin)
	for i := range 128 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(
			&body, "%q:%q",
			fmt.Sprintf("%s~session-%03d", contractOrigin, i),
			fmt.Sprintf("%064x", i+1),
		)
	}
	body.WriteString(`,"broken":},"v":1}`)

	_, sessions, err := decodeImportCheckpointHeader(
		[]byte(body.String()), contractOrigin, "cp-0000000007.json",
	)
	require.NoError(t, err)
	page, _, done, err := decodeImportCheckpointSessionPage(
		sessions, contractOrigin, 0, 128,
	)
	require.NoError(t, err)
	assert.Len(t, page, 128)
	assert.False(t, done)
}

func TestDecodeImportCheckpointRejectsInvalidCurrentJSON(t *testing.T) {
	t.Parallel()

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
			"native GID contains separator",
			strings.Replace(
				valid,
				contractOrigin+"~session",
				contractOrigin+"~session~nested",
				1,
			),
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

func TestDecodeImportCheckpointBoundsTopLevelFieldState(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	body.WriteByte('{')
	for i := range 10_000 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, "%q:0", fmt.Sprintf("unknown-%05d", i))
	}
	body.WriteString(`,"v":2}`)
	data := []byte(body.String())

	fields, version, future, err := decodeImportCheckpointFields(data)
	require.NoError(t, err)
	assert.Empty(t, fields)
	assert.Equal(t, 2, version)
	assert.True(t, future)

	_, err = decodeImportCheckpoint(
		data, contractOrigin, "cp-0000000007.json",
	)
	require.ErrorIs(t, err, errFutureArtifactVersion)
}

func TestDecodeImportCheckpointRejectsCurrentExtraFieldBeforeItsValue(
	t *testing.T,
) {
	// Serial: AllocsPerRun measures process-global allocation state.
	var body strings.Builder
	body.WriteString(`{"v":1,"extra":{`)
	for i := range 10_000 {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, "%q:0", fmt.Sprintf("nested-%05d", i))
	}
	body.WriteString(`}}`)
	data := []byte(body.String())

	var decodeErr error
	allocations := testing.AllocsPerRun(1, func() {
		_, decodeErr = decodeImportCheckpoint(
			data, contractOrigin, "cp-0000000007.json",
		)
	})
	require.ErrorIs(t, decodeErr, ErrArtifactInvalid)
	assert.Less(t, allocations, 500.0)
}

func TestDecodeImportCheckpointDefersExtensibleFutureJSON(t *testing.T) {
	t.Parallel()

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

func TestDecodeImportCheckpointFutureVersionDoesNotMaskMalformedJSON(
	t *testing.T,
) {
	t.Parallel()

	_, err := decodeImportCheckpoint(
		[]byte(`{"sessions":{"broken":},"v":2}`),
		contractOrigin, "cp-0000000007.json",
	)
	require.ErrorIs(t, err, ErrArtifactInvalid)
	assert.NotErrorIs(t, err, errFutureArtifactVersion)
}

func TestReadVerifiedImportArtifactUsesExactBoundedIdentity(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
