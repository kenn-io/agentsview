package telemetry

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestNewReporterDisabledDuringTestsDespiteEnabledEnv(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	clearTelemetryVetoes(t)
	t.Setenv(GenericEnabledEnv, "1")

	reporter, err := NewReporter(Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"), InstallationID: "anonymous-install-id", Version: "v1.2.3"})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestNewReporterOptedOutKeepsAllowlist(t *testing.T) {
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

	client, err := newKitReporter(Options{
		ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"),
		InstallationID:   "anonymous-install-id", Version: "v1.2.3", Commit: "abc123",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	reporter := &Reporter{client: client}

	props, err := reporter.SanitizeProperties(EventDaemonActive, map[string]any{
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "other",
		"version":                 "caller-version",
		"commit":                  "caller-commit",
		"goos":                    "caller-os",
		"goarch":                  "caller-arch",
		"source":                  "caller-source",
		"app":                     "legacy-app",
		"project":                 "private-project",
		"session":                 "private-session",
	})
	require.NoError(t, err)

	assert.False(t, props["$process_person_profile"].(bool))
	assert.True(t, props["$geoip_disable"].(bool))
	assert.Equal(t, "agentsview", props["application"])
	assert.Equal(t, "v1.2.3", props["version"])
	assert.Equal(t, "abc123", props["commit"])
	assert.Equal(t, runtime.GOOS, props["goos"])
	assert.Equal(t, runtime.GOARCH, props["goarch"])
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

func TestNewKitReporterAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, version, env, value string
		enabled                   bool
	}{
		{name: "release", version: "v1.2.3", enabled: true},
		{name: "do not track", version: "v1.2.3", env: "DO_NOT_TRACK", value: "1"},
		{name: "present empty CI", version: "v1.2.3", env: "CI"},
		{name: "development", version: "dev"},
		{name: "fork", version: "v1.2.3-fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearTelemetryVetoes(t)
			t.Setenv(EnabledEnv, "1")
			t.Setenv(GenericEnabledEnv, "1")
			if tc.env != "" {
				t.Setenv(tc.env, tc.value)
			}
			client, err := newKitReporter(Options{
				InstallationID: "anonymous-install-id", Version: tc.version,
				ScreenClaimsPath: filepath.Join(t.TempDir(), "screens"),
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			assert.Equal(t, tc.enabled, client.Enabled())
		})
	}
}
