// ABOUTME: detectTransport picks between the HTTP and direct-DB
// ABOUTME: SessionService backends based on whether a running
// ABOUTME: agentsview daemon is discoverable via its kit runtime record.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/update"
)

type transportMode int

const (
	transportDirect transportMode = iota
	transportHTTP
)

type transportIntent int

const (
	transportIntentRead transportIntent = iota
	transportIntentArchiveWrite
)

var errLocalDaemonUnreachable = errors.New(
	"local daemon owns the SQLite archive but is not responding",
)

var startBackgroundServeForTransport = ensureBackgroundServe
var waitForDaemonStartupForTransport = WaitForDaemonStartupContext

// transport captures how to reach the session-data layer from a
// CLI subcommand. Either the HTTP daemon (URL set) or the local DB.
type transport struct {
	Mode               transportMode
	URL                string
	ReadOnly           bool // daemon runtime ReadOnly flag (true for pg serve)
	DirectReadOnly     bool // writable daemon owns DB but is not reachable
	DirectIncompatible bool // live daemon owns DB but cannot serve this client
	DirectReason       string
	Runtime            *DaemonRuntime
}

type customPricingStore interface {
	SetCustomPricing(map[string]config.CustomModelRate)
}

var openPGReadStore = func(
	cfg config.Config,
	pgCfg config.PGConfig,
) (db.Store, func(), error) {
	applyClassifierConfig(cfg)
	store, err := postgres.NewStore(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { _ = store.Close() }, nil
}

// detectTransport picks the transport mode:
//  1. If a kit runtime record points to a live daemon, use HTTP.
//  2. If a daemon start lock exists, wait up to waitTimeout for the
//     daemon to become ready, then try again.
//  3. If a writable local daemon owns the SQLite archive but is not
//     reachable by ping, use direct read-only access.
//  4. Otherwise use direct access.
func detectTransport(
	dataDir string, authToken string, waitTimeout time.Duration,
) (transport, error) {
	return detectTransportContext(
		context.Background(), dataDir, authToken, waitTimeout,
	)
}

func detectTransportContext(
	ctx context.Context,
	dataDir string,
	authToken string,
	waitTimeout time.Duration,
) (transport, error) {
	if err := ctx.Err(); err != nil {
		return transport{}, err
	}
	if isExternalDaemonStarting(dataDir) {
		fmt.Fprintln(os.Stderr,
			"server is starting up, waiting...")
		if waitTimeout <= 0 {
			waitTimeout = startupWaitTimeout
		}
		_, _, err := waitForExternalServeStartup(
			ctx, dataDir, authToken, waitTimeout,
		)
		if err != nil && errors.Is(err, errServeStartupInProgress) {
			return transport{}, err
		}
		if err := ctx.Err(); err != nil {
			return transport{}, err
		}
	}
	if sf := FindDaemonRuntime(dataDir, authToken); sf != nil {
		return transportFromRuntime(sf), nil
	}
	if IsDaemonStarting(dataDir) {
		fmt.Fprintln(os.Stderr,
			"server is starting up, waiting...")
		if waitTimeout <= 0 {
			waitTimeout = startupWaitTimeout
		}
		waitForDaemonStartupForTransport(
			ctx, dataDir, waitTimeout, authToken,
		)
		if err := ctx.Err(); err != nil {
			return transport{}, err
		}
		if sf := FindDaemonRuntime(dataDir, authToken); sf != nil {
			return transportFromRuntime(sf), nil
		}
	}
	if IsLocalDaemonActive(dataDir, authToken) {
		reason := errLocalDaemonUnreachable.Error()
		incompatible := false
		if _, err := FindIncompatibleDaemonRuntime(dataDir, authToken); err != nil {
			reason = err.Error()
			incompatible = true
		}
		return transport{
			Mode:               transportDirect,
			DirectReadOnly:     true,
			DirectIncompatible: incompatible,
			DirectReason:       reason,
		}, nil
	}
	return transport{Mode: transportDirect}, nil
}

func ensureTransport(
	cfg *config.Config,
	intent transportIntent,
	waitTimeout time.Duration,
) (transport, error) {
	return ensureTransportContext(
		context.Background(), cfg, intent, waitTimeout,
	)
}

func ensureTransportContext(
	ctx context.Context,
	cfg *config.Config,
	intent transportIntent,
	waitTimeout time.Duration,
) (transport, error) {
	if cfg == nil {
		return transport{}, errors.New("nil config")
	}
	if err := ctx.Err(); err != nil {
		return transport{}, err
	}
	if (intent == transportIntentRead || intent == transportIntentArchiveWrite) &&
		waitTimeout <= 0 {
		waitTimeout = backgroundAutoStartReadyTimeout
	}
	if intent == transportIntentArchiveWrite {
		waited, err := waitForBackgroundLaunchBeforeArchiveWrite(
			ctx, cfg.DataDir, waitTimeout,
		)
		if err != nil {
			return transport{}, err
		}
		if waited && cfg.AuthToken == "" {
			adoptBackgroundLaunchConfig(cfg)
		}
	}
	tr, err := detectTransportContext(
		ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
	)
	if err != nil {
		return transport{}, err
	}
	if intent == transportIntentRead && cfg.AuthToken == "" &&
		tr.Mode == transportDirect && tr.DirectReadOnly {
		adoptBackgroundLaunchConfig(cfg)
		if cfg.AuthToken != "" {
			tr, err = detectTransportContext(
				ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
			)
			if err != nil {
				return transport{}, err
			}
		}
	}
	if tr.Mode == transportHTTP {
		if (intent == transportIntentRead ||
			intent == transportIntentArchiveWrite) &&
			shouldUpgradeDaemonRuntime(tr.Runtime, version) {
			if daemonAutostartDisabled() {
				if intent == transportIntentRead {
					return transport{}, appendDaemonRestartUpgradeHint(
						errors.New("daemon restart required: running daemon is older than this client"),
					)
				}
				return tr, nil
			}
			if err := guardDaemonAutoStartConfig(*cfg); err != nil {
				return transport{}, err
			}
			cfg.NoSync = cfg.NoSync || tr.Runtime.NoSync
			rt, err := startBackgroundServeForTransport(
				ctx, cfg, waitTimeout,
			)
			if err != nil {
				return transport{}, err
			}
			return transportFromRuntime(rt), nil
		}
		return tr, nil
	}
	if (intent == transportIntentRead || intent == transportIntentArchiveWrite) &&
		!daemonAutostartDisabled() {
		if rt, err := FindIncompatibleDaemonRuntime(
			cfg.DataDir, cfg.AuthToken,
		); err != nil && rt != nil &&
			shouldUpgradeIncompatibleDaemonRuntime(rt, version) {
			if err := guardDaemonAutoStartConfig(*cfg); err != nil {
				return transport{}, err
			}
			cfg.NoSync = cfg.NoSync || rt.NoSync
			rt, err := startBackgroundServeForTransport(
				ctx, cfg, waitTimeout,
			)
			if err != nil {
				return transport{}, err
			}
			return transportFromRuntime(rt), nil
		}
	}
	if intent == transportIntentRead {
		if daemonAutostartDisabled() {
			return transport{}, errors.New(
				"daemon autostart is disabled; direct SQLite reads are " +
					"not supported for this command. Start a daemon with " +
					"`agentsview serve --background` or unset " +
					"AGENTSVIEW_NO_DAEMON",
			)
		}
		if tr.DirectReadOnly {
			if tr.DirectReason != "" {
				if tr.DirectReason == errLocalDaemonUnreachable.Error() {
					return transport{}, errLocalDaemonUnreachable
				}
				return transport{}, appendDaemonRestartUpgradeHint(
					errors.New(tr.DirectReason),
				)
			}
			return transport{}, errLocalDaemonUnreachable
		}
		if err := guardDaemonAutoStartConfig(*cfg); err != nil {
			return transport{}, err
		}
		rt, err := startBackgroundServeForTransport(ctx, cfg, waitTimeout)
		if err != nil {
			return transport{}, err
		}
		return transportFromRuntime(rt), nil
	}
	if daemonAutostartDisabled() {
		return tr, nil
	}
	if tr.DirectReadOnly {
		if tr.DirectReason != "" {
			if tr.DirectReason == errLocalDaemonUnreachable.Error() {
				return transport{}, errLocalDaemonUnreachable
			}
			return transport{}, errors.New(tr.DirectReason)
		}
		return transport{}, errLocalDaemonUnreachable
	}
	if err := guardDaemonAutoStartConfig(*cfg); err != nil {
		return transport{}, err
	}
	rt, err := startBackgroundServeForTransport(ctx, cfg, waitTimeout)
	if err != nil {
		return transport{}, err
	}
	return transportFromRuntime(rt), nil
}

func waitForBackgroundLaunchBeforeArchiveWrite(
	ctx context.Context,
	dataDir string,
	waitTimeout time.Duration,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("data dir %q is not a directory", dataDir)
	}
	if !isBackgroundLaunchActive(dataDir) {
		return false, nil
	}
	if waitTimeout <= 0 {
		waitTimeout = backgroundAutoStartReadyTimeout
	}
	deadline := time.Now().Add(waitTimeout)
	for isBackgroundLaunchActive(dataDir) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true, errServeStartupInProgress
		}
		timer := time.NewTimer(min(remaining, startProbeTick()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return true, ctx.Err()
		case <-timer.C:
		}
	}
	return true, ctx.Err()
}

