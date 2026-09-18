package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

// persistProbeContext reports cancellation the moment probe() first returns
// true, without ever closing a Done channel: context-aware DB reads proceed
// normally while the engine's explicit ctx.Err() checks observe the
// cancellation. It pins timing windows — cancellation landing inside one
// unit of work — that a real cancel cannot hit deterministically.
type persistProbeContext struct {
	probe func() bool
}

func (persistProbeContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (persistProbeContext) Done() <-chan struct{}       { return nil }
func (persistProbeContext) Value(any) any               { return nil }
func (c persistProbeContext) Err() error {
	if c.probe() {
		return context.Canceled
	}
	return nil
}

// TestScanSecretsCountsSessionPersistedBeforeCancellation pins the summary
// contract on cancellation: a session whose scan result was committed before
// the cancellation was observed must be counted in the returned summary.
// Callers use Scanned to decide whether session eligibility may have
// changed — an undercount would suppress the extraction-scheduler
// notification for work that did commit.
func TestScanSecretsCountsSessionPersistedBeforeCancellation(t *testing.T) {
	require := require.New(t)

	fx := newEngineFixture(t)
	require.NoError(fx.db.UpsertSession(db.Session{
		ID: "s1", Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(fx.db.ReplaceSessionMessages("s1", []db.Message{
		{
			SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "no secrets here, just prose",
		},
	}))

	ver := secrets.RulesVersion()
	persisted := func() bool {
		s, err := fx.db.GetSession(t.Context(), "s1")
		require.NoError(err)
		require.NotNil(s)
		return s.SecretsRulesVersion == ver
	}
	// The context reads as canceled from the moment s1's scan result
	// commits, so the first check to observe it is the one right after
	// the persist.
	sum, err := fx.engine.ScanSecrets(
		persistProbeContext{probe: persisted}, SecretScanInput{Backfill: true}, nil)
	require.ErrorIs(err, context.Canceled)
	require.True(persisted(), "scan must have persisted s1 before canceling")
	assert.Equal(t, 1, sum.Scanned,
		"a session persisted before the cancellation was observed must be counted")
}
