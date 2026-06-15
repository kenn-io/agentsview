// ABOUTME: detectTransport picks between the HTTP and direct-DB
// ABOUTME: SessionService backends based on whether a running
// ABOUTME: agentsview daemon is discoverable via its kit runtime record.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/service"
)

type transportMode int

const (
	transportDirect transportMode = iota
	transportHTTP
)

// transport captures how to reach the session-data layer from a
// CLI subcommand. Either the HTTP daemon (URL set) or the local DB.
type transport struct {
	Mode           transportMode
	URL            string
	ReadOnly       bool // daemon runtime ReadOnly flag (true for pg serve)
	DirectReadOnly bool // writable daemon owns DB but is not reachable
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
	if sf := FindDaemonRuntime(dataDir, authToken); sf != nil {
		return transport{
			Mode:     transportHTTP,
			URL:      urlFromDaemonRuntime(sf),
			ReadOnly: sf.ReadOnly,
		}, nil
	}
	if IsDaemonStarting(dataDir) {
		fmt.Fprintln(os.Stderr,
			"server is starting up, waiting...")
		if waitTimeout <= 0 {
			waitTimeout = startupWaitTimeout
		}
		WaitForDaemonStartup(dataDir, waitTimeout, authToken)
		if sf := FindDaemonRuntime(dataDir, authToken); sf != nil {
			return transport{
				Mode:     transportHTTP,
				URL:      urlFromDaemonRuntime(sf),
				ReadOnly: sf.ReadOnly,
			}, nil
		}
	}
	if IsLocalDaemonActive(dataDir, authToken) {
		return transport{
			Mode:           transportDirect,
			DirectReadOnly: true,
		}, nil
	}
	return transport{Mode: transportDirect}, nil
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
		applyClassifierConfig(cfg)
		d, err := db.Open(cfg.DBPath)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"opening db: %w", err,
			)
		}
		cleanup := func() { d.Close() }
		if tr.DirectReadOnly {
			return service.NewReadOnlyBackend(d), cleanup, nil
		}
		// engine is nil — CLI reads don't need it, and Sync
		// is handled via the HTTP daemon when one is running.
		return service.NewDirectBackend(d, nil), cleanup, nil
	}
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