func shouldUpgradeDaemonRuntime(rt *DaemonRuntime, currentVersion string) bool {
	if rt == nil || rt.ReadOnly {
		return false
	}
	if update.IsDevBuildVersion(currentVersion) {
		return false
	}
	if rt.Record.Version == "" {
		return true
	}
	return update.IsNewer(currentVersion, rt.Record.Version)
}

func shouldUpgradeIncompatibleDaemonRuntime(
	rt *DaemonRuntime, currentVersion string,
) bool {
	if rt == nil {
		return false
	}
	if !shouldUpgradeDaemonRuntime(rt, currentVersion) {
		return false
	}
	if rt.API > daemonAPIVersion || rt.Data > db.CurrentDataVersion() {
		return false
	}
	return true
}

func guardDaemonAutoStartConfig(cfg config.Config) error {
	host := strings.TrimSpace(cfg.Host)
	if host == "" || cfg.RequireAuth || isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf(
		"refusing to auto-start an unauthenticated daemon on non-loopback "+
			"host %q; enable require_auth, bind serve to 127.0.0.1, "+
			"start 'agentsview serve --background' explicitly, or set "+
			"AGENTSVIEW_NO_DAEMON=1 for direct local writes",
		host,
	)
}

func daemonAutostartDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AGENTSVIEW_NO_DAEMON")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// transportFromRuntime builds the HTTP transport a CLI client uses to reach a
// resolved daemon runtime.
func transportFromRuntime(rt *DaemonRuntime) transport {
	return transport{
		Mode:     transportHTTP,
		URL:      urlFromDaemonRuntime(rt),
		ReadOnly: rt.ReadOnly,
		Runtime:  rt,
	}
}

