//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCommitLinksClickHousePublishesWithoutSessionCandidates(t *testing.T) {
	local, target := seedFixture(t)
	ctx := t.Context()
	_, err := local.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	for _, subject := range []string{"first", "corrected"} {
		require.NoError(t, local.StoreCommitLinks(ctx, []db.CommitLink{{SessionID: "missing", CommitSHA: "abcdef0", Source: "trailer", Subject: subject}}))
		_, err := s.PushWithOptions(ctx, storage.PushOptions{}, nil)
		require.NoError(t, err)
		rows, err := NewStoreFromDB(s.conn).CommitLinks(ctx, []string{"ABCDEF0"}, "")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, subject, rows[0].Subject)
		assert.True(t, rows[0].Gone)
	}
}
