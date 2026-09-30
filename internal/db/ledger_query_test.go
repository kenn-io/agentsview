package db

import (
	"testing"

	"go.kenn.io/agentsview/internal/ledger/ledgertest"
)

func TestQueryLedgerConformance(t *testing.T) {
	ledgertest.QueryConformance(t, func(t *testing.T) ledgertest.QueryStore {
		t.Helper()
		return testDB(t)
	})
}