// urlFromDaemonRuntime returns the HTTP URL a CLI client should use
// to reach the daemon described by rt. Bind-all addresses are
// mapped to loopback. IPv6 hosts are bracketed via
// net.JoinHostPort so the URL is well-formed.
func urlFromDaemonRuntime(rt *DaemonRuntime) string {
	host := rt.Host
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(rt.Port))
}

// newService builds the SessionService matching the detected
// transport. The returned cleanup function must be called when
// the caller is done with the service.
func newService(
	cfg config.Config, tr transport,
) (service.SessionService, func(), error) {
	switch tr.Mode {
	case transportHTTP:
		return service.NewHTTPBackend(tr.URL, cfg.AuthToken, tr.ReadOnly),
			func() {}, nil
	default:
		if err := directIncompatibleDaemonError(tr); err != nil {
			return nil, nil, err
		}
		d, err := openReadOnlyDB(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"opening db: %w", err,
			)
		}
		closeVectorSearcher, err := installDirectVectorSearcher(cfg, d)
		if err != nil {
			d.Close()
			return nil, nil, err
		}
		cleanup := func() {
			if closeVectorSearcher != nil {
				if cerr := closeVectorSearcher(); cerr != nil {
					log.Printf("close vectors.db: %v", cerr)
				}
			}
			d.Close()
		}
		// engine is nil — CLI reads don't need it, and Sync
		// is handled via the HTTP daemon when one is running.
		return service.NewDirectBackend(d, nil), cleanup, nil
	}
}

func directIncompatibleDaemonError(tr transport) error {
	if !tr.DirectIncompatible {
		return nil
	}
	reason := strings.TrimSpace(tr.DirectReason)
	if reason == "" {
		reason = "local daemon is incompatible with this agentsview client"
	}
	return appendDaemonRestartUpgradeHint(errors.New(reason))
}

// newPGReadService builds a read-only SessionService over the
// configured PostgreSQL sync store. It shares the same store
// construction path as pg serve, but leaves schema repair/migration
// to pg push/serve because CLI read commands never mutate PG.
func newPGReadService(
	cfg config.Config, pgCfg config.PGConfig,
) (service.SessionService, func(), error) {
	store, cleanup, err := openPGReadStore(cfg, pgCfg)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("opening pg store: %w", err)
	}
	if len(cfg.CustomModelPricing) > 0 {
		if priced, ok := store.(customPricingStore); ok {
			priced.SetCustomPricing(cfg.CustomModelPricing)
		}
	}
	return service.NewReadOnlyBackend(store), cleanup, nil
}
