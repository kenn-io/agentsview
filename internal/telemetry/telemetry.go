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
	"runtime"
	"strings"
	"time"

	"github.com/posthog/posthog-go"
)

const (
	EnabledEnv               = "TELEMETRY_ENABLED"
	AgentsViewEnabledEnv     = "AGENTSVIEW_TELEMETRY_ENABLED"
	installIDFilename        = "telemetry-install-id"
	postHogAPIKey            = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	postHogEndpoint          = "https://us.i.posthog.com"
	activeUserEvent          = "user_active"
	captureTimeout           = 2 * time.Second
	defaultInstallIDFilePerm = 0o600
)

type Reporter struct {
	client     enqueueCloser
	distinctID string
	enabled    bool
}

type enqueueCloser interface {
	Enqueue(posthog.Message) error
	Close() error
}

type Options struct {
	DataDir string
	Version string
	Commit  string
}

func EnabledFromEnv() bool {
	for _, name := range []string{EnabledEnv, AgentsViewEnabledEnv} {
		if strings.TrimSpace(os.Getenv(name)) == "0" {
			return false
		}
	}
	return true
}

func NewReporter(opts Options) (*Reporter, error) {
	if !EnabledFromEnv() {
		return DisabledReporter(), nil
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("telemetry data directory is required")
	}

	distinctID, err := loadOrCreateInstallID(opts.DataDir)
	if err != nil {
		return nil, err
	}

	disableGeoIP := true
	maxRetries := 0
	client, err := posthog.NewWithConfig(postHogAPIKey, posthog.Config{
		Endpoint:           postHogEndpoint,
		DisableGeoIP:       &disableGeoIP,
		BatchSize:          1,
		Interval:           time.Second,
		BatchUploadTimeout: captureTimeout,
		ShutdownTimeout:    captureTimeout,
		MaxRetries:         &maxRetries,
		DefaultEventProperties: posthog.Properties{
			"app":            "agentsview",
			"source":         "backend",
			"version":        opts.Version,
			"commit":         opts.Commit,
			"goos":           runtime.GOOS,
			"goarch":         runtime.GOARCH,
			"$geoip_disable": true,
		},
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:     client,
		distinctID: distinctID,
		enabled:    true,
	}, nil
}

func DisabledReporter() *Reporter {
	return &Reporter{}
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
	return r != nil && r.enabled && r.client != nil
}

func (r *Reporter) CaptureActiveUser(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	return r.client.Enqueue(posthog.Capture{
		DistinctId: r.distinctID,
		Event:      activeUserEvent,
		Timestamp:  time.Now().UTC(),
		Properties: posthog.Properties{
			"$geoip_disable": true,
		},
	})
}

func (r *Reporter) Close() error {
	if !r.Enabled() {
		return nil
	}
	return r.client.Close()
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
