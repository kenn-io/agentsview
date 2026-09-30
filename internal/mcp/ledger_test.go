package mcp

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/service"
)

func TestQueryLedgerTool(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	_, err := ledger.NewWriter(d, "default", "host-a", nil).Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassDecision, PayloadTier: ledger.TierStructured, Timestamp: now.Add(-time.Hour),
		Payload: map[string]any{
			"subsystem": "deploy", "summary": "rolled out",
			"large": uint64(18446744073709551615),
		},
	}})
	require.NoError(t, err)
	ts := &toolset{svc: service.NewDirectBackend(d, nil), now: func() time.Time { return now }}

	_, out, err := ts.queryLedger(t.Context(), nil, queryLedgerIn{Subsystems: []string{"dep*"}})
	require.NoError(t, err)
	assert.Equal(t, "7d", out.Since)
	require.Len(t, out.Events, 1)
	assert.Equal(t, "default", out.Events[0].Zone)
	assert.Equal(t, "decision", out.Events[0].Event["event_class"])
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"large":18446744073709551615`)

	_, _, err = ts.queryLedger(t.Context(), nil, queryLedgerIn{Class: "review"})
	require.ErrorContains(t, err, "unknown event class 'review'")
}
