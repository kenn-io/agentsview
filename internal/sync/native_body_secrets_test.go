package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeSecretScansAndSources(t *testing.T) {
	fx := newEngineFixture(t)
	require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{
		ID: "native-secrets", Project: "project", Agent: "claude", Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, fx.db.InsertMessages(t.Context(), []db.Message{{
		SessionID: "native-secrets", Role: "assistant", Content: "safe", HasThinking: true, HasToolUse: true,
		ThinkingText: "plan AKIA7QHWN2DKR4FYPLJM 界", ToolResultText: "output AKIA7QHWN2DKR4FYPLJM 界", ContentLength: 91,
		ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", Rendering: "invoke AKIA7QHWN2DKR4FYPLJM 界"}},
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
			{Kind: "thinking", End: 15},
			{Kind: "text", End: 4},
			{Kind: "thinking", Start: 15, End: 29},
			{Kind: "tool_call", CallIndex: 0},
			{Kind: "tool_result", End: 31},
		}},
	}}))
	for _, mode := range []string{"inline", "full"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "inline" {
				require.NoError(t, fx.engine.RecomputeSignals(t.Context(), "native-secrets"))
			} else {
				_, err := fx.engine.ScanSecrets(t.Context(), SecretScanInput{Project: "project"}, nil)
				require.NoError(t, err)
			}
			findings, err := fx.db.SessionSecretFindings(t.Context(), "native-secrets")
			require.NoError(t, err)
			require.Len(t, findings, 3)
			want := map[string]struct {
				text  string
				start int
				end   int
			}{
				"thinking":       {"plan AKIA7QHWN2DKR4FYPLJM 界", 5, 25},
				"tool_output":    {"output AKIA7QHWN2DKR4FYPLJM 界", 7, 27},
				"tool_rendering": {"invoke AKIA7QHWN2DKR4FYPLJM 界", 7, 27},
			}
			for _, finding := range findings {
				expected, ok := want[finding.LocationKind]
				require.True(t, ok, "unexpected source %s", finding.LocationKind)
				delete(want, finding.LocationKind)
				assert.Equal(t, "aws-access-key", finding.RuleName)
				assert.Equal(t, expected.start, finding.MatchStart)
				assert.Equal(t, expected.end, finding.MatchEnd)
				assert.Equal(t, "AKIA…PLJM", finding.RedactedMatch)
				text, resolved, err := fx.db.SecretFindingSource(t.Context(), finding)
				require.NoError(t, err)
				assert.True(t, resolved)
				assert.Equal(t, expected.text, text)
				if finding.LocationKind == "tool_rendering" {
					require.NotNil(t, finding.CallIndex)
					assert.Zero(t, *finding.CallIndex)
				} else {
					assert.Nil(t, finding.CallIndex)
				}
			}
			assert.Empty(t, want)
		})
	}
}

func TestNativeSecretBackfillSupersedesOldCoverageStamp(t *testing.T) {
	fx := newEngineFixture(t)
	require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{
		ID: "old-secret-coverage", Project: "project", Agent: "claude", Machine: "local", MessageCount: 1,
	}))
	require.NoError(t, fx.db.InsertMessages(t.Context(), []db.Message{{
		SessionID: "old-secret-coverage", Role: "assistant", ThinkingText: "AKIA7QHWN2DKR4FYPLJM", HasThinking: true,
	}}))
	// Full v7 scans did not inspect thinking, standalone output or renderings.
	require.NoError(t, fx.db.ReplaceSessionSecretFindings(t.Context(), "old-secret-coverage", nil, 0,
		"bd4c273e0d48a52d630b8c3c270b5444891b447cd47d31ae979935e5c0810a93"))
	report, err := fx.engine.ScanSecrets(t.Context(), SecretScanInput{Backfill: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Scanned)
	findings, err := fx.db.SessionSecretFindings(t.Context(), "old-secret-coverage")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "thinking", findings[0].LocationKind)
}
