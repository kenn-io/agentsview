package rawcheckpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestRequireBackfillPreservesLookupErrors(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	_, err := store.BeginBackfill(t.Context(), BackfillRunSpec{
		RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example",
		Providers: []parser.AgentType{parser.AgentClaude},
		Roots:     []BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: root.ID}},
	})
	require.NoError(t, err)
	conn, err := store.db.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()

	_, err = requireBackfillConn(t.Context(), conn, "missing-run")
	require.ErrorIs(t, err, ErrBackfillConflict)

	// Cancel after acquiring the real connection to exercise lookup error
	// classification rather than database connection acquisition.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = requireBackfillConn(ctx, conn, "run-a")
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrBackfillConflict)
}
