package rawderive

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
)

func parityTestBinding() ParityBinding {
	return ParityBinding{
		Request: ParityRequest{
			RunID:           "00000000-0000-4000-8000-000000000001",
			RuntimeID:       "00000000-0000-4000-8000-000000000002",
			BaselineProfile: "baseline-a",
			Cohort:          ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"},
		},
		BaselineID: "00000000-0000-4000-8000-000000000003",
		Tenant:     "tenant-a",
		Versions: ParityVersions{
			ParserBuild: ParityDigest{1}, Data: db.CurrentDataVersion(),
			Preparation: ParityPreparationVersion, Projection: ParityProjectionVersion,
			Comparison: ParitySchemaVersion, Quality: 7, SecretRules: "secret-v4",
			Policy: ParityDigest{2},
		},
		ObservedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
	}
}

func parityTestGraph() ParityGraph {
	zeroCost := money.Money{}
	pressure := 0.875
	score := 77
	grade := "C"
	first := "first prompt"
	title := "provider title"
	started := "2026-01-01T10:00:00.123456789Z"
	ended := "2026-01-01T11:00:00.987654321Z"
	parent := "physical-parent"
	parserParent := "provider-parent"
	termination := "complete"
	deletedAt := "2026-01-01T12:00:00Z"
	deletionCause := "excluded"
	sourceMissingAt := "2026-01-01T13:00:00Z"
	filePath := "/synthetic/session.jsonl"
	fileSize, fileMtime := int64(4096), int64(1767261600123456789)
	lastEntry := "entry-final"
	linearParse := false
	fileInode, fileDevice := int64(101), int64(202)
	fileHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	localModified := "2026-01-01T11:00:01Z"
	transcriptRevision := "revision-a"
	pending := "2026-01-01T11:00:00Z"
	call0, event1 := 0, 1
	signals := db.SessionSignalUpdate{
		ToolFailureSignalCount: 2, ToolRetryCount: 3, EditChurnCount: 4,
		ConsecutiveFailureMax: 5, Outcome: "completed", OutcomeConfidence: "high",
		EndedWithRole: "assistant", FinalFailureStreak: 6,
		SignalsPendingSince: &pending,
		CompactionCount:     7, MidTaskCompactionCount: 8,
		ContextPressureMax: &pressure, HealthScore: &score, HealthGrade: &grade,
		HasToolCalls: true, HasContextData: true, SecretLeakCount: 2,
		SecretsRulesVersion: "secret-v4",
		QualitySignals: db.QualitySignals{
			Version: 7, ShortPromptCount: 9, UnstructuredStart: true,
			MissingSuccessCriteriaCount: 10, MissingVerificationCount: 11,
			DuplicatePromptCount: 12, NoCodeContextCount: 13,
			RunawayToolLoopCount: 14,
		},
	}
	session := db.Session{
		ID: "physical-session", Project: "project-a", Machine: "machine-a",
		Agent: "codex", AgentLabel: "Codex", Entrypoint: "cli", SessionKind: "interactive",
		FirstMessage: &first, DisplayName: new("curated title"), SessionName: &title,
		StartedAt: &started, EndedAt: &ended, MessageCount: 2, UserMessageCount: 1,
		ParentSessionIDs: []string{"physical-parent"}, ParentSessionID: &parent,
		ParserParentSessionID: &parserParent, RelationshipType: "subagent",
		TotalOutputTokens: 21, PeakContextTokens: 34,
		HasTotalOutputTokens: true, HasPeakContextTokens: true, IsAutomated: false,
		DataVersion: db.CurrentDataVersion(), Cwd: "/synthetic/work", GitBranch: "feature-a",
		SourceSessionID: "portable-session", SourceVersion: "provider-v3",
		TranscriptFidelity: "full", ParserMalformedLines: 1, IsTruncated: true,
		TerminationStatus: &termination,
		DeletedAt:         &deletedAt, DeletionCause: &deletionCause,
		SourceMissingAt: &sourceMissingAt,
		FilePath:        &filePath, FileSize: &fileSize, FileMtime: &fileMtime,
		NextOrdinal: 2, LastEntryUUID: &lastEntry, ClaudeLinearParse: &linearParse,
		LastWriteIncremental: false, FileInode: &fileInode, FileDevice: &fileDevice,
		FileHash: &fileHash, LocalModifiedAt: &localModified,
		TranscriptRevision: &transcriptRevision, CreatedAt: "2026-01-01T10:00:00Z",
		PreserveSessionName: false, PreserveStoredAutomation: false,
	}
	ingest.ApplySignalFields(&session, signals)
	qualityCopy := signals.QualitySignals
	session.QualitySignals = &qualityCopy
	messages := []db.Message{
		{
			ID: 101, SessionID: "physical-session", Ordinal: 0, Role: "user",
			Content: "run the task", ThinkingText: "consider", Timestamp: "2026-01-01T10:00:00.123456789Z",
			HasThinking: true, HasToolUse: true, ContentLength: 12, IsSystem: true,
			Model: "model-a", ReasoningEffort: "high", ProviderID: "provider-a",
			TokenUsage:    jsontext.Value(`{"input_tokens":9007199254740993,"nested":{"b":2,"a":1}}`),
			ContextTokens: 34, OutputTokens: 0, HasContextTokens: true, HasOutputTokens: true,
			ClaudeMessageID: "message-a", ClaudeRequestID: "request-a",
			SourceType: "queue-operation", SourceSubtype: "user", PromptSource: "typed",
			SourceUUID: "source-message-a", SourceParentUUID: "source-parent-a",
			IsSidechain: true, IsCompactBoundary: false,
			ToolResults: []db.ToolResult{{ToolUseID: "paired-call", ContentLength: 6, ContentRaw: `"paired"`}},
			ToolCalls: []db.ToolCall{{
				MessageID: 101, SessionID: "physical-session", ToolName: "Read", Category: "Read",
				ToolUseID: "call-a", InputJSON: `{"path":"file.go","offset":9007199254740993}`,
				FilePath: "file.go", CallIndex: 0, SkillName: "", ResultContentLength: 11,
				ResultContent: "same result", SubagentSessionID: "portable-child", Rendering: "rendered-a",
				ResultEvents: []db.ToolResultEvent{
					{ToolUseID: "call-a", AgentID: "agent-a", SubagentSessionID: "portable-child", Source: "tool", Status: "completed", Content: "same result", ContentLength: 11, Timestamp: "2026-01-01T10:01:00.123456789Z", EventIndex: 0, RawContentDigest: []byte{1, 2}, SummaryParticipates: new(true)},
					{ToolUseID: "call-a", AgentID: "agent-a", SubagentSessionID: "portable-child", Source: "tool", Status: "completed", Content: "same result", ContentLength: 11, Timestamp: "2026-01-01T10:02:00.123456789Z", EventIndex: 1, RawContentDigest: []byte{3, 4}, SummaryParticipates: new(true)},
				},
			}},
		},
		{
			ID: 102, SessionID: "physical-session", Ordinal: 1, Role: "assistant",
			Content: "finished", Timestamp: "2026-01-01T11:00:00.987654321Z",
			ContentLength: 8, Model: "model-b", ProviderID: "provider-b",
			TokenUsage: jsontext.Value(`null`), ContextTokens: 0, OutputTokens: 21,
			HasContextTokens: false, HasOutputTokens: true,
			SourceType: "assistant", SourceSubtype: "final", SourceUUID: "source-message-b",
			ToolCalls: []db.ToolCall{{
				MessageID: 102, SessionID: "physical-session", ToolName: "Skill", Category: "Other",
				ToolUseID: "call-b", InputJSON: `{"skill":"verify"}`, FilePath: "report.md",
				CallIndex: 0, SkillName: "verify", ResultContentLength: 0,
				SubagentSessionID: "", Rendering: "rendered-b",
			}},
		},
	}
	usage := []db.UsageEvent{
		{ID: 201, SessionID: "physical-session", MessageOrdinal: new(1), Source: "message", Model: "model-b", ProviderID: "provider-b", InputTokens: 34, OutputTokens: 21, CacheCreationInputTokens: 3, CacheReadInputTokens: 4, ReasoningTokens: 5, Cost: &zeroCost, CostStatus: "exact", CostSource: "provider", OccurredAt: "2026-01-01T11:00:00.987654321Z", DedupKey: "linked"},
		{ID: 202, SessionID: "physical-session", MessageOrdinal: nil, Source: "session", Model: "model-a", ProviderID: "provider-a", InputTokens: 55, OutputTokens: 21, ReasoningTokens: 8, Cost: nil, CostStatus: "unknown", CostSource: "", OccurredAt: "2026-01-01T11:01:00.123456789Z", DedupKey: "unlinked"},
	}
	findings := []db.SecretFinding{
		{SessionID: "physical-session", RuleName: "synthetic-rule-a", Confidence: "high", LocationKind: "tool_input", MessageOrdinal: 0, CallIndex: &call0, MatchStart: 2, MatchEnd: 5, MatchIndex: 0, RedactedMatch: "[redacted-a]", RulesVersion: "secret-v4"},
		{SessionID: "physical-session", RuleName: "synthetic-rule-b", Confidence: "medium", LocationKind: "tool_result_event", MessageOrdinal: 0, CallIndex: &call0, EventIndex: &event1, MatchStart: 6, MatchEnd: 9, MatchIndex: 1, RedactedMatch: "[redacted-b]", RulesVersion: "secret-v4"},
	}
	signals.FullState = &db.SessionSignalState{
		SessionID: "physical-session", State: []byte("opaque-replay-state"),
		TranscriptRevision: "revision-a", SignalVersion: 7,
		UpdatedAt: "2026-01-01T11:00:01Z",
	}
	return ParityGraph{
		Key: ParityMemberKey{SourceID: "source-a", LogicalKey: "portable-session", Kind: "session"},
		Prepared: ingest.PreparedSession{
			Session: session, Messages: messages, UsageEvents: usage,
			Signals: signals, Findings: findings,
			Validation: db.ValidationStats{
				ControlCharsStripped: 1, ModelClamped: 2, TokensClamped: 3,
				RoleCoerced: 4, TimestampsBlanked: 5,
			},
		},
		PromptEvidenceDiscarded: true,
		Links: []ParityLink{
			{Kind: "parent", Ordinal: -1, CallIndex: -1, EventIndex: -1, Target: ParityMemberKey{SourceID: "source-a", LogicalKey: "portable-parent", Kind: "session"}},
			{Kind: "call", Ordinal: 0, CallIndex: 0, EventIndex: -1, Target: ParityMemberKey{SourceID: "source-a", LogicalKey: "portable-child", Kind: "session"}},
		},
		Overlay: ParityOverlay{Excluded: true, SourceDeleted: false, ProviderExcluded: true},
	}
}

