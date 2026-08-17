package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

func syntheticAWSAccessKey() string {
	return "AKIA" + "7QHWN2DKR4FYPLJM"
}

func TestScanSecretsFromMessages(t *testing.T) {
	sess := db.Session{ID: "s1"}
	msgs := []db.Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "my key " + syntheticAWSAccessKey() + " here"},
		{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "running",
			ToolCalls: []db.ToolCall{{
				ToolName: "Bash", ToolUseID: "tu1",
				InputJSON:     `{"command":"printenv"}`,
				ResultContent: "AWS_SECRET=sk-ant-api03-Xa9Kd03Lm5Qp7Rt2Vw8Zb4",
			}}},
	}
	findings, leak := scanSecretsFromMessages(sess, msgs, secrets.Scan)
	require.GreaterOrEqual(t, leak, 1, "expected >=1 definite finding, got leak=%d", leak)
	var sawMsg, sawTool bool
	for _, f := range findings {
		if f.LocationKind == "message" && f.MessageOrdinal == 0 {
			sawMsg = true
		}
		if f.LocationKind == "tool_result" && f.MessageOrdinal == 1 {
			sawTool = true
			assert.NotEmpty(t, f.RedactedMatch, "tool finding has empty RedactedMatch")
		}
	}
	assert.True(t, sawMsg && sawTool, "missing findings: msg=%v tool=%v (%+v)", sawMsg, sawTool, findings)
}

func TestScanSecretsDedupEventsVsResult(t *testing.T) {
	sess := db.Session{ID: "s1"}
	// Tool call WITH result events: result_content must be skipped.
	msgs := []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", ToolUseID: "tu1",
			ResultContent: syntheticAWSAccessKey(),
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "tu1", Status: "completed",
				Content: syntheticAWSAccessKey(), EventIndex: 0,
			}},
		}},
	}}
	findings, _ := scanSecretsFromMessages(sess, msgs, secrets.Scan)
	n := 0
	for _, f := range findings {
		if f.RuleName == "aws-access-key" {
			n++
		}
	}
	require.Equal(t, 1, n, "expected 1 aws finding (event canonical), got %d", n)
}

func TestScanSecretsResultEventIndexUsesCanonicalIndex(t *testing.T) {
	sess := db.Session{ID: "s1"}
	msgs := []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", ToolUseID: "tu1",
			ResultEvents: []db.ToolResultEvent{
				{Status: "running", Content: "starting up", EventIndex: 5},
				{Status: "completed", Content: syntheticAWSAccessKey(), EventIndex: 9},
			},
		}},
	}}
	findings, _ := scanSecretsFromMessages(sess, msgs, secrets.Scan)
	var got *db.SecretFinding
	for i := range findings {
		if findings[i].RuleName == "aws-access-key" {
			got = &findings[i]
			break
		}
	}
	require.NotNil(t, got, "no aws-access-key finding: %+v", findings)
	assert.Equal(t, "tool_result_event", got.LocationKind)
	require.NotNil(t, got.EventIndex)
	assert.Equal(t, 9, *got.EventIndex)
}

func TestScanSecretsResultEventIndexSurvivesPersistence(t *testing.T) {
	fx := newEngineFixture(t)
	const sessionID = "persisted-event-index"
	session := db.Session{
		ID: sessionID, Project: "proj", Machine: "machine", Agent: "claude",
		MessageCount: 1,
	}
	messages := []db.Message{{
		SessionID: sessionID, Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", ToolUseID: "tool-1",
			ResultEvents: []db.ToolResultEvent{
				{Status: "running", Content: "starting up", EventIndex: 5},
				{Status: "completed", Content: syntheticAWSAccessKey(), EventIndex: 9},
			},
		}},
	}}
	require.NoError(t, fx.db.UpsertSession(session))
	require.NoError(t, fx.db.ReplaceSessionMessages(sessionID, messages))
	findings, leak := scanSecretsFromMessages(session, messages, secrets.Scan)
	require.NoError(t, fx.db.ReplaceSessionSecretFindings(
		sessionID, findings, leak, "test-rules",
	))
	var finding *db.SecretFinding
	for index := range findings {
		if findings[index].RuleName == "aws-access-key" {
			finding = &findings[index]
			break
		}
	}
	require.NotNil(t, finding)

	source, ok, err := fx.db.SecretFindingSource(t.Context(), *finding)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, syntheticAWSAccessKey(), source)
}

