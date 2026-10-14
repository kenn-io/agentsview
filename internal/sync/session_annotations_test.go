package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSessionAnnotationWritesEmitOnlyOnChange(t *testing.T) {
	fx := newEngineFixture(t)
	emitter := &fakeEmitter{}
	fx.engineWithEmitter(t.Context(), emitter)
	ctx := t.Context()
	for _, id := range []string{"manager", "worker"} {
		require.NoError(t, fx.db.UpsertSession(ctx, db.Session{
			ID: id, Project: "proj", Machine: "local",
			Agent: string(parser.AgentClaude), MessageCount: 1,
		}))
	}
	expectEmits := func(t *testing.T, want int, step string) {
		t.Helper()
		got := emitter.got()
		assert.Len(t, got, want, step)
		emitter.mu.Lock()
		emitter.scopes = nil
		emitter.mu.Unlock()
	}

	_, err := fx.engine.UpdateSessionLabels(ctx, "worker", []string{"ticket=A"}, nil)
	require.NoError(t, err)
	expectEmits(t, 1, "adding a label")
	_, err = fx.engine.UpdateSessionLabels(ctx, "worker", []string{"ticket=A"}, nil)
	require.NoError(t, err)
	expectEmits(t, 0, "re-adding the same label")
	_, err = fx.engine.SetSessionLabels(ctx, "worker", []string{"ticket=A"})
	require.NoError(t, err)
	expectEmits(t, 0, "replacing labels with the same set")
	_, err = fx.engine.UpdateSessionLabels(ctx, "worker", nil, []string{"missing"})
	require.NoError(t, err)
	expectEmits(t, 0, "removing an absent label")
	_, err = fx.engine.SetSessionLabels(ctx, "worker", nil)
	require.NoError(t, err)
	expectEmits(t, 1, "clearing the last label")
	_, err = fx.engine.SetSessionLabels(ctx, "not-synced", []string{"ticket=A"})
	require.NoError(t, err)
	expectEmits(t, 0, "labeling a session that has not synced")

	_, err = fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	expectEmits(t, 1, "applying a parent link")
	_, err = fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	expectEmits(t, 0, "re-recording the same parent link")
	_, err = fx.engine.SetSessionExternalParent(ctx, "pending", "manager")
	require.NoError(t, err)
	expectEmits(t, 0, "linking a session that has not synced")
	_, err = fx.engine.ClearSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	expectEmits(t, 1, "clearing an applied parent link")
}
