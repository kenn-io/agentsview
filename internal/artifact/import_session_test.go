package artifact

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestRewriteManifestForImportClearsLocalStateAndPrefixesRelationships(
	t *testing.T,
) {
	t.Parallel()

	filePath := "/provider/session.jsonl"
	fileSize := int64(1024)
	fileMtime := int64(42)
	fileInode := int64(7)
	fileDevice := int64(8)
	fileHash := strings.Repeat("f", 64)
	parent := "parent"
	m := manifest{
		Version: manifestFormatVersion, Origin: contractOrigin,
		NativeSessionID: "session",
		SessionName:     new("Agent title"),
		Session: manifestSession{
			ID: "session", Project: "project", Machine: contractOrigin,
			Agent: "claude", ParentSessionID: &parent,
			SourceSessionID: "source",
			SecretLeakCount: 4,
			FilePath:        &filePath,
			FileSize:        &fileSize,
			FileMtime:       &fileMtime,
			FileInode:       &fileInode,
			FileDevice:      &fileDevice,
			FileHash:        &fileHash,
			CreatedAt:       "2026-07-28T00:00:00Z",
		},
		UsageEvents: []artifactUsageEvent{{
			Source: "provider", Model: "model",
			Cost:       &money.Money{Microdollars: 31_250},
			CostStatus: "known", CostSource: "provider",
		}},
		DataVersion:           9,
		SessionHasToolCalls:   true,
		SessionHasContextData: true,
		SessionQualitySignals: &manifestQualitySignals{
			Version: 3, ShortPromptCount: 2, UnstructuredStart: true,
		},
	}
	messages := []db.Message{{
		ID: 99, SessionID: "session", Ordinal: 0, Role: "assistant",
		Content: "done",
		ToolCalls: []db.ToolCall{{
			MessageID: 88, SessionID: "session",
			ToolName: "Task", SubagentSessionID: "child",
			ResultEvents: []db.ToolResultEvent{{
				SubagentSessionID: contractOrigin + "~existing",
				EventIndex:        0,
			}},
		}},
	}}

	write := rewriteManifestForImport(m, messages)
	importedID := contractOrigin + "~session"
	assert.Equal(t, importedID, write.Session.ID)
	assert.Equal(t, contractOrigin, write.Session.Machine)
	assert.Equal(t, m.SessionName, write.Session.SessionName)
	assert.Nil(t, write.Session.FilePath)
	assert.Nil(t, write.Session.FileSize)
	assert.Nil(t, write.Session.FileMtime)
	assert.Nil(t, write.Session.FileInode)
	assert.Nil(t, write.Session.FileDevice)
	assert.Nil(t, write.Session.FileHash)
	assert.Zero(t, write.Session.NextOrdinal)
	assert.Nil(t, write.Session.LastEntryUUID)
	assert.Zero(t, write.Session.SecretLeakCount)
	assert.Empty(t, write.Session.SecretsRulesVersion)
	assert.Equal(t, contractOrigin+"~source", write.Session.SourceSessionID)
	require.NotNil(t, write.Session.ParentSessionID)
	assert.Equal(t, contractOrigin+"~parent", *write.Session.ParentSessionID)
	assert.True(t, write.Session.HasToolCalls)
	assert.True(t, write.Session.HasContextData)
	assert.Equal(
		t, m.SessionQualitySignals.dbQualitySignals(),
		write.Session.StoredQualitySignals(),
	)
	assert.True(t, write.ReplaceMessages)
	assert.Equal(t, m.DataVersion, write.DataVersion)
	assert.Empty(t, write.Findings)

	require.Len(t, write.Messages, 1)
	assert.Zero(t, write.Messages[0].ID)
	assert.Equal(t, importedID, write.Messages[0].SessionID)
	require.Len(t, write.Messages[0].ToolCalls, 1)
	call := write.Messages[0].ToolCalls[0]
	assert.Zero(t, call.MessageID)
	assert.Equal(t, importedID, call.SessionID)
	assert.Equal(t, contractOrigin+"~child", call.SubagentSessionID)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(
		t, contractOrigin+"~existing",
		call.ResultEvents[0].SubagentSessionID,
	)

	require.Len(t, write.UsageEvents, 1)
	assert.Equal(t, importedID, write.UsageEvents[0].SessionID)
	assert.Equal(t, m.UsageEvents[0].Cost, write.UsageEvents[0].Cost)
	assert.Empty(t, write.Signals.SecretsRulesVersion)
	assert.Zero(t, write.Signals.SecretLeakCount)
}

