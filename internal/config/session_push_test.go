package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPushConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		c := Config{AcceptSessionPush: enabled}
		err := c.validateSessionPush()
		if enabled {
			require.ErrorContains(t, err, "require_auth")
		} else {
			require.NoError(t, err)
		}
		c.RequireAuth = true
		require.NoError(t, c.validateSessionPush())
	}
	c := Config{}
	require.NoError(t, c.applyConfigTOML("accept_session_push = true\nrequire_auth = true\n"))
	assert.True(t, c.AcceptSessionPush)
}

func TestSessionPushDeploymentOptIn(t *testing.T) {
	for _, value := range []string{"true", "false", "invalid"} {
		t.Run(value, func(t *testing.T) {
			unsetDeploymentEnv(t)
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv("AGENTSVIEW_ACCEPT_SESSION_PUSH", value)
			cfg := Config{RequireAuth: true, AcceptSessionPush: value == "false"}
			err := cfg.applyDeploymentEnv()
			if value == "invalid" {
				require.ErrorContains(t, err, "AGENTSVIEW_ACCEPT_SESSION_PUSH")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, value == "true", cfg.AcceptSessionPush)
		})
	}
}

func TestSessionPushLocalScopeIgnoresReceiverDeploymentSetting(t *testing.T) {
	unsetDeploymentEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_ACCEPT_SESSION_PUSH", "invalid")
	_, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err, "a sender must not load unused receiver settings")
}
