//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/storage"
)

// A rebuilt archive gets a new database ID. Rows the rebuild wrote before the
// stored cutoff fall outside the next incremental window, so the next push
// must be full.
func TestPushAfterArchiveRebuildIsFull(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.False(t, res.Full)

	require.NoError(t, f.local.SetDatabaseIDForTest(t.Context(), "rebuilt"))
	res, err = f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, "archive was rebuilt", res.FullReason)
}

// A rebuild during a push mixes rows from two archives. The push must fail
// before it saves its cutoff, and the next push must be full.
func TestPushFailsWhenTheArchiveIsRebuiltDuringIt(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	before, err := f.local.GetSyncState(t.Context(), "last_push_at")
	require.NoError(t, err)

	s := f.sync()
	s.afterPushBatch = func() {
		require.NoError(t, f.local.SetDatabaseIDForTest(t.Context(), "rebuilt"))
	}
	_, err = s.PushWithOptions(t.Context(), storage.PushOptions{Full: true}, nil)
	require.ErrorContains(t, err, "rebuilt during the push")
	_, notStarted := errors.AsType[storage.PushNotStartedError](err)
	assert.False(t, notStarted, "the failed push recorded a full push to resume")
	after, err := f.local.GetSyncState(t.Context(), "last_push_at")
	require.NoError(t, err)
	assert.Equal(t, before, after, "the failed push must not save its cutoff")

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
}

// A --full push that fails before it records its full push reports that it
// never started, so a watch keeps the --full request.
func TestFullPushFailingBeforeItsRunIsNotStarted(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, full := range []bool{false, true} {
		_, err := f.sync().PushWithOptions(ctx, storage.PushOptions{Full: full}, nil)
		require.ErrorIs(t, err, context.Canceled)
		_, notStarted := errors.AsType[storage.PushNotStartedError](err)
		assert.Equal(t, full, notStarted, "full=%t", full)
	}
}
