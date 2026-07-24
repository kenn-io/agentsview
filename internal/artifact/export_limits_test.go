package artifact

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func exportSessionToTestStoreWithLimits(
	t *testing.T,
	ctx context.Context,
	database *db.DB,
	store ArtifactStore,
	origin string,
	limits artifactLimits,
) (string, bool, error) {
	t.Helper()
	sess, err := database.GetSessionFull(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	messages, err := database.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	usage, err := database.GetUsageEvents(ctx, "sess-1")
	require.NoError(t, err)
	return exportLoadedSessionToStore(ctx, store, origin, sess, messages, usage, limits)
}

// latestTestStoreManifest is a test-only stand-in for the reference's
// latestTestStoreManifest, rewritten to use latestStoreCheckpointForTest
// (export_test.go) instead of peer.go's latestStoreCheckpointSummary (out of
// PR2 scope; see plan hazard 6).
func latestTestStoreManifest(t *testing.T, store ArtifactStore, origin string) manifest {
	t.Helper()
	cp := latestStoreCheckpointForTest(t, store, origin)
	hash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, hash)
	ref, err := NewRef(origin, KindManifests, hash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	return m
}

func testStoreManifestMessages(t *testing.T, store ArtifactStore, origin string, m manifest) []db.Message {
	t.Helper()
	var messages []db.Message
	for _, hash := range m.Segments {
		ref, err := NewRef(origin, KindSegments, hash+".ndjson")
		require.NoError(t, err)
		segment, err := decodeSegment(readContractArtifact(t, store, ref))
		require.NoError(t, err)
		messages = append(messages, segment...)
	}
	return messages
}

func assertNoPublishedAuthority(t *testing.T, store ArtifactStore, origin string) {
	t.Helper()
	for _, kind := range []Kind{KindManifests, KindCheckpoints} {
		page, err := firstStoreEntryPage(t.Context(), store, origin, kind, maxArtifactListPageSize)
		require.NoError(t, err)
		assert.Empty(t, page.Items, "unexpected published %s artifact", kind)
		require.Empty(t, page.Next)
	}
}
func TestExportRejectsNestedAmplificationBeforePublication(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		wantError string
	}{
		{
			name: "too many tool calls in one message",
			message: db.Message{
				ToolCalls: make([]db.ToolCall, 257),
			},
			wantError: "tool call limit exceeded for message ordinal 0",
		},
		{
			name: "too many result events in one tool call",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: make([]db.ToolResultEvent, 1_025),
				}},
			},
			wantError: "result event limit exceeded for tool call 0 in message ordinal 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))

			_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func TestExportChunksOnAggregateNestedLimitsWithSmallLimits(t *testing.T) {
	tests := []struct {
		name         string
		resultEvents []db.ToolResultEvent
		configure    func(*artifactLimits)
	}{
		{
			name: "tool calls per segment",
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 2
			},
		},
		{
			name:         "result events per segment",
			resultEvents: []db.ToolResultEvent{{EventIndex: 0}},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 2
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 3)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: tt.resultEvents,
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			manifestHash, changed, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.NoError(t, err)
			assert.True(t, changed)
			manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
			require.NoError(t, err)
			m, err := decodeManifestWithLimits(
				readContractArtifact(t, store, manifestRef), productionArtifactLimits(),
			)
			require.NoError(t, err)
			require.Len(t, m.Segments, 2)
			got := testStoreManifestMessages(t, store, origin, m)
			require.Len(t, got, 3)
			for ordinal := range got {
				assert.Equal(t, ordinal, got[ordinal].Ordinal)
				require.Len(t, got[ordinal].ToolCalls, 1)
				assert.Len(t, got[ordinal].ToolCalls[0].ResultEvents, len(tt.resultEvents))
			}
		})
	}
}