func parityFingerprint(t *testing.T, graph ParityGraph) ParityFingerprint {
	t.Helper()
	fingerprint, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	require.NoError(t, err)
	return fingerprint
}

func parityVerdict(t *testing.T, baseline, candidate ParityGraph) ParityComparison {
	t.Helper()
	return CompareParity(parityFingerprint(t, baseline), parityFingerprint(t, candidate))
}

func TestParityFullyPopulatedGraphMatches(t *testing.T) {
	assert.Equal(t, ParityComparison{Verdict: ParityMatched}, parityVerdict(t, parityTestGraph(), parityTestGraph()))
}

func TestParityComparesPromptEvidenceDiscarded(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.PromptEvidenceDiscarded = false
	comparison := parityVerdict(t, baseline, candidate)
	assert.Equal(t, ParityMismatched, comparison.Verdict)
	assert.NotZero(t, comparison.Different&paritySessionBit)
}

func TestParityDetectsStoredSignalCorruption(t *testing.T) {
	baseline := parityTestGraph()
	candidate := parityTestGraph()
	baseline.Prepared.Session.ToolRetryCount++
	baseline.Prepared.Signals = ingest.SignalFields(baseline.Prepared.Session)
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityDetectsSemanticMutationsByCategory(t *testing.T) {
	tests := []struct {
		name   string
		bit    uint16
		mutate func(*ParityGraph)
	}{
		{"provider title", 1 << 0, func(g *ParityGraph) { g.Prepared.Session.SessionName = new("other") }},
		{"parser parent", 1 << 0, func(g *ParityGraph) { g.Prepared.Session.ParserParentSessionID = new("other") }},
		{"total tokens", 1 << 0, func(g *ParityGraph) { g.Prepared.Session.TotalOutputTokens++ }},
		{"total presence", 1 << 0, func(g *ParityGraph) { g.Prepared.Session.HasTotalOutputTokens = false }},
		{"peak presence", 1 << 0, func(g *ParityGraph) { g.Prepared.Session.HasPeakContextTokens = false }},
		{"message ordinal", 1 << 1, func(g *ParityGraph) {
			g.Prepared.Messages[1].Ordinal = 2
			*g.Prepared.UsageEvents[0].MessageOrdinal = 2
		}},
		{"message system", 1 << 1, func(g *ParityGraph) { g.Prepared.Messages[0].IsSystem = false }},
		{"message source identity", 1 << 1, func(g *ParityGraph) { g.Prepared.Messages[0].SourceUUID = "other" }},
		{"message context presence", 1 << 1, func(g *ParityGraph) { g.Prepared.Messages[0].HasContextTokens = false }},
		{"message output presence", 1 << 1, func(g *ParityGraph) { g.Prepared.Messages[0].HasOutputTokens = false }},
		{"tool input", 1 << 2, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].InputJSON = `{"path":"other"}` }},
		{"tool summary", 1 << 2, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].ResultContent = "other" }},
		{"tool file path", 1 << 2, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].FilePath = "other.go" }},
		{"event coordinate", 1 << 2, func(g *ParityGraph) {
			g.Prepared.Messages[0].ToolCalls[0].ResultEvents[1].EventIndex = 2
			*g.Prepared.Findings[1].EventIndex = 2
		}},
		{"event status", 1 << 2, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].Status = "errored" }},
		{"event bytes", 1 << 2, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].Content = "other" }},
		{"usage provider", 1 << 3, func(g *ParityGraph) { g.Prepared.UsageEvents[0].ProviderID = "other" }},
		{"usage dedup", 1 << 3, func(g *ParityGraph) { g.Prepared.UsageEvents[0].DedupKey = "other" }},
		{"usage reasoning", 1 << 3, func(g *ParityGraph) { g.Prepared.UsageEvents[0].ReasoningTokens++ }},
		{"usage nil cost", 1 << 3, func(g *ParityGraph) { g.Prepared.UsageEvents[1].Cost = new(money.Money{}) }},
		{"stable counter", 1 << 4, func(g *ParityGraph) { g.Prepared.Signals.ToolRetryCount++ }},
		{"quality scalar", 1 << 4, func(g *ParityGraph) { g.Prepared.Signals.QualitySignals.DuplicatePromptCount++ }},
		{"finding offset", 1 << 5, func(g *ParityGraph) { g.Prepared.Findings[0].MatchStart++ }},
		{"relationship target", 1 << 6, func(g *ParityGraph) { g.Links[0].Target.LogicalKey = "other" }},
		{"relationship kind", 1 << 6, func(g *ParityGraph) { g.Links[0].Kind = "parser-parent" }},
		{"exclusion", 1 << 7, func(g *ParityGraph) { g.Overlay.Excluded = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseline, candidate := parityTestGraph(), parityTestGraph()
			tc.mutate(&candidate)
			got := parityVerdict(t, baseline, candidate)
			assert.Equal(t, ParityMismatched, got.Verdict)
			assert.NotZero(t, got.Different&tc.bit)
		})
	}
}

