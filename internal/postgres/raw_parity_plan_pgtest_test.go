//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawparity"
)

func TestRawParityLoadsPlanUsage(t *testing.T) {
	f := newHostedFixture(t, "tenant-plan-parity")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,agent,project,machine)
	 VALUES ('with-plan','codex-cloud','sample','device'), ('without-plan','codex-cloud','sample','device')`)
	require.NoError(t, err)
	p := fullPlanUsage()
	p.CountedUnder = "codex-cloud:root"
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, insertPlanUsage(t.Context(), tx, "with-plan", p))
	require.NoError(t, tx.Commit())
	snapshots, err := LoadRawParitySnapshots(t.Context(), parityTx(t, f.runtime), []string{"with-plan", "without-plan"})
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	assert.Nil(t, snapshots["without-plan"].PlanUsage)
	assert.Equal(t, p, snapshots["with-plan"].PlanUsage)
	comparison, err := rawparity.Compare(snapshots["with-plan"], snapshots["without-plan"])
	require.NoError(t, err)
	assert.Equal(t, []string{"usage"}, comparison.Differing)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE session_plan_usage SET credits='0.5',uncaptured_turns=4 WHERE session_id='with-plan'`)
	require.NoError(t, err)
	fresh, err := LoadRawParitySnapshots(t.Context(), parityTx(t, f.runtime), []string{"with-plan"})
	require.NoError(t, err)
	comparison, err = rawparity.Compare(snapshots["with-plan"], fresh["with-plan"])
	require.NoError(t, err)
	assert.Equal(t, []string{"usage"}, comparison.Differing)
}