func TestLoadImportedSessionCompleteClosure(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	m := importTestManifest("session")
	messages := []db.Message{{
		Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5,
	}}
	manifestHash := createImportTestClosure(t, store, &m, messages)

	write, outcome, err := loadImportedSession(
		t.Context(), database, store, contractOrigin,
		contractOrigin+"~session", manifestHash,
		productionArtifactLimits(),
	)
	require.NoError(t, err)
	assert.Equal(t, importClosureComplete, outcome)
	assert.Equal(t, contractOrigin+"~session", write.Session.ID)
	require.Len(t, write.Messages, 1)
	assert.Equal(t, "hello", write.Messages[0].Content)
}

func TestLoadImportedSessionDefersOversizedFutureSegment(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	var segment strings.Builder
	for range productionArtifactLimits().segmentMessages + 1 {
		segment.WriteString(
			"{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":3}\n",
		)
	}
	segmentHash := createHashedImportArtifact(
		t, store, KindSegments, ".ndjson", []byte(segment.String()),
	)
	m := importTestManifest("session")
	m.Session.MessageCount = 1
	m.Session.UserMessageCount = 1
	m.Segments = []string{segmentHash}
	manifestHash := createImportTestManifest(t, store, m, false)

	_, outcome, err := loadImportedSession(
		t.Context(), database, store, contractOrigin,
		contractOrigin+"~session", manifestHash,
		productionArtifactLimits(),
	)
	assert.Equal(t, importClosureDeferred, outcome)
	require.ErrorIs(t, err, errFutureArtifactVersion)
	var future *futureArtifactVersionError
	require.ErrorAs(t, err, &future)
	assert.Equal(t, Kind(KindSegments), future.Kind)
	assert.Equal(t, 3, future.Version)
}

func TestLoadImportedSessionDefersMissingAndFutureDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prepare    func(*testing.T, ArtifactStore) string
		futureKind Kind
	}{
		{
			name: "missing manifest",
			prepare: func(*testing.T, ArtifactStore) string {
				return strings.Repeat("a", 64)
			},
		},
		{
			name: "missing segment",
			prepare: func(t *testing.T, store ArtifactStore) string {
				m := importTestManifest("session")
				m.Segments = []string{strings.Repeat("b", 64)}
				return createImportTestManifest(t, store, m, false)
			},
		},
		{
			name: "future manifest",
			prepare: func(t *testing.T, store ArtifactStore) string {
				body := []byte(`{"origin":"contract-a1b2c3","v":4}`)
				return createHashedImportArtifact(
					t, store, KindManifests, ".json", body,
				)
			},
			futureKind: KindManifests,
		},
		{
			name: "future segment",
			prepare: func(t *testing.T, store ArtifactStore) string {
				segment := []byte(
					"{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":3}\n",
				)
				segmentHash := createHashedImportArtifact(
					t, store, KindSegments, ".ndjson", segment,
				)
				m := importTestManifest("session")
				m.Segments = []string{segmentHash}
				return createImportTestManifest(t, store, m, false)
			},
			futureKind: KindSegments,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			manifestHash := tc.prepare(t, store)

			_, outcome, err := loadImportedSession(
				t.Context(), database, store, contractOrigin,
				contractOrigin+"~session", manifestHash,
				productionArtifactLimits(),
			)
			assert.Equal(t, importClosureDeferred, outcome)
			if tc.futureKind == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, errFutureArtifactVersion)
			var future *futureArtifactVersionError
			require.ErrorAs(t, err, &future)
			assert.Equal(t, tc.futureKind, future.Kind)
		})
	}
}

func TestLoadImportedSessionQuarantinesInvalidStatDependency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind Kind
	}{
		{name: "manifest", kind: KindManifests},
		{name: "segment", kind: KindSegments},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := testExportDB(t)
			base := newTestArtifactStore(t)
			m := importTestManifest("session")
			manifestHash := createImportTestClosure(t, base, &m, []db.Message{{
				Ordinal: 0, Role: "user", Content: "one",
			}})
			ref := requireContractRef(
				t, contractOrigin, KindManifests, manifestHash+".json",
			)
			if tc.kind == KindSegments {
				ref = requireContractRef(
					t, contractOrigin, KindSegments, m.Segments[0]+".ndjson",
				)
			}
			store := &failingImportStatStore{
				ArtifactStore: base,
				failRef:       ref,
				err:           fmt.Errorf("%w: corrupt catalog", ErrArtifactCorrupt),
			}

			_, outcome, err := loadImportedSession(
				t.Context(), database, store, contractOrigin,
				contractOrigin+"~session", manifestHash,
				productionArtifactLimits(),
			)
			require.NoError(t, err)
			assert.Equal(t, importClosureInvalid, outcome)
			_, err = base.Stat(t.Context(), ref)
			assert.ErrorIs(t, err, ErrArtifactNotFound)
		})
	}
}