func TestParityRetainsUsageMultiplicity(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.UsageEvents = append(candidate.Prepared.UsageEvents, candidate.Prepared.UsageEvents[0])
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityMapsPhysicalOwnershipIDsBeforeComparison(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.Session.ID = "other-physical-session"
	for i := range candidate.Prepared.Messages {
		candidate.Prepared.Messages[i].SessionID = candidate.Prepared.Session.ID
		for j := range candidate.Prepared.Messages[i].ToolCalls {
			candidate.Prepared.Messages[i].ToolCalls[j].SessionID = candidate.Prepared.Session.ID
		}
	}
	for i := range candidate.Prepared.UsageEvents {
		candidate.Prepared.UsageEvents[i].SessionID = candidate.Prepared.Session.ID
	}
	for i := range candidate.Prepared.Findings {
		candidate.Prepared.Findings[i].SessionID = candidate.Prepared.Session.ID
	}
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityRejectsMismatchedChildOwnership(t *testing.T) {
	graph := parityTestGraph()
	graph.Prepared.Messages[0].SessionID = "other-physical-session"
	_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	assert.Error(t, err)
}

func TestParityUsesTypedLinksInsteadOfPhysicalRelationshipIDs(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.Session.ParentSessionIDs = []string{"other-parent"}
	candidate.Prepared.Session.ParentSessionID = new("other-parent")
	candidate.Prepared.Messages[0].ToolCalls[0].SubagentSessionID = "other-child"
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].SubagentSessionID = "other-child"
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Links[1].Target.LogicalKey = "other-child"
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityEventOrdinalSwapMismatches(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].EventIndex = 1
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[1].EventIndex = 0
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityRejectsDuplicateCoordinates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ParityGraph)
	}{
		{"message", func(g *ParityGraph) { g.Prepared.Messages[1].Ordinal = 0 }},
		{"call", func(g *ParityGraph) {
			g.Prepared.Messages[0].ToolCalls = append(g.Prepared.Messages[0].ToolCalls, g.Prepared.Messages[0].ToolCalls[0])
		}},
		{"event", func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].ResultEvents[1].EventIndex = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := parityTestGraph()
			tc.mutate(&graph)
			_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
			assert.Error(t, err)
		})
	}
}

