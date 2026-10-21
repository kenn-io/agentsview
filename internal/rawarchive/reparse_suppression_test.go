package rawarchive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestSuppressionForMatchesLegacyCodexPageRollout(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const rollout = "22222222-2222-4222-8222-222222222222"
	policy := map[string]db.RawArchiveSuppression{"codex:" + rollout: {Kind: "deleted"}}

	got, ok := rawderive.SuppressionFor(policy, "codex:"+thread+"_"+rollout)
	require.True(t, ok)
	assert.Equal(t, "deleted", got.Kind)
	_, ok = rawderive.SuppressionFor(policy, "codex:"+thread+"_33333333-3333-4333-8333-333333333333")
	assert.False(t, ok, "another page of the thread")
	_, ok = rawderive.SuppressionFor(policy, "codex:"+thread)
	assert.False(t, ok)
}
