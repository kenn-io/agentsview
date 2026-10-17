package rawsync

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// Codex cloud mirror files carry no secrets, so raw capture may ship them
// to a hub even though remote host sync must not copy their directory.
func TestRawSyncAcceptsCodexCloud(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateProvider(parser.AgentCodexCloud))
}
