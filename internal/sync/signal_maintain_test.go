package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// fakeSignalQuery is a minimal in-memory db.SignalQuery for maintainer
// unit tests. It lets the maintainer run without a live transaction.
type fakeSignalQuery struct {
	sess     *db.Session
	state    db.SessionSignalState
	hasState bool
	revision string
}

func (f *fakeSignalQuery) Session(context.Context) (*db.Session, error) {
	return f.sess, nil
}

func (f *fakeSignalQuery) TranscriptRevision(context.Context) (string, error) {
	return f.revision, nil
}

func (f *fakeSignalQuery) SignalState(
	context.Context,
) (db.SessionSignalState, bool, error) {
	return f.state, f.hasState, nil
}

func (f *fakeSignalQuery) TrailingToolCalls(
	context.Context, int,
) ([]db.ToolCallSignalFact, error) {
	return nil, nil
}

func (f *fakeSignalQuery) ToolCallsByUseID(
	context.Context, []string,
) ([]db.ToolCallSignalFact, error) {
	return nil, nil
}

func (f *fakeSignalQuery) CallResultEvents(
	context.Context, int, int,
) ([]db.ToolResultEvent, error) {
	return nil, nil
}

// newTestMaintainer builds a maintainer stamped with the current quality
// and secrets rules versions, mirroring newIncrementalSignalMaintainer.
func newTestMaintainer(
	preWriteRevision, preWriteSecretsVersion string, appended []db.Message,
) *incrementalSignalMaintainer {
	return &incrementalSignalMaintainer{
		sessionID:              "s1",
		appended:               appended,
		preWriteRevision:       preWriteRevision,
		preWriteSecretsVersion: preWriteSecretsVersion,
		qualitySignalVersion:   db.CurrentQualitySignalVersion,
		secretsRulesVersion:    secrets.DefiniteRulesVersion(),
	}
}

func currentStateBlob(t *testing.T) []byte {
	t.Helper()
	state := signals.SeedIncrementalState(
		nil, nil, "", "", nil, nil, 0, 0,
	)
	blob, err := state.MarshalBinary()
	require.NoError(t, err)
	return blob
}

// TestIncrementalMaintainerDeclinesStaleVersions pins the version gate: a
// state or session whose quality/secret versions are not current must
// decline (nil delta) so the debounced full recompute reseeds — even when
// the stored state and session versions agree with each other but are both
// stale.
func TestIncrementalMaintainerDeclinesStaleVersions(t *testing.T) {
	blob := currentStateBlob(t)
	cases := []struct {
		name            string
		sessQuality     int
		preWriteSecrets string
		storedSignal    int
	}{
		{
			name:            "both versions stale but equal",
			sessQuality:     db.CurrentQualitySignalVersion - 1,
			preWriteSecrets: secrets.DefiniteRulesVersion(),
			storedSignal:    db.CurrentQualitySignalVersion - 1,
		},
		{
			name:            "stale stored signal version",
			sessQuality:     db.CurrentQualitySignalVersion,
			preWriteSecrets: secrets.DefiniteRulesVersion(),
			storedSignal:    db.CurrentQualitySignalVersion - 1,
		},
		{
			name:            "stale session quality version",
			sessQuality:     db.CurrentQualitySignalVersion - 1,
			preWriteSecrets: secrets.DefiniteRulesVersion(),
			storedSignal:    db.CurrentQualitySignalVersion,
		},
		{
			name:            "stale pre-write secrets version",
			sessQuality:     db.CurrentQualitySignalVersion,
			preWriteSecrets: "stale-secrets-version",
			storedSignal:    db.CurrentQualitySignalVersion,
		},
		{
			name:            "un-stamped pre-write secrets version",
			sessQuality:     db.CurrentQualitySignalVersion,
			preWriteSecrets: "",
			storedSignal:    db.CurrentQualitySignalVersion,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestMaintainer("rev", tc.preWriteSecrets, nil)
			q := &fakeSignalQuery{
				sess: &db.Session{
					QualitySignalVersion: tc.sessQuality,
				},
				state: db.SessionSignalState{
					State:              blob,
					TranscriptRevision: "rev",
					SignalVersion:      tc.storedSignal,
				},
				hasState: true,
				revision: "rev",
			}
			delta, err := m.MaintainTx(context.Background(), q)
			require.NoError(t, err)
			assert.Nil(t, delta, "stale versions must decline")
		})
	}
}