func TestLoadImportedSessionQuarantinesPersistenceInvariantViolations(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, ArtifactStore) (manifest, string)
	}{
		{
			name: "duplicate message ordinals",
			prepare: func(t *testing.T, store ArtifactStore) (manifest, string) {
				m := importTestManifest("session")
				hash := createImportTestClosure(t, store, &m, []db.Message{
					{Ordinal: 0, Role: "user", Content: "one"},
					{Ordinal: 0, Role: "assistant", Content: "two"},
				})
				return m, hash
			},
		},
		{
			name: "manifest message count mismatch",
			prepare: func(t *testing.T, store ArtifactStore) (manifest, string) {
				m := importTestManifest("session")
				segment, err := encodeSegment([]db.Message{{
					Ordinal: 0, Role: "user", Content: "one",
				}})
				require.NoError(t, err)
				segmentHash := createHashedImportArtifact(
					t, store, KindSegments, ".ndjson", segment,
				)
				m.Segments = []string{segmentHash}
				m.Session.MessageCount = 2
				m.Session.UserMessageCount = 1
				hash := createImportTestManifest(t, store, m, false)
				return m, hash
			},
		},
		{
			name: "manifest user message count mismatch",
			prepare: func(t *testing.T, store ArtifactStore) (manifest, string) {
				m := importTestManifest("session")
				createImportTestClosure(t, store, &m, []db.Message{{
					Ordinal: 0, Role: "user", Content: "one",
				}})
				m.Session.UserMessageCount = 0
				hash := createImportTestManifest(t, store, m, false)
				return m, hash
			},
		},
		{
			name: "duplicate nonempty usage key",
			prepare: func(t *testing.T, store ArtifactStore) (manifest, string) {
				m := importTestManifest("session")
				m.UsageEvents = []artifactUsageEvent{
					{Source: "provider", DedupKey: "same"},
					{Source: "provider", DedupKey: "same"},
				}
				hash := createImportTestClosure(t, store, &m, []db.Message{{
					Ordinal: 0, Role: "user", Content: "one",
				}})
				return m, hash
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			_, manifestHash := tc.prepare(t, store)
			manifestRef := requireContractRef(
				t, contractOrigin, KindManifests, manifestHash+".json",
			)

			_, outcome, err := loadImportedSession(
				t.Context(), database, store, contractOrigin,
				contractOrigin+"~session", manifestHash,
				productionArtifactLimits(),
			)
			require.NoError(t, err)
			assert.Equal(t, importClosureInvalid, outcome)
			_, err = store.Stat(t.Context(), manifestRef)
			assert.ErrorIs(t, err, ErrArtifactNotFound)
		})
	}
}

func TestLoadImportedSessionQuarantinesInvalidCompleteDependency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, ArtifactStore) (string, Ref)
	}{
		{
			name: "wrong manifest origin",
			prepare: func(t *testing.T, store ArtifactStore) (string, Ref) {
				m := importTestManifest("session")
				m.Origin = "another-a1b2c3"
				body, err := canonicalJSON(m)
				require.NoError(t, err)
				hash := createHashedImportArtifact(
					t, store, KindManifests, ".json", body,
				)
				return hash, requireContractRef(
					t, contractOrigin, KindManifests, hash+".json",
				)
			},
		},
		{
			name: "native ID contains separator",
			prepare: func(t *testing.T, store ArtifactStore) (string, Ref) {
				m := importTestManifest("bad~session")
				m.Session.ID = "bad~session"
				body, err := canonicalJSON(m)
				require.NoError(t, err)
				hash := createHashedImportArtifact(
					t, store, KindManifests, ".json", body,
				)
				return hash, requireContractRef(
					t, contractOrigin, KindManifests, hash+".json",
				)
			},
		},
		{
			name: "invalid segment",
			prepare: func(t *testing.T, store ArtifactStore) (string, Ref) {
				segment := []byte("{not-json}\n")
				segmentHash := createHashedImportArtifact(
					t, store, KindSegments, ".ndjson", segment,
				)
				m := importTestManifest("session")
				m.Segments = []string{segmentHash}
				manifestHash := createImportTestManifest(t, store, m, false)
				return manifestHash, requireContractRef(
					t, contractOrigin, KindSegments, segmentHash+".ndjson",
				)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			manifestHash, invalidRef := tc.prepare(t, store)

			_, outcome, err := loadImportedSession(
				t.Context(), database, store, contractOrigin,
				contractOrigin+"~session", manifestHash,
				productionArtifactLimits(),
			)
			require.NoError(t, err)
			assert.Equal(t, importClosureInvalid, outcome)
			_, err = store.Stat(t.Context(), invalidRef)
			assert.ErrorIs(t, err, ErrArtifactNotFound)
		})
	}
}

