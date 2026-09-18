package sync

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestRecomputeHeapBytesCountsLoadedText(t *testing.T) {
	callIndex := 0
	eventIndex := 1
	msgs := []db.Message{{
		SessionID: "s1",
		Ordinal:   0,
		Content:   "message",
		ToolCalls: []db.ToolCall{{
			ToolName:      "Bash",
			Category:      "Bash",
			ToolUseID:     "tool-1",
			InputJSON:     `{"cmd":"echo"}`,
			ResultContent: "legacy result",
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "tool-1",
				Status:    "completed",
				Content:   "event result",
			}},
		}},
	}}
	findings := []db.SecretFinding{{
		SessionID:      "s1",
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "tool_result_event",
		MessageOrdinal: 0,
		CallIndex:      &callIndex,
		EventIndex:     &eventIndex,
		RedactedMatch:  "AKIA************PLJM",
		RulesVersion:   "v1",
	}}

	got := recomputeHeapBytes(msgs, findings)

	assert.GreaterOrEqual(t, got, len("message")+len(`{"cmd":"echo"}`)+
		len("legacy result")+len("event result")+
		len("AKIA************PLJM"))
}

func TestBackfillSignalComputerReleasesAccumulatedHeap(t *testing.T) {
	require := require.New(t)

	fx := newEngineFixture(t)
	ctx := t.Context()
	const id = "s1"
	require.NoError(fx.db.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(fx.db.ReplaceSessionMessages(id, []db.Message{{
		SessionID: id,
		Ordinal:   0,
		Role:      "user",
		Content:   strings.Repeat("x", 128),
	}}))

	oldThreshold := recomputeHeapReleaseThreshold
	oldFree := freeRecomputeHeap
	defer func() {
		recomputeHeapReleaseThreshold = oldThreshold
		freeRecomputeHeap = oldFree
	}()
	recomputeHeapReleaseThreshold = 1
	var calls int
	freeRecomputeHeap = func() {
		calls++
	}

	compute := fx.engine.BackfillSignalComputer()
	require.NoError(compute(ctx, id))

	assert.Equal(t, 1, calls)
}

func TestRecomputeSignalsDoesNotReleaseHeapDirectly(t *testing.T) {
	require := require.New(t)

	fx := newEngineFixture(t)
	ctx := t.Context()
	const id = "s1"
	require.NoError(fx.db.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(fx.db.ReplaceSessionMessages(id, []db.Message{{
		SessionID: id,
		Ordinal:   0,
		Role:      "user",
		Content:   strings.Repeat("x", 128),
	}}))

	oldThreshold := recomputeHeapReleaseThreshold
	oldFree := freeRecomputeHeap
	defer func() {
		recomputeHeapReleaseThreshold = oldThreshold
		freeRecomputeHeap = oldFree
	}()
	recomputeHeapReleaseThreshold = 1
	var calls int
	freeRecomputeHeap = func() {
		calls++
	}

	require.NoError(fx.engine.RecomputeSignals(ctx, id))

	assert.Zero(t, calls)
}

func TestBackfillSignalsRecomputesVersion2TerminalAPIErrorSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fx := newEngineFixture(t)
	ctx := t.Context()
	const id = "api-stale"
	endedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)

	require.NoError(fx.db.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 2, UserMessageCount: 1, EndedAt: &endedAt,
	}))
	require.NoError(fx.db.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello"},
		{
			SessionID: id, Ordinal: 1, Role: "assistant",
			Content: "API Error: Unable to connect to API (ConnectionRefused)",
		},
	}))
	require.NoError(fx.db.UpdateSessionSignals(id, db.SessionSignalUpdate{
		Outcome:           "completed",
		OutcomeConfidence: "medium",
		QualitySignals: db.QualitySignals{
			Version: 2,
		},
	}))

	require.NoError(fx.db.BackfillSignals(ctx, fx.engine.BackfillSignalComputer()))

	sess, err := fx.db.GetSessionFull(ctx, id)
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal(db.CurrentQualitySignalVersion, sess.QualitySignalVersion)
	assert.Equal("errored", sess.Outcome)
	assert.Equal("medium", sess.OutcomeConfidence)
}

func TestRecomputeHeapReleaserSkipsSmallSessions(t *testing.T) {
	oldThreshold := recomputeHeapReleaseThreshold
	oldFree := freeRecomputeHeap
	defer func() {
		recomputeHeapReleaseThreshold = oldThreshold
		freeRecomputeHeap = oldFree
	}()
	recomputeHeapReleaseThreshold = 128
	var calls int
	freeRecomputeHeap = func() {
		calls++
	}

	var release recomputeHeapReleaser
	release.Account(127)

	assert.Zero(t, calls)
}

func TestRecomputeHeapReleaserAccumulatesBeforeRelease(t *testing.T) {
	oldThreshold := recomputeHeapReleaseThreshold
	oldFree := freeRecomputeHeap
	defer func() {
		recomputeHeapReleaseThreshold = oldThreshold
		freeRecomputeHeap = oldFree
	}()
	recomputeHeapReleaseThreshold = 128
	var calls int
	freeRecomputeHeap = func() {
		calls++
	}

	var release recomputeHeapReleaser
	release.Account(64)
	release.Account(63)
	assert.Zero(t, calls)

	release.Account(1)
	assert.Equal(t, 1, calls)
}
