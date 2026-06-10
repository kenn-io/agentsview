package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kittelemetry "go.kenn.io/kit/telemetry"
)

const (
	EnabledEnv               = "AGENTSVIEW_TELEMETRY_ENABLED"
	GenericEnabledEnv        = kittelemetry.GenericTelemetryEnabledEnv
	installIDFilename        = "telemetry-install-id"
	postHogAPIKey            = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	EventDaemonActive        = "daemon_active"
	application              = "agentsview"
	envPrefix                = "AGENTSVIEW"
	defaultInstallIDFilePerm = 0o600
)

var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

type Reporter struct {
	client *kittelemetry.PostHogReporter
}

type Options struct {
	DataDir string
	Version string
	Commit  string
}

func EnabledFromEnv() bool {
	return kittelemetry.PostHogTelemetryEnabledFromEnv(envPrefix)
}

func NewReporter(opts Options) (*Reporter, error) {
	if runningUnderGoTest() || !EnabledFromEnv() {
		return DisabledReporter(), nil
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("telemetry data directory is required")
	}

	distinctID, err := loadOrCreateInstallID(opts.DataDir)
	if err != nil {
		return nil, err
	}

	client, err := newKitReporter(distinctID, opts.Version, opts.Commit)
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

func loadOrCreateInstallID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, installIDFilename)
	if id, err := readInstallID(path); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	id, err := randomInstallID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("creating telemetry data directory: %w", err)
	}

	f, err := os.OpenFile(
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, defaultInstallIDFilePerm,
	)
	if errors.Is(err, os.ErrExist) {
		return readInstallID(path)
	}
	if err != nil {
		return "", fmt.Errorf("creating telemetry install id: %w", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, id); err != nil {
		return "", fmt.Errorf("writing telemetry install id: %w", err)
	}
	return id, nil
}

func readInstallID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("telemetry install id is empty")
	}
	return id, nil
}

func randomInstallID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate telemetry install id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