func TestBackfillRewritesPreCanonicalEventCoordinateFindings(t *testing.T) {
	fx := newEngineFixture(t)
	ctx := context.Background()
	const (
		sessionID                = "pre-canonical-event-coordinate"
		preCanonicalRulesVersion = "bd4c273e0d48a52d630b8c3c270b5444891b447cd47d31ae979935e5c0810a93"
	)
	session := db.Session{
		ID: sessionID, Project: "proj", Machine: "machine", Agent: "claude",
		MessageCount: 1,
	}
	messages := []db.Message{{
		SessionID: sessionID, Ordinal: 0, Role: "assistant",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", ToolUseID: "tool-1",
			ResultEvents: []db.ToolResultEvent{
				{Status: "running", Content: "starting up", EventIndex: 5},
				{Status: "completed", Content: syntheticAWSAccessKey(), EventIndex: 9},
			},
		}},
	}}
	require.NoError(t, fx.db.UpsertSession(session))
	require.NoError(t, fx.db.ReplaceSessionMessages(sessionID, messages))
	callIndex, oldEventIndex := 0, 1
	require.NoError(t, fx.db.ReplaceSessionSecretFindings(
		sessionID,
		[]db.SecretFinding{{
			RuleName: "aws-access-key", Confidence: secrets.ConfidenceDefinite,
			LocationKind: "tool_result_event", MessageOrdinal: 0,
			CallIndex: &callIndex, EventIndex: &oldEventIndex,
			MatchStart: 0, MatchEnd: len(syntheticAWSAccessKey()),
			RedactedMatch: "AKIA…PLJM",
		}},
		1,
		preCanonicalRulesVersion,
	))

	summary, err := fx.engine.ScanSecrets(
		ctx, SecretScanInput{Backfill: true}, nil,
	)

	require.NoError(t, err)
	require.Equal(t, 1, summary.Scanned,
		"the pre-canonical rules stamp must be stale")
	findings, err := fx.db.SessionSecretFindings(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.NotNil(t, findings[0].EventIndex)
	assert.Equal(t, 9, *findings[0].EventIndex)
	assert.Equal(t, secrets.RulesVersion(), findings[0].RulesVersion)
	source, ok, err := fx.db.SecretFindingSource(ctx, findings[0])
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, syntheticAWSAccessKey(), source)
}

// TestComputeSignalsAndSecretsDefiniteOnly pins the inline-sync contract: the
// per-write scan path stores only definite findings and stamps the definite
// rules version, keeping the FP-prone, CPU-heavy candidate regexes out of the
// sync hot path.
func TestComputeSignalsAndSecretsDefiniteOnly(t *testing.T) {
	sess := db.Session{ID: "s1"}
	msgs := []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "user",
		Content: "aws " + syntheticAWSAccessKey() +
			" and SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6",
	}}
	update, findings := computeSignalsAndSecrets(sess, msgs)
	require.NotEmpty(t, findings, "expected at least one definite finding")
	for _, f := range findings {
		assert.Equal(t, secrets.ConfidenceDefinite, f.Confidence, "inline scan stored a non-definite finding: %+v", f)
	}
	assert.Equal(t, secrets.DefiniteRulesVersion(), update.SecretsRulesVersion)
	assert.Equal(t, 1, update.SecretLeakCount, "SecretLeakCount = %d, want 1 (one definite)", update.SecretLeakCount)
}

// TestInlineScanThenBackfillStoresCandidates verifies the full split-version
// lifecycle: an inline sync (RecomputeSignals) stores only definite findings at
// the definite version; because that version differs from the full ruleset
// version, secrets scan --backfill treats the session as stale, re-scans it,
// adds candidate findings, and stamps the full version (so a second backfill is
// a no-op).
func TestInlineScanThenBackfillStoresCandidates(t *testing.T) {
	fx := newEngineFixture(t)
	ctx := context.Background()
	const id = "s1"
	require.NoError(t, fx.db.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, fx.db.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user",
			Content: "aws " + syntheticAWSAccessKey() +
				" and SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"},
	}))

	// Inline sync path: definite-only findings, definite version.
	require.NoError(t, fx.engine.RecomputeSignals(ctx, id))
	got, err := fx.db.SessionSecretFindings(ctx, id)
	require.NoError(t, err)
	assert.Zero(t, countConfidence(got, secrets.ConfidenceCandidate), "inline scan stored candidate findings: %+v", got)
	assert.NotZero(t, countConfidence(got, secrets.ConfidenceDefinite), "inline scan stored no definite findings")

	// Backfill must treat the inline-only session as stale and rescan it.
	sum, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Scanned, "backfill Scanned = %d, want 1 (inline-only session is stale)", sum.Scanned)
	got, err = fx.db.SessionSecretFindings(ctx, id)
	require.NoError(t, err)
	assert.NotZero(t, countConfidence(got, secrets.ConfidenceCandidate), "backfill did not store candidate findings: %+v", got)

	// Now current at the full version: a second backfill scans nothing.
	sum2, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true}, nil)
	require.NoError(t, err)
	assert.Zero(t, sum2.Scanned, "second backfill Scanned = %d, want 0 (now at full version)", sum2.Scanned)
}

