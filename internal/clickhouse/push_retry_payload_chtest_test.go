//go:build chtest

package clickhouse

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushRetainsPayloadAcrossRetry(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	messages, err := local.GetAllMessages(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.Greater(t, len(messages), 1)
	originalCount := len(messages)
	original := messages[0].Content
	changed := original + " changed during retry"
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
		s.hooks = nil
		messages[0].Content = changed
		require.NoError(t, local.ReplaceSessionMessages(ctx, fixtureAlphaID, messages[:1]))
		return errors.New("injected failure after dependent inserts")
	}}
	result, err := s.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	var mirrored string
	require.NoError(t, s.conn.QueryRowContext(ctx,
		"SELECT content FROM messages WHERE session_id = ? AND ordinal = ?",
		fixtureAlphaID, messages[0].Ordinal).Scan(&mirrored))
	require.Equal(t, original, mirrored, "a retry must retain the payload already assigned to its version")
	var mirroredCount int
	require.NoError(t, s.conn.QueryRowContext(ctx,
		"SELECT count() FROM messages WHERE session_id = ?", fixtureAlphaID).Scan(&mirroredCount))
	require.Equal(t, originalCount, mirroredCount)
	result, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	require.NoError(t, s.conn.QueryRowContext(ctx,
		"SELECT content FROM messages WHERE session_id = ? AND ordinal = ?",
		fixtureAlphaID, messages[0].Ordinal).Scan(&mirrored))
	require.Equal(t, changed, mirrored, "the next version must include the local correction")
	require.NoError(t, s.conn.QueryRowContext(ctx,
		"SELECT count() FROM messages WHERE session_id = ?", fixtureAlphaID).Scan(&mirroredCount))
	require.Equal(t, 1, mirroredCount, "the next version must remove the deleted ordinal")
}