func TestExportRejectsMessageThatCannotFitNestedSegmentLimits(t *testing.T) {
	tests := []struct {
		name      string
		message   db.Message
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{}, {}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentToolCalls = 1
			},
			wantError: "2 tool calls",
		},
		{
			name: "one tool result history cannot split across segments",
			message: db.Message{
				ToolCalls: []db.ToolCall{{
					ResultEvents: []db.ToolResultEvent{{EventIndex: 0}, {EventIndex: 1}},
				}},
			},
			configure: func(limits *artifactLimits) {
				limits.segmentResultEvents = 1
			},
			wantError: "2 result events",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			message := tt.message
			message.SessionID = "sess-1"
			message.Ordinal = 0
			message.Role = "assistant"
			require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{message}))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot fit in one segment")
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func TestExportRejectsSessionNestedLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "tool calls per session",
			configure: func(limits *artifactLimits) {
				limits.sessionToolCalls = 1
			},
			wantError: "session tool call limit",
		},
		{
			name: "result events per session",
			configure: func(limits *artifactLimits) {
				limits.sessionResultEvents = 1
			},
			wantError: "session result event limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			msgs := make([]db.Message, 2)
			for ordinal := range msgs {
				msgs[ordinal] = db.Message{
					SessionID: "sess-1",
					Ordinal:   ordinal,
					Role:      "assistant",
					ToolCalls: []db.ToolCall{{
						ResultEvents: []db.ToolResultEvent{{EventIndex: 0}},
					}},
				}
			}
			require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))
			limits := productionArtifactLimits()
			tt.configure(&limits)

			_, _, err := exportSessionToTestStoreWithLimits(
				t, ctx, database, store, origin, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedArtifacts(t, store, origin)
		})
	}
}

func TestExportChunksOnMessageRecordLimit(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 4_097)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	m := latestTestStoreManifest(t, store, origin)
	require.Len(t, m.Segments, 2)
	got := testStoreManifestMessages(t, store, origin, m)
	assert.Len(t, got, 4_097)
}

func TestExportRejectsOversizedGeneratedManifestBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha", func(sess *db.Session) {
		first := strings.Repeat("x", int(manifestDecodedLimit))
		sess.FirstMessage = &first
	})

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generated manifest exceeds")
	assertNoPublishedAuthority(t, store, origin)
}

func TestExportRejectsSessionMessageAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs := make([]db.Message, 32_769)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "sess-1", Ordinal: i, Role: "user"}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session message limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportRejectsUsageEventAmplificationBeforePublication(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	events := make([]db.UsageEvent, 32_769)
	for i := range events {
		events[i] = db.UsageEvent{
			SessionID: "sess-1",
			Source:    "fixture",
			DedupKey:  fmt.Sprintf("usage-%d", i),
		}
	}
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", events))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage event limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportSessionRejectsAggregateLimitsBeforeWritingWithSmallLimits(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*artifactLimits)
		wantError string
	}{
		{
			name: "decoded bytes",
			configure: func(limits *artifactLimits) {
				limits.sessionDecodedBytes = 1
			},
			wantError: "session decoded byte limit",
		},
		{
			name: "segment references",
			configure: func(limits *artifactLimits) {
				limits.segmentMessages = 1
				limits.manifestSegments = 1
			},
			wantError: "segment reference limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			database := testExportDB(t)
			store := newTestArtifactStore(t)
			origin := "laptop-a1b2c3"
			seedSession(t, database, "sess-1", "alpha")
			limits := productionArtifactLimits()
			tt.configure(&limits)

			sess, err := database.GetSessionFull(ctx, "sess-1")
			require.NoError(t, err)
			require.NotNil(t, sess)
			messages, err := database.GetAllMessages(ctx, "sess-1")
			require.NoError(t, err)
			_, _, err = exportLoadedSessionToStore(
				ctx, store, origin, sess, messages, nil, limits,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
			assertNoPublishedAuthority(t, store, origin)
		})
	}
}

func TestExportChunksLargeMultiMessageSessionInOrder(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 9<<20)
	msgs := make([]db.Message, 4)
	for i := range msgs {
		msgs[i] = db.Message{
			SessionID:     "sess-1",
			Ordinal:       i,
			Role:          "user",
			Content:       content,
			ContentLength: len(content),
		}
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", msgs))

	exportResult, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, exportResult.ExportedSessions)
	m := latestTestStoreManifest(t, store, origin)
	require.Len(t, m.Segments, 2)

	got := testStoreManifestMessages(t, store, origin, m)
	require.Len(t, got, 4)
	for i := range got {
		assert.Equal(t, i, got[i].Ordinal)
		assert.Equal(t, content, got[i].Content)
	}
}

func TestExportRejectsSingleEncodedRecordAboveReadableLimit(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	content := strings.Repeat("x", 64<<20)
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID:     "sess-1",
		Ordinal:       0,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
	}}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encoded message record")
	assert.Contains(t, err.Error(), "67108864-byte readable limit")
	assertNoPublishedArtifacts(t, store, origin)
}

func TestExportPreservesSmallSingleSegmentHash(t *testing.T) {
	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	msgs, err := database.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	segmentData, err := encodeSegment(canonicalMessages(msgs))
	require.NoError(t, err)
	wantHash := hashHex(segmentData)

	_, err = ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	m := latestTestStoreManifest(t, store, origin)
	assert.Equal(t, []string{wantHash}, m.Segments)
}
