package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry/posthog"
)

func TestEnabledFromEnvHonorsAgentsViewAndGenericOptOut(t *testing.T) {
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	t.Setenv(EnabledEnv, "0")
	assert.False(t, EnabledFromEnv())

	t.Setenv(EnabledEnv, "1")
	assert.True(t, EnabledFromEnv())

	t.Setenv(GenericEnabledEnv, "0")
	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledWithoutProjectKey(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	reporter, err := NewReporter(Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"), InstallationID: "anonymous-install-id", Version: "v1.2.3"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })

	assert.False(t, reporter.Enabled())
	assert.True(t, reporter.EventAllowed(EventDaemonActive))
}

func TestNewReporterOptedOutKeepsAllowlist(t *testing.T) {
	clearTelemetryVetoes(t)
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "0")

	reporter, err := NewReporter(Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })

	assert.False(t, reporter.Enabled())
	assert.True(t, reporter.EventAllowed(EventAppOpened))
	assert.True(t, reporter.EventAllowed(EventDaemonActive))
	assert.False(t, reporter.EventAllowed("daemon_started"))
}

func TestAllowedEventOptionsConfigureDaemonActiveShape(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	opts := Options{
		ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"),
		InstallationID:   "anonymous-install-id", Version: "v1.2.3", Commit: "abc123",
	}
	kitOpts := kitOptions(opts)
	kitOpts.APIKey = "phc_test"
	client, err := kittelemetry.NewReporter(kitOpts, allowedEventOptions(opts)...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	reporter := &Reporter{client: client}

	props, err := reporter.SanitizeProperties(EventDaemonActive, map[string]any{
		"app":     "legacy-app",
		"project": "private-project",
		"session": "private-session",
	})
	require.NoError(t, err)

	assert.Equal(t, "agentsview", props["application"])
	assert.Equal(t, "v1.2.3", props["version"])
	assert.Equal(t, "abc123", props["commit"])
	assert.Equal(t, "daemon", props["source"])
	assert.NotContains(t, props, "app")
	assert.NotContains(t, props, "project")
	assert.NotContains(t, props, "session")
}

func clearTelemetryVetoes(t *testing.T) {
	t.Helper()
	t.Setenv("DO_NOT_TRACK", "0")
	t.Setenv("CI", "")
	require.NoError(t, os.Unsetenv("CI"))
}
