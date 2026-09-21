package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry"
)

const (
	EnabledEnv        = "AGENTSVIEW_TELEMETRY_ENABLED"
	GenericEnabledEnv = kittelemetry.GenericTelemetryEnabledEnv
	postHogAPIKey     = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	EventDaemonActive = "daemon_active"
	application       = "agentsview"
	envPrefix         = "AGENTSVIEW"
)

var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

type Reporter struct {
	client *kittelemetry.PostHogReporter
}

type Options struct {
	InstallationID string
	Version        string
	Commit         string
}

func EnabledFromEnv() bool {
	return kittelemetry.PostHogTelemetryEnabledFromEnv(envPrefix)
}

func NewReporter(opts Options) (*Reporter, error) {
	if runningUnderGoTest() || !EnabledFromEnv() {
		return DisabledReporter(), nil
	}
	if strings.TrimSpace(opts.InstallationID) == "" {
		return nil, errors.New("installation ID is required")
	}

	client, err := newKitReporter(opts.InstallationID, opts.Version, opts.Commit)
	if err != nil {
		return nil, err
	}
	return &Reporter{client: client}, nil
}

func DisabledReporter() *Reporter {
	return &Reporter{client: kittelemetry.DisabledPostHogReporter()}
}

func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		slog.Warn("telemetry disabled", "err", err)
		return DisabledReporter()
	}
	return reporter
}

func (r *Reporter) Enabled() bool {
	return r != nil && r.client != nil && r.client.Enabled()
}

func (r *Reporter) CaptureDaemonActive(ctx context.Context) error {
	if runningUnderGoTest() || !r.Enabled() {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	return r.client.Capture(EventDaemonActive, nil)
}

func (r *Reporter) EventAllowed(event string) bool {
	return r != nil && r.client != nil && r.client.EventAllowed(event)
}

func (r *Reporter) SanitizeProperties(
	event string,
	properties map[string]any,
) (map[string]any, error) {
	if r == nil || r.client == nil {
		return nil, ErrUnsupportedEvent
	}
	return r.client.SanitizeProperties(event, properties)
}

func runningUnderGoTest() bool {
	return testing.Testing()
}

func (r *Reporter) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

func newKitReporter(
	distinctID, version, commit string,
) (*kittelemetry.PostHogReporter, error) {
	return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Application: application,
		EnvPrefix:   envPrefix,
		DistinctID:  distinctID,
		Version:     version,
		Commit:      commit,
		Source:      "daemon",
	}, allowedEventOptions()...)
}

func allowedEventOptions() []kittelemetry.PostHogOption {
	return []kittelemetry.PostHogOption{
		kittelemetry.WithAllowedEvent(EventDaemonActive),
	}
}
