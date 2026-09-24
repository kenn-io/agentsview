package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/service"
)

func TestDirectBackendLedgerQuery(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	_, err := ledger.NewWriter(d, "default", "host-a", nil).Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
		Timestamp: time.Now().UTC(),
	}})
	require.NoError(t, err)
	svc := service.NewDirectBackend(d, nil)
	require.True(t, service.SupportsLedgerQueries(svc))
	results, err := service.LedgerQuery(t.Context(), svc, ledger.Query{Since: time.Now().Add(-time.Hour), Limit: 10})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "default", results[0].Zone)
}
