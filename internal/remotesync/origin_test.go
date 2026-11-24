package remotesync

import (
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The collector records the installation identity the remote reports, so
// reporting names the host's sessions by the origin the host's own archive
// uses.
func TestPrepareRecordsRemoteInstallationIdentity(t *testing.T) {
	const identity = "fedcba9876543210fedcba9876543210"
	targetsJSON, err := json.Marshal(TargetSet{
		Dirs:           map[parser.AgentType][]string{parser.AgentClaude: {"/remote"}},
		InstallationID: identity,
	})
	require.NoError(t, err)
	server := newCurrentProtocolServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			_, _ = w.Write(targetsJSON)
		case "/api/v1/remote-sync/manifest":
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "active.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	prepared, err := RemoteSync{
		Host: "devbox", Source: HTTPSource{URL: server.URL}, DataDir: t.TempDir(), DB: database,
	}.Prepare(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	var recorded string
	require.NoError(t, database.Reader().QueryRowContext(t.Context(),
		`SELECT value FROM pg_sync_state WHERE key = ?`, db.MachineOriginKeyPrefix+"devbox").Scan(&recorded))
	assert.Equal(t, identity, recorded)
}
