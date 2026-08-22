package capture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestResultDistinguishesZeroFromUnavailable(t *testing.T) {
	zero := 0
	result := Result{
		Schema:       Schema{Name: ResultSchemaName, Version: ResultSchemaVersion},
		OccurrenceID: "build-17",
		Usage: &TokenUsage{
			InputTokens:  &zero,
			OutputTokens: &zero,
		},
	}

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"input_tokens":0`)
	assert.NotContains(t, string(encoded), "cache_creation_input_tokens")

	result.Usage = nil
	encoded, err = json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"usage"`)
}

func TestDecodeResultRejectsUnknownContract(t *testing.T) {
	for _, raw := range []string{
		`{"schema":{"name":"other","version":1}}`,
		`{"schema":{"name":"agentsview.one-shot-usage","version":2}}`,
		`{"schema":{"name":"agentsview.one-shot-usage","version":1},"reporting":{"outcome":"maybe"}}`,
		`{"schema":{"name":"agentsview.one-shot-usage","version":1},"assurance":{"state":"maybe"},"reporting":{"outcome":"complete"}}`,
		`{"schema":{"name":"agentsview.one-shot-usage","version":1},"assurance":{"reasons":["future_reason"]},"reporting":{"outcome":"complete"}}`,
	} {
		_, err := DecodeResult(strings.NewReader(raw))
		require.Error(t, err)
	}
}

func TestClaudeWorkDirEncodingMatchesObservedProducerLayout(t *testing.T) {
	assert.Equal(t,
		"-workspace-Space-dot-underscore--hash",
		encodeClaudeWorkDir("/workspace/Space.dot_underscore/@hash"),
	)
}

func TestResultMarksIncompleteTokenAndCostProvenance(t *testing.T) {
	termination := string(parser.TerminationClean)
	result, err := resultFromIngest(context.Background(), manifest{
		OccurrenceID: "partial-provenance", Provider: string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
	}, &ingestedCapture{
		Root: &db.Session{ID: "11111111-1111-4111-8111-111111111111",
			TerminationStatus: &termination},
		Usage: &db.SessionUsage{
			HasTokenData: true, TotalOutputTokens: 70, BreakdownCount: 1,
			Models: []string{"claude-test"},
			Breakdown: []db.SessionUsageBreakdownEntry{{
				InputTokens: 10, OutputTokens: 50,
				CacheCreationInputTokens: 20, CacheReadInputTokens: 30,
			}},
		},
	}, "test")
	require.NoError(t, err)

	require.NotNil(t, result.Usage)
	assertIntPointer(t, result.Usage.OutputTokens, 70)
	assert.Nil(t, result.Usage.InputTokens)
	assert.Nil(t, result.Usage.CacheCreationInputTokens)
	assert.Nil(t, result.Usage.CacheReadInputTokens)
	assert.Nil(t, result.Cost)
	assert.Equal(t, AssurancePartial, result.Assurance.State)
	assert.Contains(t, result.Assurance.Reasons, ReasonUsageUnavailable)
	assert.Contains(t, result.Assurance.Reasons, ReasonCostUnavailable)
}

func TestBoundedMetadataDoesNotSplitUTF8(t *testing.T) {
	assert.Equal(t, "ab", bounded("ab€", 4))
}
