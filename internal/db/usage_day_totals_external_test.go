//go:build fts5

package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/testdata/usagefixture"
)

func TestUsageDayTotalsFiltersMatchLegacy(t *testing.T) {
	db.CheckUsageDayTotalsFiltersMatchLegacy(t, usagefixture.SeedPage(t, 32))
}

func TestUsageTopSessionsMatchLegacy(t *testing.T) {
	db.CheckUsageTopSessionsMatchLegacy(t, usagefixture.SeedPage(t, 32))
}