func TestParityCanonicalJSONExactness(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		matched     bool
	}{
		{"object key reorder", `{"a":1,"b":2}`, `{ "b": 2.0, "a": 1e0 }`, true},
		{"exact integer above float precision", `{"n":9007199254740993}`, `{"n":9.007199254740993e15}`, true},
		{"different integer above float precision", `{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"null and empty", `null`, ``, false},
		{"malformed exact bytes", `{"a":`, `{"a": `, false},
		{"duplicate keys stay raw", `{"a":1,"a":2}`, `{ "a":1,"a":2 }`, false},
		{"large exponent", `{"n":1e1000000000}`, `{"n":10e999999999}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseline, candidate := parityTestGraph(), parityTestGraph()
			baseline.Prepared.Messages[0].TokenUsage = jsontext.Value(tc.left)
			candidate.Prepared.Messages[0].TokenUsage = jsontext.Value(tc.right)
			comparison := parityVerdict(t, baseline, candidate)
			if tc.matched {
				assert.Equal(t, ParityMatched, comparison.Verdict)
			} else {
				assert.Equal(t, ParityMismatched, comparison.Verdict)
			}
		})
	}
}

func TestParityMalformedUnicodeEscapesRemainDistinct(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		matched     bool
	}{
		{"different unpaired high surrogates", `{"s":"\ud800"}`, `{"s":"\ud801"}`, false},
		{"unpaired surrogate and replacement character", `{"s":"\ud800"}`, `{"s":"�"}`, false},
		{"valid surrogate pair and scalar", `{"s":"\ud83d\ude00"}`, `{"s":"😀"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseline, candidate := parityTestGraph(), parityTestGraph()
			baseline.Prepared.Messages[0].TokenUsage = jsontext.Value(tc.left)
			candidate.Prepared.Messages[0].TokenUsage = jsontext.Value(tc.right)
			comparison := parityVerdict(t, baseline, candidate)
			if tc.matched {
				assert.Equal(t, ParityMatched, comparison.Verdict)
			} else {
				assert.Equal(t, ParityMismatched, comparison.Verdict)
			}
		})
	}
}

func TestParityCanonicalizesToolInputJSONWithoutLosingLargeIntegers(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	baseline.Prepared.Messages[0].ToolCalls[0].InputJSON = `{"path":"file.go","offset":9007199254740993}`
	candidate.Prepared.Messages[0].ToolCalls[0].InputJSON = `{ "offset": 9.007199254740993e15, "path": "file.go" }`
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Prepared.Messages[0].ToolCalls[0].InputJSON = `{ "offset": 9007199254740992, "path": "file.go" }`
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityNormalizesOnlyClockDerivedSignalValues(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	baseline.Prepared.Signals.Outcome = "unknown"
	baseline.Prepared.Signals.OutcomeConfidence = "low"
	baseline.Prepared.Signals.HealthScore = new(1)
	baseline.Prepared.Signals.HealthGrade = new("F")
	baseline.Prepared.Signals.SignalsPendingSince = new("first")
	candidate.Prepared.Signals.Outcome = "abandoned"
	candidate.Prepared.Signals.OutcomeConfidence = "medium"
	candidate.Prepared.Signals.HealthScore = new(99)
	candidate.Prepared.Signals.HealthGrade = new("A")
	candidate.Prepared.Signals.SignalsPendingSince = new("second")
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Prepared.Signals.EndedWithRole = "user"
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityCanonicalizesPostgresTimestampPrecision(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	baseline.Prepared.Messages[0].Timestamp = "2026-01-01T10:00:00.123456789+00:00"
	candidate.Prepared.Messages[0].Timestamp = "2026-01-01T05:00:00.123456001-05:00"
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Prepared.Messages[0].Timestamp = "2026-01-01T05:00:00.123457001-05:00"
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityRejectsInvalidSemanticTimestamp(t *testing.T) {
	graph := parityTestGraph()
	graph.Prepared.Messages[0].Timestamp = "not-a-time"
	_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	assert.Error(t, err)
}

func TestParityRejectsNonFinitePressure(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		graph := parityTestGraph()
		graph.Prepared.Signals.ContextPressureMax = &value
		_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
		assert.Error(t, err)
	}
}

func TestParityFingerprintDoesNotMutateInput(t *testing.T) {
	graph := parityTestGraph()
	before := parityTestGraph()
	_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	require.NoError(t, err)
	assert.Equal(t, before, graph)
}

func TestParityRestoresElidedSingleEventSummary(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	for _, graph := range []*ParityGraph{&baseline, &candidate} {
		call := &graph.Prepared.Messages[1].ToolCalls[0]
		call.ResultContentLength = 6
		call.ResultEvents = []db.ToolResultEvent{{Content: "result", ContentLength: 6}}
	}
	baseline.Prepared.Messages[1].ToolCalls[0].ResultContent = "result"
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestDigestParityBindingIsDeterministicAndComplete(t *testing.T) {
	binding := parityTestBinding()
	first, err := DigestParityBinding(binding)
	require.NoError(t, err)
	second, err := DigestParityBinding(binding)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	binding.Versions.Policy[0]++
	changed, err := DigestParityBinding(binding)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
}

func TestParityRequiresMatchingBindingVersions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ParityGraph)
	}{
		{"data", func(g *ParityGraph) { g.Prepared.Session.DataVersion-- }},
		{"quality", func(g *ParityGraph) { g.Prepared.Signals.QualitySignals.Version-- }},
		{"secrets", func(g *ParityGraph) { g.Prepared.Signals.SecretsRulesVersion = "other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := parityTestGraph()
			tc.mutate(&graph)
			_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
			assert.Error(t, err)
		})
	}
}

func TestParityNilAndEmptyRepeatedValuesMatch(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	baseline.Prepared.Messages = []db.Message{}
	candidate.Prepared.Messages = nil
	baseline.Prepared.UsageEvents = []db.UsageEvent{}
	candidate.Prepared.UsageEvents = nil
	baseline.Prepared.Findings = []db.SecretFinding{}
	candidate.Prepared.Findings = nil
	baseline.Links = []ParityLink{}
	candidate.Links = nil
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)

	baseline, candidate = parityTestGraph(), parityTestGraph()
	baseline.Prepared.Messages[1].ToolCalls[0].ResultEvents = []db.ToolResultEvent{}
	candidate.Prepared.Messages[1].ToolCalls[0].ResultEvents = nil
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := FingerprintParity(ctx, parityTestBinding(), parityTestGraph())
	assert.ErrorIs(t, err, context.Canceled)
}

type parityCancelAfterChecks struct {
	context.Context
	checks, cancelAt int
}

func (c *parityCancelAfterChecks) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestParityChecksCancellationBetweenCallsAndEvents(t *testing.T) {
	tests := []struct {
		name     string
		cancelAt int
	}{
		{"call", 2},
		{"event", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &parityCancelAfterChecks{Context: t.Context(), cancelAt: tc.cancelAt}
			_, _, _, err := encodeParityMessagesAndTools(ctx, parityTestGraph().Prepared.Messages[:1])
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestParityRejectsOversizedRetainedGraphBeforeCanonicalization(t *testing.T) {
	graph := parityTestGraph()
	graph.Prepared.Session.FileHash = new(strings.Repeat("x", maxParityGraphBytes+1))
	_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	assert.ErrorIs(t, err, errParityLimit)
}

func TestParityChargesIndependentlyAllocatedOwnershipIDs(t *testing.T) {
	const children = 20000
	parentID := strings.Repeat("session-", 225)

	base := func() ParityGraph {
		graph := parityTestGraph()
		graph.Prepared.Session.ID = parentID
		graph.Prepared.Messages = nil
		graph.Prepared.UsageEvents = nil
		graph.Prepared.Findings = nil
		graph.Prepared.Signals.FullState = nil
		graph.Links = nil
		return graph
	}
	tests := []struct {
		name  string
		graph func() ParityGraph
	}{
		{"messages", func() ParityGraph {
			graph := base()
			graph.Prepared.Messages = make([]db.Message, children)
			for i := range graph.Prepared.Messages {
				graph.Prepared.Messages[i] = db.Message{SessionID: strings.Clone(parentID), Ordinal: i}
			}
			return graph
		}},
		{"tool calls", func() ParityGraph {
			graph := base()
			graph.Prepared.Messages = []db.Message{{SessionID: parentID}}
			graph.Prepared.Messages[0].ToolCalls = make([]db.ToolCall, children)
			for i := range graph.Prepared.Messages[0].ToolCalls {
				graph.Prepared.Messages[0].ToolCalls[i] = db.ToolCall{SessionID: strings.Clone(parentID), CallIndex: i}
			}
			return graph
		}},
		{"usage", func() ParityGraph {
			graph := base()
			graph.Prepared.UsageEvents = make([]db.UsageEvent, children)
			for i := range graph.Prepared.UsageEvents {
				graph.Prepared.UsageEvents[i].SessionID = strings.Clone(parentID)
			}
			return graph
		}},
		{"findings", func() ParityGraph {
			graph := base()
			graph.Prepared.Messages = []db.Message{{SessionID: parentID}}
			graph.Prepared.Findings = make([]db.SecretFinding, children)
			for i := range graph.Prepared.Findings {
				graph.Prepared.Findings[i] = db.SecretFinding{
					SessionID: strings.Clone(parentID), MessageOrdinal: 0,
					RulesVersion: parityTestBinding().Versions.SecretRules,
				}
			}
			return graph
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FingerprintParity(t.Context(), parityTestBinding(), tc.graph())
			assert.ErrorIs(t, err, errParityLimit)
		})
	}

	t.Run("full signal state", func(t *testing.T) {
		graph := base()
		largeParentID := strings.Repeat("session-", 2_100_000)
		graph.Prepared.Session.ID = largeParentID
		graph.Prepared.Signals.FullState = &db.SessionSignalState{SessionID: strings.Clone(largeParentID)}
		_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
		assert.ErrorIs(t, err, errParityLimit)
	})
}

func TestParityRejectsTooManyChildRows(t *testing.T) {
	graph := parityTestGraph()
	graph.Links = make([]ParityLink, maxParityChildRows+1)
	_, err := FingerprintParity(t.Context(), parityTestBinding(), graph)
	assert.ErrorIs(t, err, errParityLimit)
}

func TestParityReportUsesExplicitSnakeCaseJSONFields(t *testing.T) {
	report := ParityReport{
		RunID: "run", State: "complete", Code: "invalid",
		Members: ParityCounts{Matched: 1, Mismatched: 2, Ambiguous: 3, LegacyOnly: 4, Missing: 5, PartialUnsupported: 6, Stale: 7},
		Sources: ParityCounts{}, PendingSources: 8, BaselineSealed: true,
		Complete: true, Freshness: "checked", Passing: false,
		RequestGeneration: 9, CompletedGeneration: 10,
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"run_id":"run","state":"complete","code":"invalid",
		"members":{"matched":1,"mismatched":2,"ambiguous":3,"legacy_only":4,"missing":5,"partial_unsupported":6,"stale":7},
		"sources":{"matched":0,"mismatched":0,"ambiguous":0,"legacy_only":0,"missing":0,"partial_unsupported":0,"stale":0},
		"pending_sources":8,"baseline_sealed":true,"complete":true,"freshness":"checked","passing":false,
		"request_generation":9,"completed_generation":10
	}`, string(encoded))
}

