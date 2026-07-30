package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestSyncPathsAndSingleSession_KimiWork mirrors the Kimi new-layout
// integration test for the Kimi Work provider: SyncPaths must classify a
// conv-* wire.jsonl, import it with the decoded workspace project and the
// kimi-work: ID prefix, skip auxiliary daimon sessions entirely, and
// SyncSingleSession must re-derive the same identity on resync.
func TestSyncPathsAndSingleSession_KimiWork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	kimiWorkDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentKimiWork: {kimiWorkDir},
		},
		Machine: "local",
	})

	workdirDir := "wd_agentsview_e901f41e2366"
	sessionDir := "conv-3fac68340656963a67a35ba9"
	wirePath := filepath.Join(
		kimiWorkDir, workdirDir, sessionDir, "agents", "main", "wire.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(wirePath), 0o755))
	require.NoError(t, os.WriteFile(wirePath, []byte(
		`{"type": "metadata", "protocol_version": "1.4", "created_at": 1704067200000}`+"\n"+
			`{"timestamp": 1704067200.0, "type": "turn.prompt", "input": [{"type": "text", "text": "Hello Kimi Work"}]}`+"\n"+
			`{"timestamp": 1704067202.0, "type": "context.append_loop_event", "event": {"type": "step.end", "finishReason": "stop"}}`+"\n",
	), 0o644))

	auxPath := filepath.Join(
		kimiWorkDir, workdirDir,
		"ctitle-019f85a8-bd77-7f02-ad95-ce249ffdc5c5",
		"agents", "main", "wire.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(auxPath), 0o755))
	require.NoError(t, os.WriteFile(auxPath, []byte(
		`{"type": "metadata", "protocol_version": "1.4"}`+"\n"+
			`{"timestamp": 1704067200.0, "type": "turn.prompt", "input": [{"type": "text", "text": "title me"}]}`+"\n",
	), 0o644))

	sessionID := "kimi-work:" + workdirDir + ":main:" + sessionDir

	engine.SyncPaths([]string{wirePath, auxPath})
	assertSessionProject(t, testDB, sessionID, "agentsview")

	// The aux session must not have been imported.
	auxSess, err := testDB.GetSession(t.Context(),
		"kimi-work:"+workdirDir+":main:ctitle-019f85a8-bd77-7f02-ad95-ce249ffdc5c5")
	require.NoError(t, err)
	assert.Nil(t, auxSess, "aux daimon sessions must not be imported")

	// Force a single-session resync; identity and project must hold.
	require.NoError(t, testDB.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET file_mtime = NULL WHERE id = ?",
			sessionID,
		)
		return err
	}))
	require.NoError(t, engine.SyncSingleSession(sessionID))
	assertSessionProject(t, testDB, sessionID, "agentsview")
}

func TestSyncKimiWorkMissingModelPricesAcrossK3Cutoff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	kimiWorkDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	require.NoError(t, testDB.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:     "moonshot/kimi-k2.6",
			OutputPerMTok:    money.MustParseDollars("4.00"),
			CacheReadPerMTok: money.MustParseDollars("0.16"),
		},
		{
			ModelPattern:     "kimi-k3",
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
	}))
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentKimiWork: {kimiWorkDir},
		},
		Machine: "local",
	})

	tests := []struct {
		name            string
		day             string
		timestamp       int64
		sessionDir      string
		wantCost        money.Money
		wantPricedModel string
	}{
		{
			name:            "before cutoff",
			day:             "2026-07-18",
			timestamp:       1784376000,
			sessionDir:      "conv-before-cutoff",
			wantCost:        money.MustParseDollars("4.00"),
			wantPricedModel: "moonshot/kimi-k2.6",
		},
		{
			name:            "after cutoff",
			day:             "2026-07-20",
			timestamp:       1784548800,
			sessionDir:      "conv-after-cutoff",
			wantCost:        money.MustParseDollars("15.00"),
			wantPricedModel: "kimi-k3",
		},
	}

	var paths []string
	for _, tt := range tests {
		wirePath := filepath.Join(
			kimiWorkDir, "wd_agentsview_e901f41e2366", tt.sessionDir,
			"agents", "main", "wire.jsonl",
		)
		require.NoError(t, os.MkdirAll(filepath.Dir(wirePath), 0o755))
		require.NoError(t, os.WriteFile(wirePath, fmt.Appendf(nil,
			`{"type":"metadata","protocol_version":"1.4","created_at":%d}`+"\n"+
				`{"timestamp":%d.0,"type":"turn.prompt","input":[{"type":"text","text":"price me"}]}`+"\n"+
				`{"timestamp":%d.0,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"Done."}}}`+"\n"+
				`{"timestamp":%d.0,"type":"context.append_loop_event","event":{"type":"step.end","finishReason":"stop","usage":{"output":1000000}}}`+"\n",
			tt.timestamp*1000, tt.timestamp, tt.timestamp+1, tt.timestamp+2,
		), 0o644))
		paths = append(paths, wirePath)
	}
	engine.SyncPaths(paths)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := testDB.GetDailyUsage(
				context.Background(),
				db.UsageFilter{
					From:     tt.day,
					To:       tt.day,
					Agent:    string(parser.AgentKimiWork),
					Timezone: "UTC",
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCost, result.Totals.TotalCost)
			require.NotNil(t, result.Pricing)
			require.Contains(t, result.Pricing.Models, "daimon-kimi-code")
			resolutions :=
				result.Pricing.Models["daimon-kimi-code"].Resolutions
			require.Len(t, resolutions, 1)
			assert.Equal(t, tt.wantPricedModel,
				resolutions[0].PricedModel)
		})
	}
}