// TestIncrementalMaintainerProceedsCurrentVersions pins the positive case:
// with a current transcript revision, current quality version, and current
// secrets rules version, the maintainer must produce a delta.
func TestIncrementalMaintainerProceedsCurrentVersions(t *testing.T) {
	m := newTestMaintainer("rev", secrets.DefiniteRulesVersion(), nil)
	q := &fakeSignalQuery{
		sess: &db.Session{
			QualitySignalVersion: db.CurrentQualitySignalVersion,
		},
		state: db.SessionSignalState{
			State:              currentStateBlob(t),
			TranscriptRevision: "rev",
			SignalVersion:      db.CurrentQualitySignalVersion,
		},
		hasState: true,
		revision: "rev",
	}
	delta, err := m.MaintainTx(context.Background(), q)
	require.NoError(t, err)
	require.NotNil(t, delta,
		"current versions with a matching revision must proceed")
}

// TestIncrementalMaintainerCompactionExplicitBoundaryParity pins parity
// between the full compute and the incremental fold for compaction count:
// an explicit compact boundary suppresses token-drop compactions, so a
// session with both must count only the boundary.
func TestIncrementalMaintainerCompactionExplicitBoundaryParity(t *testing.T) {
	boundary := db.Message{SessionID: "s1", Ordinal: 0, Role: "assistant",
		IsCompactBoundary: true}
	preCtx := db.Message{SessionID: "s1", Ordinal: 1, Role: "assistant",
		HasContextTokens: true, ContextTokens: 1000}
	drop := db.Message{SessionID: "s1", Ordinal: 2, Role: "assistant",
		HasContextTokens: true, ContextTokens: 500}

	t.Run("explicit boundary suppresses token drop", func(t *testing.T) {
		full := computeSignalsFromMessages(
			db.Session{}, []db.Message{boundary, preCtx, drop},
		)
		require.Equal(t, 1, full.CompactionCount)

		state := signals.SeedIncrementalState(
			nil, []int{0}, "", "", nil, nil, 0, 1000,
		)
		blob, err := state.MarshalBinary()
		require.NoError(t, err)

		m := newTestMaintainer("rev", secrets.DefiniteRulesVersion(), []db.Message{drop})
		q := &fakeSignalQuery{
			sess: &db.Session{
				CompactionCount:      1,
				QualitySignalVersion: db.CurrentQualitySignalVersion,
			},
			state: db.SessionSignalState{
				State:              blob,
				TranscriptRevision: "rev",
				SignalVersion:      db.CurrentQualitySignalVersion,
			},
			hasState: true,
			revision: "rev",
		}
		delta, err := m.MaintainTx(context.Background(), q)
		require.NoError(t, err)
		require.NotNil(t, delta)
		assert.Equal(t, full.CompactionCount, delta.Update.CompactionCount,
			"explicit boundaries must suppress token-drop compactions")
	})

	t.Run("no boundary counts token drop", func(t *testing.T) {
		full := computeSignalsFromMessages(
			db.Session{}, []db.Message{preCtx, drop},
		)
		require.Equal(t, 1, full.CompactionCount)

		state := signals.SeedIncrementalState(
			nil, nil, "", "", nil, nil, 0, 1000,
		)
		blob, err := state.MarshalBinary()
		require.NoError(t, err)

		m := newTestMaintainer("rev", secrets.DefiniteRulesVersion(), []db.Message{drop})
		q := &fakeSignalQuery{
			sess: &db.Session{
				QualitySignalVersion: db.CurrentQualitySignalVersion,
			},
			state: db.SessionSignalState{
				State:              blob,
				TranscriptRevision: "rev",
				SignalVersion:      db.CurrentQualitySignalVersion,
			},
			hasState: true,
			revision: "rev",
		}
		delta, err := m.MaintainTx(context.Background(), q)
		require.NoError(t, err)
		require.NotNil(t, delta)
		assert.Equal(t, full.CompactionCount, delta.Update.CompactionCount,
			"without a boundary the token drop must still be counted")
	})
}

