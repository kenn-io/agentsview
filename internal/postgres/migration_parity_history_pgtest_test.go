//go:build pgtest

package postgres

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestParityHistoryWalksCompleteReceiptChainInAscendingOrder(t *testing.T) {
	f := newProjectionFixture(t)
	first, firstCommit := f.acceptScoped(t, "device-a", "capture-1", "", parser.AgentCodex, "root-a", "source-a")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, first), first, projectionOutcome("one")))
	second, secondCommit := f.acceptScoped(t, "device-a", "capture-2", firstCommit.Receipt, parser.AgentCodex, "root-a", "source-a")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, projectionOutcome("two")))
	third, _ := f.acceptScoped(t, "device-a", "capture-3", secondCommit.Receipt, parser.AgentCodex, "root-a", "source-a")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, third), third, projectionOutcome("three")))
	insertMalformedManifest := func(manifestID, receipt, parent, keySHA string, generation int64) {
		_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256)
			VALUES($1,'device-a','codex','root-a',$2,$2) ON CONFLICT DO NOTHING`, f.tenant, keySHA)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,canonical_json)
			VALUES($1,$2,'device-a','codex','root-a',$3,$3,$4,$5,$6,$7,'snapshot',clock_timestamp(),'{}')`, f.tenant, manifestID, keySHA, manifestID[:8], parent, receipt, generation)
		require.NoError(t, err)
	}
	cycleHead, cycleParent := strings.Repeat("a", 64), strings.Repeat("b", 64)
	cycleKey := strings.Repeat("c", 64)
	insertMalformedManifest(strings.Repeat("d", 64), cycleHead, cycleParent, cycleKey, 2)
	insertMalformedManifest(strings.Repeat("e", 64), cycleParent, cycleHead, cycleKey, 1)
	wrongHead, wrongParent, wrongKey := strings.Repeat("f", 64), strings.Repeat("1", 64), strings.Repeat("2", 64)
	insertMalformedManifest(strings.Repeat("3", 64), wrongHead, wrongParent, wrongKey, 2)

	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	binding := rawderive.ParityBinding{Tenant: f.tenant, Request: rawderive.ParityRequest{Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: parser.AgentCodex, RootID: "root-a"}}}
	sources, err := readParityCohortSources(t.Context(), tx, binding, "", 1)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	history, code, err := readParityReceiptChain(t.Context(), tx, sources[0])
	require.NoError(t, err)
	assert.Empty(t, code)
	require.Len(t, history, 3)
	assert.Equal(t, []int64{1, 2, 3}, []int64{history[0].Generation, history[1].Generation, history[2].Generation})
	assert.Equal(t, []string{first.ManifestID, second.ManifestID, third.ManifestID}, []string{history[0].ManifestID, history[1].ManifestID, history[2].ManifestID})

	for name, mutate := range map[string]func(*parityCapturedSource){
		"skipped generation": func(source *parityCapturedSource) { source.source.HeadGeneration++ },
		"wrong head":         func(source *parityCapturedSource) { source.source.HeadManifestID = first.ManifestID },
		"different provider": func(source *parityCapturedSource) { source.provider = string(parser.AgentClaude) },
		"different device":   func(source *parityCapturedSource) { source.source.Identity.DeviceID = "device-b" },
		"different root":     func(source *parityCapturedSource) { source.rootID = "root-b" },
		"different source":   func(source *parityCapturedSource) { source.keySHA256 = "wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			source := sources[0]
			mutate(&source)
			_, gotCode, readErr := readParityReceiptChain(t.Context(), tx, source)
			require.NoError(t, readErr)
			assert.Equal(t, "missing_history", gotCode)
		})
	}
	for name, source := range map[string]parityCapturedSource{
		"cycle":                {source: rawderive.ParitySource{ID: "cycle", Identity: sources[0].source.Identity, HeadManifestID: strings.Repeat("d", 64), HeadGeneration: 2}, provider: "codex", rootID: "root-a", keySHA256: cycleKey, headReceipt: cycleHead},
		"wrong parent receipt": {source: rawderive.ParitySource{ID: "wrong-parent", Identity: sources[0].source.Identity, HeadManifestID: strings.Repeat("3", 64), HeadGeneration: 2}, provider: "codex", rootID: "root-a", keySHA256: wrongKey, headReceipt: wrongHead},
	} {
		t.Run(name, func(t *testing.T) {
			_, gotCode, readErr := readParityReceiptChain(t.Context(), tx, source)
			require.NoError(t, readErr)
			assert.Equal(t, "missing_history", gotCode)
		})
	}
	require.NoError(t, tx.Commit())

	var branchID string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT branch_id FROM raw_session_branches WHERE source_id=$1`, sources[0].source.ID).Scan(&branchID))
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_source_contributions(branch_id,manifest_id,projection_generation,processing_version,prior_contributed,payload)
		VALUES($1,$2,999,'parser-1',FALSE,'{}')`, branchID, strings.Repeat("3", 64))
	require.NoError(t, err)
	tx, err = f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	_, code, err = readParityReceiptChain(t.Context(), tx, sources[0])
	require.NoError(t, err)
	assert.Equal(t, "missing_history", code, "a contribution manifest outside the receipt chain makes history partial")
	require.NoError(t, tx.Commit())
}