func TestLoadImportedSessionAcceptsNoncanonicalManifestAndSegment(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	segment := []byte(
		" { \"v\" : 1, \"role\" : \"user\", \"ordinal\" : 0, " +
			"\"content\" : \"hello\" }\n",
	)
	segmentHash := createHashedImportArtifact(
		t, store, KindSegments, ".ndjson", segment,
	)
	m := importTestManifest("session")
	m.Session.MessageCount = 1
	m.Session.UserMessageCount = 1
	m.Segments = []string{segmentHash}
	canonical, err := canonicalJSON(m)
	require.NoError(t, err)
	var indented bytes.Buffer
	require.NoError(t, jsonIndent(&indented, canonical))
	manifestHash := createHashedImportArtifact(
		t, store, KindManifests, ".json", indented.Bytes(),
	)

	write, outcome, err := loadImportedSession(
		t.Context(), database, store, contractOrigin,
		contractOrigin+"~session", manifestHash,
		productionArtifactLimits(),
	)
	require.NoError(t, err)
	assert.Equal(t, importClosureComplete, outcome)
	require.Len(t, write.Messages, 1)
	assert.Equal(t, "hello", write.Messages[0].Content)
}

func TestLoadImportedSessionEnforcesAggregateLimits(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	m := importTestManifest("session")
	manifestHash := createImportTestClosure(t, store, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "one",
	}})
	limits := productionArtifactLimits()
	limits.sessionMessages = 0

	_, outcome, err := loadImportedSession(
		t.Context(), database, store, contractOrigin,
		contractOrigin+"~session", manifestHash, limits,
	)
	require.NoError(t, err)
	assert.Equal(t, importClosureInvalid, outcome)
}

func TestLoadImportedSessionAggregateBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*artifactLimits)
		message   func(int) []db.Message
	}{
		{
			name: "messages",
			configure: func(limits *artifactLimits) {
				limits.sessionMessages = 2
			},
			message: func(count int) []db.Message {
				messages := make([]db.Message, count)
				for ordinal := range messages {
					messages[ordinal] = db.Message{
						Ordinal: ordinal, Role: "assistant",
					}
				}
				return messages
			},
		},
		{
			name: "tool calls",
			configure: func(limits *artifactLimits) {
				limits.sessionToolCalls = 2
			},
			message: func(count int) []db.Message {
				return []db.Message{{
					Ordinal: 0, Role: "assistant",
					ToolCalls: make([]db.ToolCall, count),
				}}
			},
		},
		{
			name: "result events",
			configure: func(limits *artifactLimits) {
				limits.sessionResultEvents = 2
			},
			message: func(count int) []db.Message {
				return []db.Message{{
					Ordinal: 0, Role: "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: make([]db.ToolResultEvent, count),
					}},
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, count := range []int{2, 3} {
				database := testExportDB(t)
				store := newTestArtifactStore(t)
				m := importTestManifest("session")
				manifestHash := createImportTestClosure(
					t, store, &m, tc.message(count),
				)
				limits := productionArtifactLimits()
				tc.configure(&limits)

				_, outcome, err := loadImportedSession(
					t.Context(), database, store, contractOrigin,
					contractOrigin+"~session", manifestHash, limits,
				)
				require.NoError(t, err)
				if count == 2 {
					assert.Equal(t, importClosureComplete, outcome)
				} else {
					assert.Equal(t, importClosureInvalid, outcome)
				}
			}
		})
	}

	t.Run("decoded bytes", func(t *testing.T) {
		database := testExportDB(t)
		store := newTestArtifactStore(t)
		m := importTestManifest("session")
		messages := []db.Message{{
			Ordinal: 0, Role: "assistant", Content: "bounded",
		}}
		manifestHash := createImportTestClosure(t, store, &m, messages)
		segmentBody, err := encodeSegment(messages)
		require.NoError(t, err)

		for _, delta := range []int64{0, -1} {
			limits := productionArtifactLimits()
			limits.sessionDecodedBytes = int64(len(segmentBody)) + delta
			_, outcome, err := loadImportedSession(
				t.Context(), database, store, contractOrigin,
				contractOrigin+"~session", manifestHash, limits,
			)
			require.NoError(t, err)
			if delta == 0 {
				assert.Equal(t, importClosureComplete, outcome)
			} else {
				assert.Equal(t, importClosureInvalid, outcome)
			}
		}
	})
}