// TestIncrementalMaintainerContextPressureSessionPeakParity pins parity
// for context pressure when the session-level peak exceeds per-message
// maxima: both paths must feed sess.PeakContextTokens into
// ComputeContextPressure and land the same ContextPressureMax.
func TestIncrementalMaintainerContextPressureSessionPeakParity(t *testing.T) {
	const model = "claude-sonnet-4-5"
	sess := db.Session{
		PeakContextTokens:    200_000,
		HasPeakContextTokens: true,
		QualitySignalVersion: db.CurrentQualitySignalVersion,
		SecretsRulesVersion:  secrets.DefiniteRulesVersion(),
	}
	msgs := []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant", Model: model,
		HasContextTokens: true, ContextTokens: 1000,
	}}
	full := computeSignalsFromMessages(sess, msgs)
	require.NotNil(t, full.ContextPressureMax)

	state := signals.SeedIncrementalState(
		nil, nil, "", "",
		map[string]int{model: 1}, map[string]int{model: 0}, 1, 0,
	)
	blob, err := state.MarshalBinary()
	require.NoError(t, err)

	m := newTestMaintainer("rev", secrets.DefiniteRulesVersion(), nil)
	q := &fakeSignalQuery{
		sess: &sess,
		state: db.SessionSignalState{
			State:              blob,
			TranscriptRevision: "rev",
			SignalVersion:      db.CurrentQualitySignalVersion,
		},
		hasState: true,
		revision: "rev",
	}
	delta, err := m.MaintainTx(context.Background(), q)
	require.NoError(t, err)
	require.NotNil(t, delta)
	require.NotNil(t, delta.Update.ContextPressureMax)
	assert.Equal(t, full.ContextPressureMax, delta.Update.ContextPressureMax,
		"session-level peak must feed context pressure in both paths")
}

// signalSnapshot captures the session signal columns for parity
// comparisons between the incremental maintainer and an authoritative
// full recompute.
type signalSnapshot struct {
	ToolFailureSignalCount   int
	ToolRetryCount           int
	EditChurnCount           int
	ConsecutiveFailureMax    int
	Outcome                  string
	OutcomeConfidence        string
	EndedWithRole            string
	FinalFailureStreak       int
	CompactionCount          int
	MidTaskCompactionCount   int
	ContextPressureMax       *float64
	HealthScore              *int
	HealthGrade              *string
	HasToolCalls             bool
	HasContextData           bool
	SecretLeakCount          int
	QualitySignalVersion     int
	ShortPromptCount         int
	UnstructuredStart        bool
	MissingSuccessCriteria   int
	MissingVerificationCount int
	DuplicatePromptCount     int
	NoCodeContextCount       int
	RunawayToolLoopCount     int
}

func snapshotSessionSignals(s *db.Session) signalSnapshot {
	return signalSnapshot{
		ToolFailureSignalCount:   s.ToolFailureSignalCount,
		ToolRetryCount:           s.ToolRetryCount,
		EditChurnCount:           s.EditChurnCount,
		ConsecutiveFailureMax:    s.ConsecutiveFailureMax,
		Outcome:                  s.Outcome,
		OutcomeConfidence:        s.OutcomeConfidence,
		EndedWithRole:            s.EndedWithRole,
		FinalFailureStreak:       s.FinalFailureStreak,
		CompactionCount:          s.CompactionCount,
		MidTaskCompactionCount:   s.MidTaskCompactionCount,
		ContextPressureMax:       s.ContextPressureMax,
		HealthScore:              s.HealthScore,
		HealthGrade:              s.HealthGrade,
		HasToolCalls:             s.HasToolCalls,
		HasContextData:           s.HasContextData,
		SecretLeakCount:          s.SecretLeakCount,
		QualitySignalVersion:     s.QualitySignalVersion,
		ShortPromptCount:         s.ShortPromptCount,
		UnstructuredStart:        s.UnstructuredStart,
		MissingSuccessCriteria:   s.MissingSuccessCriteriaCount,
		MissingVerificationCount: s.MissingVerificationCount,
		DuplicatePromptCount:     s.DuplicatePromptCount,
		NoCodeContextCount:       s.NoCodeContextCount,
		RunawayToolLoopCount:     s.RunawayToolLoopCount,
	}
}

const signalMaintainUUID = "019eb791-cf7d-75c1-8439-9ed74c122a01"

