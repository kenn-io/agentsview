//go:build pgtest

package postgres

import (
	"testing"

	"go.kenn.io/agentsview/internal/ledger/ledgertest"
)

func TestQueryLedgerConformancePG(t *testing.T) {
	ledgertest.QueryConformance(t, func(t *testing.T) ledgertest.QueryStore {
		t.Helper()
		return newLedgerTestStore(t)
	})
}
