package telemetry

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry"
)

func TestEnabledFromEnvHonorsAgentsViewAndGenericOptOut(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	assert.False(t, EnabledFromEnv())

	t.Setenv(EnabledEnv, "1")
	if kittelemetry.PostHogTelemetryDisabled() {
		assert.False(t, EnabledFromEnv())
		return
	}
	assert.True(t, EnabledFromEnv())

	t.Setenv(GenericEnabledEnv, "0")
	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledByEnv(t *testing.T) {
	t.Setenv(EnabledEnv, "0")

	reporter, err := NewReporter(Options{InstallationID: "anonymous-install-id"})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestGenericTelemetryEnvDisablesReporter(t *testing.T) {
	t.Setenv(GenericEnabledEnv, "0")

	reporter, err := NewReporter(Options{InstallationID: "anonymous-install-id"})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestNewReporterDisabledDuringTestsDespiteEnabledEnv(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	reporter, err := NewReporter(Options{InstallationID: "anonymous-install-id"})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestAllowedEventOptionsConfigureDaemonActiveShape(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	client, err := newKitReporter(
		"anonymous-install-id", "v1.2.3", "abc123",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	reporter := &Reporter{client: client}

	assert.True(t, reporter.EventAllowed(EventDaemonActive))
	assert.False(t, reporter.EventAllowed("daemon_started"))

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

func TestReporterCaptureDaemonActiveNoopsDuringTests(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	client, err := newKitReporter(
		"anonymous-install-id", "v1.2.3", "abc123",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	reporter := &Reporter{client: client}
	assert.True(t, reporter.Enabled())

	err = reporter.CaptureDaemonActive(context.Background())
	require.NoError(t, err)
}

func TestReporterCaptureDaemonActiveTestBlockerWinsOverCanceledContext(t *testing.T) {
	client := kittelemetry.DisabledPostHogReporter()
	reporter := &Reporter{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := reporter.CaptureDaemonActive(ctx)
	require.NoError(t, err)
}
