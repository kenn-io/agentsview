//go:build chtest

package clickhouse

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// A --full push that fails before it records its full push reports that it
// never started, so a watch keeps the --full request. One that fails after it
// recorded the run resumes that run instead.
func TestFullPushReportsWhetherItRecordedItsRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, target := seedFixture(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := newTestSync(t, local, target, storage.PusherOptions{}).
		PushWithOptions(canceled, storage.PushOptions{Full: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, notStarted := errors.AsType[storage.PushNotStartedError](err)
	assert.True(t, notStarted, "the push failed before it recorded a run")

	s := newTestSync(t, local, target, storage.PusherOptions{})
	killCtx, kill := context.WithCancel(ctx)
	defer kill()
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
		kill()
		return killCtx.Err()
	}}
	_, err = s.PushWithOptions(killCtx, storage.PushOptions{Full: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, notStarted = errors.AsType[storage.PushNotStartedError](err)
	assert.False(t, notStarted, "the push recorded a run the next push resumes")
}
