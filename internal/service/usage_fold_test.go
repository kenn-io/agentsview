package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestFoldProjectTotalsKeepsDistinctOpaqueProjectKeys(t *testing.T) {
	got := foldProjectTotals([]db.DailyUsageEntry{{
		ProjectBreakdowns: []db.ProjectBreakdown{
			{ProjectKey: "pl1:sha256:first", Project: "", Cost: money.MustParseDollars("1")},
			{ProjectKey: "pl1:sha256:second", Project: "", Cost: money.MustParseDollars("2")},
		},
	}})

	require.Len(t, got, 2)
	assert.Equal(t, "pl1:sha256:second", got[0].ProjectKey)
	assert.Equal(t, "pl1:sha256:first", got[1].ProjectKey)
}
