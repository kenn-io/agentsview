//go:build fts5

package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	dbtest.AssertRecordedCostUsage(t, local)
}

func TestSessionUsageTotalStaysApartFromUsage(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsageTotal(t, local)
	dbtest.AssertSessionUsageTotal(t, local)
}

func TestSessionUsageTotalChangesOnlyOnReplacement(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsageTotal(t, local)
	session, err := local.GetSessionFull(ctx, dbtest.SessionTotalSessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	messages, err := local.GetAllMessages(ctx, dbtest.SessionTotalSessionID)
	require.NoError(t, err)
	rewrite := func(write db.SessionBatchWrite) {
		t.Helper()
		write.Session, write.Messages, write.ReplaceMessages = *session, messages, true
		_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{write})
		require.NoError(t, err)
	}
	total := func() *db.SessionUsageTotal {
		t.Helper()
		got, err := local.GetSessionUsageTotal(ctx, dbtest.SessionTotalSessionID)
		require.NoError(t, err)
		return got
	}

	// A rewrite that is not a full parse, such as a repair or an artifact
	// import, keeps the stored total.
	rewrite(db.SessionBatchWrite{})
	assert.Equal(t, dbtest.SessionUsageTotalFixture(), total())

	newer := dbtest.SessionUsageTotalFixture()
	newer.Cost = &money.Money{Microdollars: 900_000}
	rewrite(db.SessionBatchWrite{SessionTotal: newer, ReplaceSessionTotal: true})
	assert.Equal(t, newer, total())

	rewrite(db.SessionBatchWrite{ReplaceSessionTotal: true})
	assert.Nil(t, total(), "a full parse without a total removes it")
}