func TestParityExcludesRawEventDigestButDetectsResultContent(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest = []byte("other raw event")
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Prepared.Messages[0].ToolCalls[0].ResultContent = "other summary"
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestParityExcludesSummaryParticipationButDetectsEventIndex(t *testing.T) {
	baseline, candidate := parityTestGraph(), parityTestGraph()
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].SummaryParticipates = new(false)
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	candidate.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].EventIndex = 9
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func TestPrepareParityCandidatePreservesPriorGraphAndReplayMetadata(t *testing.T) {
	prior := parityTestGraph().Prepared
	before := parityTestGraph().Prepared
	parsed := parser.ParseResult{Session: parser.ParsedSession{
		ID: "physical-session", Agent: parser.AgentRooCode,
		SourceSessionID: "portable-session", CountsAuthoritative: true,
	}}

	got, err := PrepareParityCandidate(t.Context(), parsed, &prior, ingest.ContentOptions{}, parityTestBinding().ObservedAt)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	require.Len(t, got.Messages[0].ToolCalls, 1)
	require.Len(t, got.Messages[0].ToolCalls[0].ResultEvents, 2)
	assert.Equal(t, []byte{1, 2}, got.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest)
	assert.Equal(t, new(true), got.Messages[0].ToolCalls[0].ResultEvents[0].SummaryParticipates)
	assert.Len(t, got.UsageEvents, 2)
	assert.Len(t, got.Findings, 2)
	assert.Equal(t, db.CurrentDataVersion(), got.Session.DataVersion)

	got.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest[0] = 99
	*got.Messages[0].ToolCalls[0].ResultEvents[0].SummaryParticipates = false
	*got.UsageEvents[0].Cost = money.Money{Microdollars: 99}
	*got.Findings[0].CallIndex = 99
	assert.Equal(t, before, prior, "preparation must clone every retained mutable value")
}
