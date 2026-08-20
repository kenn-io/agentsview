// ABOUTME: Tests that a subagent's billed web searches reach the parent's
// ABOUTME: combined usage document with both the count and the fee.
package service_test

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

// A parent that delegated a web search to a subagent pays the search fee.
// Rates are $2/Mtok input and $10/Mtok output, so the parent's row costs
// $0.007 and the subagent's costs $0.014; the subagent's two searches add
// $0.02 on top.
func TestSessionUsageWithSubagentsBillsSubagentWebSearches(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-opus",
		InputPerMTok:  money.MustParseDollars("2.0"),
		OutputPerMTok: money.MustParseDollars("10.0"),
	}}), "UpsertModelPricing")

	const (
		parentID = "claude:ws-parent"
		childID  = "agent-ws-child"
	)
	parent := parentID
	seedSubagentSession(t, d, parentID, nil, "", 500)
	seedSubagentSession(t, d, childID, &parent, "subagent", 1000)

	parentMsg := usageMessage(parentID, 0, "10:00:00", "m-1", 1000, 500)
	childMsg := usageMessage(childID, 0, "10:01:00", "m-2", 2000, 1000)
	childMsg.TokenUsage = jsontext.Value(
		`{"input_tokens":2000,"output_tokens":1000,` +
			`"server_tool_use":{"web_search_requests":2}}`)
	dbtest.SeedMessages(t, d, parentMsg, childMsg)

	own, err := d.GetSessionUsage(ctx, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Equal(t, money.MustParseDollars("0.007"), own.Cost,
		"the parent's own rows performed no web search")

	got, err := service.SessionUsageWithSubagents(ctx, d, parentID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars("0.041"), got.Cost,
		"$0.007 parent + $0.014 subagent tokens + $0.02 for two searches")

	require.Len(t, got.Breakdown, 2)
	assert.Zero(t, got.Breakdown[0].WebSearchRequests)
	assert.Equal(t, 2, got.Breakdown[1].WebSearchRequests)
	assert.Equal(t, childID, got.Breakdown[1].SubagentSessionID)
	assert.Equal(t, money.MustParseDollars("0.034"),
		got.Breakdown[1].Cost)
}
