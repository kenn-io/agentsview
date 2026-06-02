package telemetry

import (
	"context"
	"os"
	"path/filepath"
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

func TestReporterCaptureActiveUserUsesAnonymousDistinctID(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.CaptureActiveUser(context.Background())
	require.NoError(t, err)

	capture, ok := client.message.(posthog.Capture)
	require.True(t, ok)
	assert.Equal(t, "anonymous-install-id", capture.DistinctId)
	assert.Equal(t, activeUserEvent, capture.Event)
	assert.True(t, capture.Properties["$geoip_disable"].(bool))
	assert.NotContains(t, capture.Properties, "project")
	assert.NotContains(t, capture.Properties, "session")
}

func TestReporterCaptureActiveUserHonorsCanceledContext(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := reporter.CaptureActiveUser(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, client.message)
}
