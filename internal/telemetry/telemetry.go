package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	kittelemetry "go.kenn.io/kit/telemetry"
)

const (
	EnabledEnv            = "AGENTSVIEW_TELEMETRY_ENABLED"
	GenericEnabledEnv     = kittelemetry.GenericTelemetryEnabledEnv
	postHogAPIKey         = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	EventDaemonActive     = "daemon_active"
	EventAppOpened        = "app_opened"
	EventSearchRun        = "search_run"
	EventSessionViewed    = "session_viewed"
	EventExportRun        = "export_run"
	EventInsightGenerated = "insight_generated"
	EventAnalyticsViewed  = "analytics_viewed"
	application           = "agentsview"
	envPrefix             = "AGENTSVIEW"
)

var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

type Reporter struct {
	client *kittelemetry.PostHogReporter
}

type Options struct {
	InstallationID string
	// InstalledAt is when InstallationID was created. Reports carry its age as
	// install_age_hours; zero sends them without an age.
	InstalledAt  time.Time
	Version      string
	Commit       string
	AgentTypes   []string
	InsightKinds []string
}

func EnabledFromEnv() bool {
	return kittelemetry.PostHogTelemetryEnabledFromEnv(envPrefix)
}

func NewReporter(opts Options) (*Reporter, error) {
	if !EnabledFromEnv() {
		// kit keeps the allowlist on an opted-out reporter, so the UI route still rejects unknown events.
		client, err := newKitReporter(opts)
		if err != nil {
			return nil, err
		}
		return &Reporter{client: client}, nil
	}
	if runningUnderGoTest() {
		return DisabledReporter(), nil
	}
	if strings.TrimSpace(opts.InstallationID) == "" {
		return nil, errors.New("installation ID is required")
	}

	client, err := newKitReporter(opts)
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

// CaptureHandler lets the web UI report allowlisted events through this reporter.
func (r *Reporter) CaptureHandler() http.Handler {
	var client *kittelemetry.PostHogReporter
	if r != nil {
		client = r.client
	}
	return kittelemetry.NewPostHogCaptureHandler(client)
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

func newKitReporter(opts Options) (*kittelemetry.PostHogReporter, error) {
	return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Application: application,
		EnvPrefix:   envPrefix,
		DistinctID:  opts.InstallationID,
		InstalledAt: opts.InstalledAt,
		Version:     opts.Version,
		Commit:      opts.Commit,
		Source:      "daemon",
	}, allowedEventOptions(opts)...)
}

func allowedEventOptions(opts Options) []kittelemetry.PostHogOption {
	return []kittelemetry.PostHogOption{
		kittelemetry.WithAllowedEvent(EventDaemonActive),
		kittelemetry.WithAllowedEvent(EventAppOpened),
		oneOf(EventSearchRun, "query_type", "text", "semantic", "hybrid"),
		oneOf(EventSessionViewed, "agent", opts.AgentTypes...),
		oneOf(EventExportRun, "format", "html", "insight_html", "csv", "markdown_link", "gist", "insight_gist"),
		oneOf(EventInsightGenerated, "kind", opts.InsightKinds...),
		oneOf(EventAnalyticsViewed, "page", "usage", "activity", "trends", "quality"),
	}
}

func oneOf(event, property string, values ...string) kittelemetry.PostHogOption {
	return kittelemetry.WithAllowedEvent(event,
		kittelemetry.AllowTelemetryProperty(property, kittelemetry.AllowTelemetryStringValues(values...)))
}