func TestLoadImportedSessionPropagatesOperationalStoreError(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	base := newTestArtifactStore(t)
	m := importTestManifest("session")
	manifestHash := createImportTestClosure(t, base, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "one",
	}})
	manifestRef := requireContractRef(
		t, contractOrigin, KindManifests, manifestHash+".json",
	)
	operational := errors.New("archive unavailable")
	store := &failingImportOpenStore{
		ArtifactStore: base, failRef: manifestRef, err: operational,
	}

	_, outcome, err := loadImportedSession(
		t.Context(), database, store, contractOrigin,
		contractOrigin+"~session", manifestHash,
		productionArtifactLimits(),
	)
	assert.Equal(t, importClosureDeferred, outcome)
	assert.ErrorIs(t, err, operational)
}

func importTestManifest(nativeID string) manifest {
	return manifest{
		Version: manifestFormatVersion, Origin: contractOrigin,
		NativeSessionID: nativeID,
		Session: manifestSession{
			ID: nativeID, Project: "project", Machine: contractOrigin,
			Agent: "claude", CreatedAt: "2026-07-28T00:00:00Z",
		},
		DataVersion: 1, Generation: 1,
	}
}

func createImportTestClosure(
	t *testing.T,
	store ArtifactStore,
	m *manifest,
	messages []db.Message,
) string {
	t.Helper()
	m.Session.MessageCount = len(messages)
	m.Session.UserMessageCount = 0
	for _, message := range messages {
		if message.Role == "user" && !message.IsSystem {
			m.Session.UserMessageCount++
		}
	}
	segment, err := encodeSegment(messages)
	require.NoError(t, err)
	segmentHash := createHashedImportArtifact(
		t, store, KindSegments, ".ndjson", segment,
	)
	m.Segments = []string{segmentHash}
	return createImportTestManifest(t, store, *m, false)
}

func createImportTestManifest(
	t *testing.T, store ArtifactStore, m manifest, indented bool,
) string {
	t.Helper()
	body, err := canonicalJSON(m)
	require.NoError(t, err)
	if indented {
		var out bytes.Buffer
		require.NoError(t, jsonIndent(&out, body))
		body = out.Bytes()
	}
	return createHashedImportArtifact(
		t, store, KindManifests, ".json", body,
	)
}

func createHashedImportArtifact(
	t *testing.T,
	store ArtifactStore,
	kind Kind,
	extension string,
	body []byte,
) string {
	t.Helper()
	identity := identityForBytes(t, body)
	ref := requireContractRef(
		t, contractOrigin, kind, identity.SHA256+extension,
	)
	createContractArtifact(t, store, ref, body)
	return identity.SHA256
}

func jsonIndent(destination *bytes.Buffer, source []byte) error {
	formatted := jsontext.Value(source).Clone()
	if err := formatted.Indent(jsontext.WithIndent("  ")); err != nil {
		return err
	}
	_, err := destination.Write(formatted)
	return err
}

type failingImportOpenStore struct {
	ArtifactStore
	failRef Ref
	err     error
}

type failingImportStatStore struct {
	ArtifactStore
	failRef Ref
	err     error
}

func (s *failingImportStatStore) Stat(
	ctx context.Context, ref Ref,
) (Entry, error) {
	if ref == s.failRef {
		return Entry{}, s.err
	}
	return s.ArtifactStore.Stat(ctx, ref)
}

func (s *failingImportOpenStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref == s.failRef {
		return Entry{}, nil, s.err
	}
	return s.ArtifactStore.Open(ctx, ref)
}
