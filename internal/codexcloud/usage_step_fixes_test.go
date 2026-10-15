package codexcloud

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// loseAccount removes account.json, or replaces it with bytes that do not
// decode.
func (e *usageEnv) loseAccount(t *testing.T, corrupt bool) {
	t.Helper()
	path, err := e.syncer.Store.path(passKey, "account", ".json")
	require.NoError(t, err)
	if corrupt {
		require.NoError(t, os.WriteFile(path, []byte("{garbage"), 0o600))
		return
	}
	require.NoError(t, os.Remove(path))
}

// requestedSince lists every root asked about in plan usage requests
// since from, sorted.
func (e *usageEnv) requestedSince(from int) []string {
	var roots []string
	for _, r := range e.requestedRoots(from) {
		roots = append(roots, r...)
	}
	slices.Sort(roots)
	return roots
}

func TestLostAccountStateRebuildsPlanUsageQueue(t *testing.T) {
	env := newUsageEnv(t)
	for _, id := range []string{"partial", "available", "leftout"} {
		env.threads.Set(finishedThread(id, ago(time.Hour), message(id+"-m", "hi")))
	}
	env.usage.SetRow("partial", codexcloudtest.Row("partial", "partial", pct(1), nil))
	env.usage.SetRow("available", codexcloudtest.Row("available", "available", pct(2), nil))
	env.run(t)
	acct := env.account(t)
	require.Contains(t, acct.UsagePending, "partial")
	require.Contains(t, acct.UsagePending, "leftout")
	require.Contains(t, acct.Refresh, "available")

	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from, fetchFrom := len(env.usage.Requests()), len(env.host.Requests())
	env.run(t)

	assert.Equal(t, []string{"available", "leftout", "partial"}, env.requestedSince(from),
		"every unsettled root is asked about again")
	assert.Empty(t, env.fetchedThreads(fetchFrom), "no chat is fetched again")
	acct = env.account(t)
	assert.Equal(t, fetchNow, acct.UsagePending["partial"].LastChange, "the stored change time, not now")
	assert.Equal(t, fetchNow, acct.UsagePending["leftout"].LastChange)
	assert.Equal(t, fetchNow, acct.Refresh["available"].LastChange)

	// Lost again after the settle point: each root settles on its first
	// answer.
	env.loseAccount(t, true)
	env.clock.Set(fetchNow.Add(settleAfter + time.Hour))
	from = len(env.usage.Requests())
	env.run(t)
	assert.Equal(t, []string{"available", "leftout", "partial"}, env.requestedSince(from))
	partial := env.plan(t, "partial")
	assert.True(t, partial.rows[len(partial.rows)-1].Final, "the partial row settles")
	leftOut := env.plan(t, "leftout")
	assert.Equal(t, mirror.OutcomeSettledNoRow, leftOut.outcomes[len(leftOut.outcomes)-1].Kind)
	acct = env.account(t)
	assert.NotContains(t, acct.UsagePending, "partial")
	assert.NotContains(t, acct.UsagePending, "leftout")

	// Settled roots stay settled when the account state is lost once more.
	env.loseAccount(t, false)
	env.clock.Advance(time.Hour)
	from = len(env.usage.Requests())
	env.run(t)
	requested := env.requestedSince(from)
	assert.NotContains(t, requested, "partial")
	assert.NotContains(t, requested, "leftout")
}