// TestScanSecretsBreakdown verifies that ScanSecrets reports definite
// and candidate findings separately while preserving the existing
// WithSecrets semantic (sessions with ≥1 definite finding).
func TestScanSecretsBreakdown(t *testing.T) {
	fx := newEngineFixture(t)
	ctx := context.Background()
	const id = "s1"
	if err := fx.db.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	// One message containing both a definite AWS key and a candidate
	// high-entropy assignment.
	if err := fx.db.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user",
			Content: "aws " + syntheticAWSAccessKey() +
				" and SECRET=Xa9Kd03Lm5Qp7Rt2Vw8Zb4Nc6"},
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	sum, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true}, nil)
	if err != nil {
		t.Fatalf("ScanSecrets: %v", err)
	}
	if sum.Scanned != 1 {
		t.Fatalf("Scanned = %d, want 1", sum.Scanned)
	}
	if sum.DefiniteFindings != 1 {
		t.Errorf("DefiniteFindings = %d, want 1", sum.DefiniteFindings)
	}
	if sum.CandidateFindings != 1 {
		t.Errorf("CandidateFindings = %d, want 1", sum.CandidateFindings)
	}
	if sum.TotalFindings != 2 {
		t.Errorf("TotalFindings = %d, want 2", sum.TotalFindings)
	}
	if sum.WithSecrets != 1 {
		t.Errorf("WithSecrets = %d, want 1 (session has ≥1 definite finding)", sum.WithSecrets)
	}
}

func countConfidence(findings []db.SecretFinding, confidence string) int {
	n := 0
	for _, f := range findings {
		if f.Confidence == confidence {
			n++
		}
	}
	return n
}

func TestEngineScanSecretsBackfillResumable(t *testing.T) {
	fx := newEngineFixture(t)
	ctx := context.Background()
	// Seed two sessions with secret-bearing content directly, bypassing the
	// sync scan path, so secrets_rules_version stays "" (unscanned).
	for _, id := range []string{"s1", "s2"} {
		require.NoError(t, fx.db.UpsertSession(db.Session{
			ID: id, Project: "proj", Machine: "m", Agent: "claude",
			MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, fx.db.ReplaceSessionMessages(id, []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user",
				Content: "my key " + syntheticAWSAccessKey() + " here"},
		}))
	}
	ticks := 0
	sum, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true},
		func(SecretScanProgress) { ticks++ })
	require.NoError(t, err)
	require.Equal(t, 2, sum.Scanned, "scan summary = %+v, want Scanned=2", sum)
	require.Equal(t, 2, sum.WithSecrets, "scan summary = %+v, want WithSecrets=2", sum)
	assert.NotZero(t, ticks, "expected at least one progress tick")
	for _, id := range []string{"s1", "s2"} {
		s, err := fx.db.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, s)
		assert.GreaterOrEqual(t, s.SecretLeakCount, 1, "%s SecretLeakCount = %d, want >=1", id, s.SecretLeakCount)
	}
	// Re-running the backfill scans nothing: all sessions are now current.
	sum2, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true}, nil)
	require.NoError(t, err)
	assert.Zero(t, sum2.Scanned, "resumed Scanned = %d, want 0 (already current)", sum2.Scanned)
}

// TestScanSecretsCanceledContextReturnsError pins the cancellation contract: a
// scan run with a canceled context must return that error rather than report a
// partial scan as success, and must persist nothing.
func TestScanSecretsCanceledContextReturnsError(t *testing.T) {
	fx := newEngineFixture(t)
	require.NoError(t, fx.db.UpsertSession(db.Session{
		ID: "s1", Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, fx.db.ReplaceSessionMessages("s1", []db.Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "my key " + syntheticAWSAccessKey() + " here"},
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fx.engine.ScanSecrets(ctx, SecretScanInput{Backfill: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	s, err := fx.db.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Zero(t, s.SecretLeakCount, "SecretLeakCount = %d, want 0 (canceled scan persisted nothing)", s.SecretLeakCount)
}
