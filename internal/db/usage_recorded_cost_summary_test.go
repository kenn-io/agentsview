package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestSummarizeRecordedCostCountsKnownFeesOnUnpricedRows(t *testing.T) {
	zero := money.Money{}
	recorded := money.Money{Microdollars: 7_000}
	got, err := SummarizeRecordedCost([]SessionUsageBreakdownEntry{
		// No recorded cost: not part of the comparison.
		{Cost: money.Money{Microdollars: 1_000}, HasCost: true},
		// Unpriced model with a recorded $0 and two billed web searches.
		{Cost: money.Money{Microdollars: 20_000}, HasCost: false, WebSearchRequests: 2, RecordedCost: &zero},
		// Priced by the recorded cost.
		{Cost: recorded, HasCost: true, RecordedCost: &recorded, RecordedCostPriced: true},
	})
	require.NoError(t, err)
	assert.Equal(t, &SessionRecordedCost{
		Recorded: recorded, Computed: money.Money{Microdollars: 27_000},
		Difference: money.Money{Microdollars: 20_000},
		Rows:       2, FallbackRows: 1, UnpricedRows: 1,
	}, got)

	got, err = SummarizeRecordedCost([]SessionUsageBreakdownEntry{{HasCost: true}})
	require.NoError(t, err)
	assert.Nil(t, got, "no row carries a recorded cost")
}
