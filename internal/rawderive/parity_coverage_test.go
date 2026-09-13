package rawderive

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type fieldDecision struct {
	Type, Field, GoType, Class string
}

var parityFieldDecisions = []fieldDecision{
	{Type: "Session", Field: "ID", GoType: "string", Class: "B"},
	{Type: "Session", Field: "Project", GoType: "string", Class: "S"},
	{Type: "Session", Field: "Machine", GoType: "string", Class: "B"},
	{Type: "Session", Field: "Agent", GoType: "string", Class: "S"},
	{Type: "Session", Field: "AgentLabel", GoType: "string", Class: "S"},
	{Type: "Session", Field: "Entrypoint", GoType: "string", Class: "S"},
	{Type: "Session", Field: "SessionKind", GoType: "string", Class: "S"},
	{Type: "Session", Field: "FirstMessage", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "DisplayName", GoType: "*string", Class: "B"},
	{Type: "Session", Field: "SessionName", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "StartedAt", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "EndedAt", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "MessageCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "UserMessageCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "ParentSessionIDs", GoType: "[]string", Class: "S"},
	{Type: "Session", Field: "ParentSessionID", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "ParserParentSessionID", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "RelationshipType", GoType: "string", Class: "S"},
	{Type: "Session", Field: "TotalOutputTokens", GoType: "int", Class: "S"},
	{Type: "Session", Field: "PeakContextTokens", GoType: "int", Class: "S"},
	{Type: "Session", Field: "HasTotalOutputTokens", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "HasPeakContextTokens", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "IsAutomated", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "ToolFailureSignalCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "ToolRetryCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "EditChurnCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "ConsecutiveFailureMax", GoType: "int", Class: "S"},
	{Type: "Session", Field: "Outcome", GoType: "string", Class: "S"},
	{Type: "Session", Field: "OutcomeConfidence", GoType: "string", Class: "S"},
	{Type: "Session", Field: "EndedWithRole", GoType: "string", Class: "S"},
	{Type: "Session", Field: "FinalFailureStreak", GoType: "int", Class: "S"},
	{Type: "Session", Field: "SignalsPendingSince", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "CompactionCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "MidTaskCompactionCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "ContextPressureMax", GoType: "*float64", Class: "S"},
	{Type: "Session", Field: "HealthScore", GoType: "*int", Class: "S"},
	{Type: "Session", Field: "HealthGrade", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "QualitySignals", GoType: "*QualitySignals", Class: "S"},
	{Type: "Session", Field: "HasToolCalls", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "HasContextData", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "SecretLeakCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "SecretsRulesVersion", GoType: "string", Class: "B"},
	{Type: "Session", Field: "QualitySignalVersion", GoType: "int", Class: "B"},
	{Type: "Session", Field: "ShortPromptCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "UnstructuredStart", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "MissingSuccessCriteriaCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "MissingVerificationCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "DuplicatePromptCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "NoCodeContextCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "RunawayToolLoopCount", GoType: "int", Class: "S"},
	{Type: "Session", Field: "DataVersion", GoType: "int", Class: "B"},
	{Type: "Session", Field: "Cwd", GoType: "string", Class: "S"},
	{Type: "Session", Field: "GitBranch", GoType: "string", Class: "S"},
	{Type: "Session", Field: "SourceSessionID", GoType: "string", Class: "S"},
	{Type: "Session", Field: "SourceVersion", GoType: "string", Class: "S"},
	{Type: "Session", Field: "TranscriptFidelity", GoType: "string", Class: "S"},
	{Type: "Session", Field: "ParserMalformedLines", GoType: "int", Class: "S"},
	{Type: "Session", Field: "IsTruncated", GoType: "bool", Class: "S"},
	{Type: "Session", Field: "DeletedAt", GoType: "*string", Class: "B"},
	{Type: "Session", Field: "DeletionCause", GoType: "*string", Class: "B"},
	{Type: "Session", Field: "SourceMissingAt", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "TerminationStatus", GoType: "*string", Class: "S"},
	{Type: "Session", Field: "FilePath", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "FileSize", GoType: "*int64", Class: "X"},
	{Type: "Session", Field: "FileMtime", GoType: "*int64", Class: "X"},
	{Type: "Session", Field: "NextOrdinal", GoType: "int", Class: "X"},
	{Type: "Session", Field: "LastEntryUUID", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "ClaudeLinearParse", GoType: "*bool", Class: "X"},
	{Type: "Session", Field: "LastWriteIncremental", GoType: "bool", Class: "X"},
	{Type: "Session", Field: "FileInode", GoType: "*int64", Class: "X"},
	{Type: "Session", Field: "FileDevice", GoType: "*int64", Class: "X"},
	{Type: "Session", Field: "FileHash", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "LocalModifiedAt", GoType: "*string", Class: "X"},
	{Type: "Session", Field: "TranscriptRevision", GoType: "*string", Class: "B"},
	{Type: "Session", Field: "CreatedAt", GoType: "string", Class: "X"},
	{Type: "Session", Field: "PreserveSessionName", GoType: "bool", Class: "X"},
	{Type: "Session", Field: "PreserveStoredAutomation", GoType: "bool", Class: "X"},
	{Type: "QualitySignals", Field: "Version", GoType: "int", Class: "B"},
	{Type: "QualitySignals", Field: "ShortPromptCount", GoType: "int", Class: "S"},
	{Type: "QualitySignals", Field: "UnstructuredStart", GoType: "bool", Class: "S"},
	{Type: "QualitySignals", Field: "MissingSuccessCriteriaCount", GoType: "int", Class: "S"},
	{Type: "QualitySignals", Field: "MissingVerificationCount", GoType: "int", Class: "S"},
	{Type: "QualitySignals", Field: "DuplicatePromptCount", GoType: "int", Class: "S"},
	{Type: "QualitySignals", Field: "NoCodeContextCount", GoType: "int", Class: "S"},
	{Type: "QualitySignals", Field: "RunawayToolLoopCount", GoType: "int", Class: "S"},
	{Type: "Message", Field: "ID", GoType: "int64", Class: "X"},
	{Type: "Message", Field: "SessionID", GoType: "string", Class: "B"},
	{Type: "Message", Field: "Ordinal", GoType: "int", Class: "S"},
	{Type: "Message", Field: "Role", GoType: "string", Class: "S"},
	{Type: "Message", Field: "Content", GoType: "string", Class: "S"},
	{Type: "Message", Field: "ThinkingText", GoType: "string", Class: "S"},
	{Type: "Message", Field: "Timestamp", GoType: "string", Class: "S"},
	{Type: "Message", Field: "HasThinking", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "HasToolUse", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "ContentLength", GoType: "int", Class: "S"},
	{Type: "Message", Field: "Model", GoType: "string", Class: "S"},
	{Type: "Message", Field: "ReasoningEffort", GoType: "string", Class: "S"},
	{Type: "Message", Field: "ProviderID", GoType: "string", Class: "S"},
	{Type: "Message", Field: "TokenUsage", GoType: "jsontext.Value", Class: "S"},
	{Type: "Message", Field: "ContextTokens", GoType: "int", Class: "S"},
	{Type: "Message", Field: "OutputTokens", GoType: "int", Class: "S"},
	{Type: "Message", Field: "HasContextTokens", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "HasOutputTokens", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "ClaudeMessageID", GoType: "string", Class: "S"},
	{Type: "Message", Field: "ClaudeRequestID", GoType: "string", Class: "S"},
	{Type: "Message", Field: "ToolCalls", GoType: "[]ToolCall", Class: "S"},
	{Type: "Message", Field: "ToolResults", GoType: "[]ToolResult", Class: "X"},
	{Type: "Message", Field: "IsSystem", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "SourceType", GoType: "string", Class: "S"},
	{Type: "Message", Field: "SourceSubtype", GoType: "string", Class: "S"},
	{Type: "Message", Field: "PromptSource", GoType: "string", Class: "S"},
	{Type: "Message", Field: "SourceUUID", GoType: "string", Class: "S"},
	{Type: "Message", Field: "SourceParentUUID", GoType: "string", Class: "S"},
	{Type: "Message", Field: "IsSidechain", GoType: "bool", Class: "S"},
	{Type: "Message", Field: "IsCompactBoundary", GoType: "bool", Class: "S"},
	{Type: "ToolCall", Field: "MessageID", GoType: "int64", Class: "X"},
	{Type: "ToolCall", Field: "SessionID", GoType: "string", Class: "B"},
	{Type: "ToolCall", Field: "ToolName", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "Category", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "ToolUseID", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "InputJSON", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "FilePath", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "CallIndex", GoType: "int", Class: "S"},
	{Type: "ToolCall", Field: "SkillName", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "ResultContentLength", GoType: "int", Class: "S"},
	{Type: "ToolCall", Field: "ResultContent", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "SubagentSessionID", GoType: "string", Class: "S"},
	{Type: "ToolCall", Field: "ResultEvents", GoType: "[]ToolResultEvent", Class: "S"},
	{Type: "ToolCall", Field: "Rendering", GoType: "string", Class: "X"},
	{Type: "ToolResultEvent", Field: "ToolUseID", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "AgentID", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "SubagentSessionID", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "Source", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "Status", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "Content", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "ContentLength", GoType: "int", Class: "S"},
	{Type: "ToolResultEvent", Field: "Timestamp", GoType: "string", Class: "S"},
	{Type: "ToolResultEvent", Field: "EventIndex", GoType: "int", Class: "S"},
	{Type: "ToolResultEvent", Field: "RawContentDigest", GoType: "[]byte", Class: "X"},
	{Type: "ToolResultEvent", Field: "SummaryParticipates", GoType: "*bool", Class: "X"},
	{Type: "UsageEvent", Field: "ID", GoType: "int64", Class: "X"},
	{Type: "UsageEvent", Field: "SessionID", GoType: "string", Class: "B"},
	{Type: "UsageEvent", Field: "MessageOrdinal", GoType: "*int", Class: "S"},
	{Type: "UsageEvent", Field: "Source", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "Model", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "ProviderID", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "InputTokens", GoType: "int", Class: "S"},
	{Type: "UsageEvent", Field: "OutputTokens", GoType: "int", Class: "S"},
	{Type: "UsageEvent", Field: "CacheCreationInputTokens", GoType: "int", Class: "S"},
	{Type: "UsageEvent", Field: "CacheReadInputTokens", GoType: "int", Class: "S"},
	{Type: "UsageEvent", Field: "ReasoningTokens", GoType: "int", Class: "S"},
	{Type: "UsageEvent", Field: "Cost", GoType: "*money.Money", Class: "S"},
	{Type: "UsageEvent", Field: "CostStatus", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "CostSource", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "OccurredAt", GoType: "string", Class: "S"},
	{Type: "UsageEvent", Field: "DedupKey", GoType: "string", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "FullState", GoType: "*SessionSignalState", Class: "X"},
	{Type: "SessionSignalUpdate", Field: "ToolFailureSignalCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "ToolRetryCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "EditChurnCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "ConsecutiveFailureMax", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "Outcome", GoType: "string", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "OutcomeConfidence", GoType: "string", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "EndedWithRole", GoType: "string", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "FinalFailureStreak", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "SignalsPendingSince", GoType: "*string", Class: "X"},
	{Type: "SessionSignalUpdate", Field: "CompactionCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "MidTaskCompactionCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "ContextPressureMax", GoType: "*float64", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "HealthScore", GoType: "*int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "HealthGrade", GoType: "*string", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "HasToolCalls", GoType: "bool", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "HasContextData", GoType: "bool", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "SecretLeakCount", GoType: "int", Class: "S"},
	{Type: "SessionSignalUpdate", Field: "SecretsRulesVersion", GoType: "string", Class: "B"},
	{Type: "SessionSignalUpdate", Field: "QualitySignals", GoType: "QualitySignals", Class: "S"},
	{Type: "SecretFinding", Field: "SessionID", GoType: "string", Class: "B"},
	{Type: "SecretFinding", Field: "RuleName", GoType: "string", Class: "S"},
	{Type: "SecretFinding", Field: "Confidence", GoType: "string", Class: "S"},
	{Type: "SecretFinding", Field: "LocationKind", GoType: "string", Class: "S"},
	{Type: "SecretFinding", Field: "MessageOrdinal", GoType: "int", Class: "S"},
	{Type: "SecretFinding", Field: "CallIndex", GoType: "*int", Class: "S"},
	{Type: "SecretFinding", Field: "EventIndex", GoType: "*int", Class: "S"},
	{Type: "SecretFinding", Field: "MatchStart", GoType: "int", Class: "S"},
	{Type: "SecretFinding", Field: "MatchEnd", GoType: "int", Class: "S"},
	{Type: "SecretFinding", Field: "MatchIndex", GoType: "int", Class: "S"},
	{Type: "SecretFinding", Field: "RedactedMatch", GoType: "string", Class: "S"},
	{Type: "SecretFinding", Field: "RulesVersion", GoType: "string", Class: "B"},
}

var parityPreparedFieldDecisions = []fieldDecision{
	{Type: "PreparedSession", Field: "Session", GoType: "db.Session", Class: "R"},
	{Type: "PreparedSession", Field: "Messages", GoType: "[]db.Message", Class: "R"},
	{Type: "PreparedSession", Field: "UsageEvents", GoType: "[]db.UsageEvent", Class: "R"},
	{Type: "PreparedSession", Field: "Signals", GoType: "db.SessionSignalUpdate", Class: "R"},
	{Type: "PreparedSession", Field: "Findings", GoType: "[]db.SecretFinding", Class: "R"},
	{Type: "PreparedSession", Field: "Validation", GoType: "db.ValidationStats", Class: "X"},
}

func TestParityFieldDecisionRegistryIsComplete(t *testing.T) {
	types := map[string]reflect.Type{
		"Session":             reflect.TypeFor[db.Session](),
		"QualitySignals":      reflect.TypeFor[db.QualitySignals](),
		"Message":             reflect.TypeFor[db.Message](),
		"ToolCall":            reflect.TypeFor[db.ToolCall](),
		"ToolResultEvent":     reflect.TypeFor[db.ToolResultEvent](),
		"UsageEvent":          reflect.TypeFor[db.UsageEvent](),
		"SessionSignalUpdate": reflect.TypeFor[db.SessionSignalUpdate](),
		"SecretFinding":       reflect.TypeFor[db.SecretFinding](),
	}
	require.Len(t, parityFieldDecisions, 188)
	seen := make(map[string]fieldDecision, len(parityFieldDecisions))
	for _, decision := range parityFieldDecisions {
		require.Contains(t, types, decision.Type)
		require.Contains(t, []string{"S", "B", "X"}, decision.Class)
		key := decision.Type + "." + decision.Field
		assert.NotContains(t, seen, key, "duplicate decision")
		seen[key] = decision
		field, ok := types[decision.Type].FieldByName(decision.Field)
		if assert.True(t, ok, key) {
			gotType := strings.ReplaceAll(field.Type.String(), "db.", "")
			if gotType == "[]uint8" {
				gotType = "[]byte"
			}
			assert.Equal(t, decision.GoType, gotType, key)
		}
	}
	var actualCount int
	for typeName, typ := range types {
		actualCount += typ.NumField()
		for field := range typ.Fields() {
			_, ok := seen[typeName+"."+field.Name]
			assert.True(t, ok, "unclassified field %s.%s", typeName, field.Name)
		}
	}
	assert.Equal(t, 188, actualCount)
}

func TestParityPreparedSessionFieldCoverageIsExplicit(t *testing.T) {
	prepared := reflect.TypeOf(parityTestGraph().Prepared)
	require.Equal(t, len(parityPreparedFieldDecisions), prepared.NumField())
	seen := make(map[string]struct{}, len(parityPreparedFieldDecisions))
	for _, decision := range parityPreparedFieldDecisions {
		assert.Equal(t, "PreparedSession", decision.Type)
		assert.Contains(t, []string{"R", "X"}, decision.Class)
		_, duplicate := seen[decision.Field]
		assert.False(t, duplicate, "duplicate PreparedSession decision")
		seen[decision.Field] = struct{}{}
		field, ok := prepared.FieldByName(decision.Field)
		if assert.True(t, ok, decision.Field) {
			assert.Equal(t, decision.GoType, field.Type.String(), decision.Field)
		}
	}
	for field := range prepared.Fields() {
		_, ok := seen[field.Name]
		assert.True(t, ok, "unclassified PreparedSession.%s", field.Name)
	}
}

func assertParityExcludedField(
	t *testing.T,
	excludedMutation func(*ParityGraph),
	semanticMutation func(*ParityGraph),
) {
	t.Helper()
	baseline, candidate := parityTestGraph(), parityTestGraph()
	excludedMutation(&candidate)
	assert.Equal(t, ParityMatched, parityVerdict(t, baseline, candidate).Verdict)
	semanticMutation(&candidate)
	assert.Equal(t, ParityMismatched, parityVerdict(t, baseline, candidate).Verdict)
}

func mutateSessionProject(g *ParityGraph) { g.Prepared.Session.Project = "other" }
func mutateMessageRole(g *ParityGraph)    { g.Prepared.Messages[0].Role = "assistant" }
func mutateToolName(g *ParityGraph)       { g.Prepared.Messages[0].ToolCalls[0].ToolName = "Write" }
func mutateEventStatus(g *ParityGraph) {
	g.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].Status = "errored"
}
func mutateUsageSource(g *ParityGraph)   { g.Prepared.UsageEvents[0].Source = "other" }
func mutateSignalCounter(g *ParityGraph) { g.Prepared.Signals.ToolRetryCount++ }

func TestParityExcludesSessionSignalsPendingSince(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.SignalsPendingSince = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionSourceMissingAt(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.SourceMissingAt = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionFilePath(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FilePath = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionFileSize(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FileSize = new(int64(99)) }, mutateSessionProject)
}
func TestParityExcludesSessionFileMtime(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FileMtime = new(int64(99)) }, mutateSessionProject)
}
func TestParityExcludesSessionNextOrdinal(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.NextOrdinal = 99 }, mutateSessionProject)
}
func TestParityExcludesSessionLastEntryUUID(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.LastEntryUUID = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionClaudeLinearParse(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.ClaudeLinearParse = new(true) }, mutateSessionProject)
}
func TestParityExcludesSessionLastWriteIncremental(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.LastWriteIncremental = true }, mutateSessionProject)
}
func TestParityExcludesSessionFileInode(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FileInode = new(int64(99)) }, mutateSessionProject)
}
func TestParityExcludesSessionFileDevice(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FileDevice = new(int64(99)) }, mutateSessionProject)
}
func TestParityExcludesSessionFileHash(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.FileHash = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionLocalModifiedAt(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.LocalModifiedAt = new("other") }, mutateSessionProject)
}
func TestParityExcludesSessionCreatedAt(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.CreatedAt = "other" }, mutateSessionProject)
}
func TestParityExcludesSessionPreserveSessionName(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.PreserveSessionName = true }, mutateSessionProject)
}
func TestParityExcludesSessionPreserveStoredAutomation(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Session.PreserveStoredAutomation = true }, mutateSessionProject)
}
func TestParityExcludesMessageID(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Messages[0].ID = 999 }, mutateMessageRole)
}
func TestParityExcludesMessageToolResults(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) {
		g.Prepared.Messages[0].ToolResults = []db.ToolResult{{ToolUseID: "other", ContentRaw: `"other"`}}
	}, mutateMessageRole)
}
func TestParityExcludesToolCallMessageID(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].MessageID = 999 }, mutateToolName)
}
func TestParityExcludesToolCallRendering(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Messages[0].ToolCalls[0].Rendering = "other" }, mutateToolName)
}
func TestParityExcludesToolResultEventRawContentDigest(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) {
		g.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest = []byte("other")
	}, mutateEventStatus)
}
func TestParityExcludesToolResultEventSummaryParticipates(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) {
		g.Prepared.Messages[0].ToolCalls[0].ResultEvents[0].SummaryParticipates = new(false)
	}, mutateEventStatus)
}
func TestParityExcludesUsageEventID(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.UsageEvents[0].ID = 999 }, mutateUsageSource)
}
func TestParityExcludesSignalFullState(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) {
		g.Prepared.Signals.FullState = &db.SessionSignalState{State: []byte("other")}
	}, mutateSignalCounter)
}
func TestParityExcludesSignalSignalsPendingSince(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Signals.SignalsPendingSince = new("other") }, mutateSignalCounter)
}
func TestParityExcludesPreparedValidation(t *testing.T) {
	assertParityExcludedField(t, func(g *ParityGraph) { g.Prepared.Validation.ControlCharsStripped = 99 }, mutateSessionProject)
}
