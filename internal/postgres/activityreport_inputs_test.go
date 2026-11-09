package postgres

import (
	"fmt"
	"testing"
	"unique"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGActivityYearInputsFitBudget(t *testing.T) {
	// Match the comparison archive's year scale without a database or raw bodies.
	const sessions, messages, usageRows = 30_946, 986_645, 483_777
	inputs := pgActivityInputs{
		transcripts: make(map[string][]pgActivityMessage, sessions),
		usage:       make([]pgActivityUsageRow, 0, usageRows),
		candidates:  make(map[string]bool, sessions),
	}
	user, assistant, model, emptyModel := unique.Make("user"), unique.Make("assistant"), unique.Make("model-a"), unique.Make("")
	source, provider, agent := unique.Make("message"), unique.Make("provider-a"), unique.Make("claude")
	for i := range sessions {
		id := fmt.Sprintf("claude:project-a:session-%06d", i)
		count := messages / sessions
		if i < messages%sessions {
			count++
		}
		transcript := make([]pgActivityMessage, count)
		for j := range transcript {
			role, messageModel := user, emptyModel
			if j%2 == 1 {
				role, messageModel = assistant, model
			}
			transcript[j] = pgActivityMessage{ordinal: int32(j), us: int64(j) * 1_000_000, role: role, model: messageModel}
		}
		inputs.transcripts[id], inputs.candidates[id] = transcript, true
		count = usageRows / sessions
		if i < usageRows%sessions {
			count++
		}
		for j := range count {
			inputs.usage = append(inputs.usage, pgActivityUsageRow{sessionID: unique.Make(id), usageSource: source, model: model, providerID: provider, agent: agent, ordinal: int32(j*2 + 1), ordinalValid: true, us: int64(j) * 1_000_000, input: 100, output: 50})
		}
	}
	store := &Store{}
	memo := &store.runtimeState().activityInputs
	memo.Put("year", "revision", []pgActivityInputs{inputs})
	retained, ok := memo.Get("year", "revision")
	require.True(t, ok, "year inputs must be retained for the next visit")
	require.Len(t, retained, 1)
	assert.Len(t, retained[0].transcripts, sessions)
	assert.Len(t, retained[0].usage, usageRows)
}