// TestIncrementalSignalMaintainerParityWithFullResync drives a Codex
// session through a full sync, an incremental late-tool-output append, and
// an authoritative full reparse, then proves the maintained signal columns
// and secret findings equal the full recompute. It also gates the delta
// path: zero GetAllMessages calls and secret-scan bytes bounded by the
// delta content.
func TestIncrementalSignalMaintainerParityWithFullResync(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+signalMaintainUUID+".jsonl",
	)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			signalMaintainUUID, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:01Z"),
		testjsonl.CodexMsgJSON(
			"assistant", "I will run the command.",
			"2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_0", nil, "2024-01-01T10:00:03Z",
		),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	sessionID := "codex:" + signalMaintainUUID

	// Baseline: the full sync seeded the compact state.
	state, ok, err := database.GetSessionSignalState(sessionID)
	require.NoError(t, err)
	require.True(t, ok, "full sync must seed the compact signal state")

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_1", nil, "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_0",
			"build passed AKIA7QHWN2DKR4FYPLJM",
			"2024-01-01T10:00:05Z",
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	loadsBefore := database.MessagesLoadCount()
	scanBytesBefore := SecretScanBytes()
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	// Delta gates: the maintained append must not load session history,
	// and the secret scan must stay within the delta's own content.
	assert.Equal(t, loadsBefore, database.MessagesLoadCount(),
		"maintained delta must not call GetAllMessages")
	assert.LessOrEqual(t, SecretScanBytes()-scanBytesBefore,
		int64(len(appended)),
		"secret scan bytes must not exceed the delta content")

	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.LastWriteIncremental,
		"the append must take the incremental path")
	require.Equal(t, db.CurrentQualitySignalVersion, sess.QualitySignalVersion,
		"maintenance must keep the signal version current")

	findings, err := database.SessionSecretFindings(
		context.Background(), sessionID,
	)
	require.NoError(t, err)
	require.Len(t, findings, 1, "the AWS key in the output must be found")
	assert.Equal(t, "tool_result_event", findings[0].LocationKind)

	incrementalSignals := snapshotSessionSignals(sess)

	// Authoritative rebuild: rewrite the file in place (same size),
	// drop the checkpoint, and bump the mtime so the engine takes the
	// full-parse replacement path.
	rewritten := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			signalMaintainUUID, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "heylo", "2024-01-01T10:00:01Z"),
		testjsonl.CodexMsgJSON(
			"assistant", "I will run the command.",
			"2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_0", nil, "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_1", nil, "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_0",
			"build passed AKIA7QHWN2DKR4FYPLJM",
			"2024-01-01T10:00:05Z",
		),
	)
	require.Equal(t, len(initial)+len(appended), len(rewritten))
	require.NoError(t, database.DeleteParserCheckpoint(sessionID))
	require.NoError(t, os.WriteFile(path, []byte(rewritten), 0o644))
	future := time.Now().Add(2 * time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))

	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	sess, err = database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.False(t, sess.LastWriteIncremental,
		"the same-size rewrite must take the full replacement path")

	fullFindings, err := database.SessionSecretFindings(
		context.Background(), sessionID,
	)
	require.NoError(t, err)
	assert.Equal(t, findings, fullFindings,
		"incremental findings must match the authoritative full resync")
	assert.Equal(t, incrementalSignals, snapshotSessionSignals(sess),
		"incremental signals must match the authoritative full resync")

	// The full resync reseeds the state; a follow-up append must fold
	// again without a decline.
	appended2 := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_1", "second result", "2024-01-01T10:00:06Z",
	))
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended2)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	loadsBefore = database.MessagesLoadCount()
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	assert.Equal(t, loadsBefore, database.MessagesLoadCount(),
		"post-resync append must fold incrementally")
	sess, err = database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, db.CurrentQualitySignalVersion, sess.QualitySignalVersion)
	_ = state
}

// TestIncrementalSignalMaintainerDeclinesForUserMessage verifies the
// fallback contract: a delta carrying a user message is not folded — the
// debounced full recompute runs (loading history) and keeps signals
// current.
func TestIncrementalSignalMaintainerDeclinesForUserMessage(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+signalMaintainUUID+".jsonl",
	)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			signalMaintainUUID, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	sessionID := "codex:" + signalMaintainUUID

	appended := testjsonl.JoinJSONL(testjsonl.CodexMsgJSON(
		"user", "please fix the failing test", "2024-01-01T10:00:02Z",
	))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	loadsBefore := database.MessagesLoadCount()
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	assert.Greater(t, database.MessagesLoadCount(), loadsBefore,
		"a user-message delta must fall back to the full recompute")
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, db.CurrentQualitySignalVersion, sess.QualitySignalVersion,
		"the fallback recompute must keep the signal version current")
	require.Equal(t, 2, sess.MessageCount)
}
