package capture

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestResultDistinguishesZeroFromUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	assert.Contains(string(encoded), `"input_tokens":0`)
	assert.NotContains(string(encoded), "cache_creation_input_tokens")

	result.Usage = nil
	encoded, err = json.Marshal(result)
	require.NoError(err)
	assert.NotContains(string(encoded), `"usage"`)
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
	assert := assert.New(t)
	require := require.New(t)

	termination := string(parser.TerminationClean)
	result, err := resultFromIngest(t.Context(), manifest{
		OccurrenceID: "partial-provenance", Provider: string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
	}, &ingestedCapture{
		Root: &db.Session{
			ID:                "11111111-1111-4111-8111-111111111111",
			TerminationStatus: &termination,
		},
		Usage: &db.SessionUsage{
			HasTokenData: true, TotalOutputTokens: 70, BreakdownCount: 1,
			Models: []string{"claude-test"},
			Breakdown: []db.SessionUsageBreakdownEntry{{
				InputTokens: 10, OutputTokens: 50,
				CacheCreationInputTokens: 20, CacheReadInputTokens: 30,
			}},
		},
	}, "test")
	require.NoError(err)

	require.NotNil(result.Usage)
	assertIntPointer(t, result.Usage.OutputTokens, 70)
	assert.Nil(result.Usage.InputTokens)
	assert.Nil(result.Usage.CacheCreationInputTokens)
	assert.Nil(result.Usage.CacheReadInputTokens)
	assert.Nil(result.Cost)
	assert.Equal(AssurancePartial, result.Assurance.State)
	assert.Contains(result.Assurance.Reasons, ReasonUsageUnavailable)
	assert.Contains(result.Assurance.Reasons, ReasonCostUnavailable)
}

func TestBoundedMetadataDoesNotSplitUTF8(t *testing.T) {
	assert.Equal(t, "ab", bounded("ab€", 4))
}
