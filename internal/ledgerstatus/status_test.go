package ledgerstatus

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestCollectAndWriteText(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	w := ledger.NewWriter(d, "default", "host-a", nil)
	_, err := w.Append(ctx, []ledger.Event{{
		EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}})
	require.NoError(t, err)
	require.NoError(t, d.SetSyncState(ctx, postgres.LedgerPushStatusKeyPrefix+"hub",
		`{"at":"2026-09-22T10:00:00.000000Z","zones":{"default":{"pushed":1}},"failures":[]}`))

	reports, err := Collect(ctx, config.LedgerConfig{Enabled: true}, d, []string{"default", "ops"})
	require.NoError(t, err)
	require.Len(t, reports, 2)
	assert.Equal(t, 1, reports[0].Events)
	assert.Equal(t, map[string]uint64{"host-a": 1}, reports[0].Sources)
	require.Len(t, reports[0].Push, 1)
	assert.Equal(t, 1, reports[0].Push[0].Pushed)

	var b bytes.Buffer
	require.NoError(t, WriteText(&b, true, reports))
	assert.Equal(t, "zone default: 1 segment(s), 1 event(s)\n"+
		"  source host-a: latest seq 1\n"+
		"  push [hub] at 2026-09-22T10:00:00.000000Z: 1 pushed, 0 already present, 0 held back, 0 refused\n"+
		"zone ops: 0 segment(s), 0 event(s)\n"+
		"  push [hub] at 2026-09-22T10:00:00.000000Z: 0 pushed, 0 already present, 0 held back, 0 refused\n",
		b.String())

	raw, err := json.Marshal(reports[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"zone":"default"`, "ZoneStatus is inlined")
	assert.Contains(t, string(raw), `"push":[`)
}

func TestFormatID(t *testing.T) {
	assert.Equal(t, "host-a-000004", FormatID("host-a", "4"))
	assert.Equal(t, "host-a-x", FormatID("host-a", "x"))
}
