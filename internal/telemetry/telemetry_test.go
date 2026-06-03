package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePostHogClient struct {
	message posthog.Message
	closed  bool
}

func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error {
	f.closed = true
	return nil
}

func TestNewReporterDisabledByEnvDoesNotCreateInstallID(t *testing.T) {
	t.Setenv(AgentsViewEnabledEnv, "0")
	dir := t.TempDir()

	reporter, err := NewReporter(Options{DataDir: dir})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
	_, err = os.Stat(filepath.Join(dir, installIDFilename))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestGenericTelemetryEnvDisablesReporter(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	dir := t.TempDir()

	reporter, err := NewReporter(Options{DataDir: dir})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
	_, err = os.Stat(filepath.Join(dir, installIDFilename))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestNewReporterDisabledDuringTestsDespiteEnabledEnv(t *testing.T) {
	t.Setenv(AgentsViewEnabledEnv, "1")
	t.Setenv(EnabledEnv, "1")
	dir := t.TempDir()

	reporter, err := NewReporter(Options{DataDir: dir})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
	_, err = os.Stat(filepath.Join(dir, installIDFilename))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLoadOrCreateInstallIDIsStableAndAnonymous(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreateInstallID(dir)
	require.NoError(t, err)
	second, err := loadOrCreateInstallID(dir)
	require.NoError(t, err)

	assert.Len(t, first, 32)
	assert.Equal(t, first, second)

	stored, err := os.ReadFile(filepath.Join(dir, installIDFilename))
	require.NoError(t, err)
	assert.Equal(t, first+"\n", string(stored))
}

func TestDaemonActiveCaptureUsesAnonymousDistinctID(t *testing.T) {
	capture := daemonActiveCapture(
		"anonymous-install-id", "v1.2.3", "abc123",
	)

	assert.Equal(t, "anonymous-install-id", capture.DistinctId)
	assert.Equal(t, daemonActiveEvent, capture.Event)
	assert.False(t, capture.Properties["$process_person_profile"].(bool))
	assert.True(t, capture.Properties["$geoip_disable"].(bool))
	assert.Equal(t, "agentsview", capture.Properties["application"])
	assert.Equal(t, "v1.2.3", capture.Properties["version"])
	assert.Equal(t, "abc123", capture.Properties["commit"])
	assert.Equal(t, runtime.GOOS, capture.Properties["goos"])
	assert.Equal(t, runtime.GOARCH, capture.Properties["goarch"])
	assert.Equal(t, "daemon", capture.Properties["source"])
	assert.NotContains(t, capture.Properties, "project")
	assert.NotContains(t, capture.Properties, "session")
}

func TestReporterCaptureDaemonActiveNoopsDuringTests(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
		version:    "v1.2.3",
		commit:     "abc123",
	}

	err := reporter.CaptureDaemonActive(context.Background())
	require.NoError(t, err)
	assert.Nil(t, client.message)
}

func TestDaemonActivePropertiesForcePrivacyFields(t *testing.T) {
	props := daemonActiveProperties(map[string]any{
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "other",
		"version":                 "caller-version",
		"commit":                  "caller-commit",
		"goos":                    "caller-os",
		"goarch":                  "caller-arch",
		"source":                  "caller-source",
	}, "v1.2.3", "abc123")

	assert.False(t, props["$process_person_profile"].(bool))
	assert.True(t, props["$geoip_disable"].(bool))
	assert.Equal(t, "agentsview", props["application"])
	assert.Equal(t, "v1.2.3", props["version"])
	assert.Equal(t, "abc123", props["commit"])
	assert.Equal(t, runtime.GOOS, props["goos"])
	assert.Equal(t, runtime.GOARCH, props["goarch"])
	assert.Equal(t, "daemon", props["source"])
	assert.NotContains(t, props, "app")
}

func TestReporterCaptureDaemonActiveTestBlockerWinsOverCanceledContext(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := reporter.CaptureDaemonActive(ctx)
	require.NoError(t, err)
	assert.Nil(t, client.message)
}
