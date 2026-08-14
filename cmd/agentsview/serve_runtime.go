package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/kit/daemon"
)

type serveRuntimeOptions struct {
	Mode          string
	RequestedPort int
}

type serveRuntime struct {
	Cfg        config.Config
	LocalURL   string
	PublicURL  string
	ServeErrCh <-chan error
	Caddy      *managedCaddy
}

func prepareServeRuntimeConfig(
	cfg config.Config,
	opts serveRuntimeOptions,
) (config.Config, error) {
	requestedPort := opts.RequestedPort
	if requestedPort == 0 {
		requestedPort = cfg.Port
	}

	port := server.FindAvailablePort(cfg.Host, cfg.Port)
	if port != cfg.Port {
		if cfg.Port == 0 {
			fmt.Printf("Using available port %d\n", port)
		} else {
			fmt.Printf("Port %d in use, using %d\n", cfg.Port, port)
		}
	}
	cfg.Port = port

	if cfg.Proxy.Mode == "" && cfg.PublicURL != "" {
		updatedURL, updatedOrigins, changed, err := rewriteConfiguredPublicURLPort(
			cfg.PublicURL,
			cfg.PublicOrigins,
			requestedPort,
			cfg.Port,
		)
		if err != nil {
			return cfg, fmt.Errorf("invalid public url: %w", err)
		}
		if changed {
			cfg.PublicURL = updatedURL
			cfg.PublicOrigins = updatedOrigins
		}
	}

	return cfg, nil
}

func startServerWithOptionalCaddy(
	ctx context.Context,
	cfg config.Config,
	srv *server.Server,
	opts serveRuntimeOptions,
) (*serveRuntime, error) {
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.ListenAndServe()
	}()

	if err := waitForBackendReady(
		ctx, cfg, srv.DaemonPingPath(), 5*time.Second, serveErrCh,
	); err != nil {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("server failed to start: %w", err)
	}

	var caddy *managedCaddy
	if cfg.Proxy.Mode == "caddy" {
		var err error
		caddy, err = startManagedCaddy(ctx, cfg, opts.Mode)
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return nil, fmt.Errorf("managed caddy error: %w", err)
		}

		publicPort, err := publicURLPort(cfg.PublicURL)
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			caddy.Stop()
			_ = srv.Shutdown(shutdownCtx)
			return nil, fmt.Errorf("invalid public url: %w", err)
		}
		if err := waitForLocalPort(
			ctx,
			cfg.Proxy.BindHost,
			publicPort,
			5*time.Second,
			caddy.Err(),
		); err != nil {
			shutdownCtx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			caddy.Stop()
			_ = srv.Shutdown(shutdownCtx)
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, fmt.Errorf("managed caddy error: %w", err)
		}
	}

	return &serveRuntime{
		Cfg:        cfg,
		LocalURL:   fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
		PublicURL:  browserURL(cfg),
		ServeErrCh: serveErrCh,
		Caddy:      caddy,
	}, nil
}

// waitForBackendReady waits until the started HTTP server answers the daemon
// ping contract as this process. A bare TCP dial is not enough: on a split
// IPv4/IPv6 port collision an unrelated process can accept the probe
// connection while this server listens on the other family, and another
// AgentsView daemon would answer the ping contract with a different PID.
func waitForBackendReady(
	ctx context.Context,
	cfg config.Config,
	pingPath string,
	timeout time.Duration,
	errCh <-chan error,
) error {
	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort(
		probeHostForDial(cfg.Host), strconv.Itoa(cfg.Port),
	)
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: address,
		Service: daemonService,
	}
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return fmt.Errorf(
					"service exited before becoming ready on %s",
					address,
				)
			}
			return err
		default:
		}
		info, err := probeRuntime(
			ctx, rec, cfg.AuthToken, daemon.ProbeOptions{
				Path:            pingPath,
				ExpectedService: daemonService,
				Timeout:         200 * time.Millisecond,
			},
		)
		if err == nil && info.PID == os.Getpid() {
			return nil
		}
		if err == nil {
			err = fmt.Errorf(
				"daemon ping on %s answered from pid %d, want %d",
				address, info.PID, os.Getpid(),
			)
		}
		lastErr = err
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for %s", address)
	}
	return lastErr
}

func waitForServerRuntime(
	ctx context.Context,
	srv *server.Server,
	rt *serveRuntime,
) error {
	var caddyErrCh <-chan error
	if rt.Caddy != nil {
		caddyErrCh = rt.Caddy.Err()
	}

	select {
	case err := <-rt.ServeErrCh:
		if err != nil && err != http.ErrServerClosed {
			if rt.Caddy != nil {
				rt.Caddy.Stop()
			}
			return fmt.Errorf("server error: %w", err)
		}
		if rt.Caddy != nil {
			rt.Caddy.Stop()
		}
		return nil
	case err := <-caddyErrCh:
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if ctx.Err() != nil {
			if serveErr := <-rt.ServeErrCh; serveErr != nil &&
				serveErr != http.ErrServerClosed {
				return fmt.Errorf("server error: %w", serveErr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("managed caddy error: %w", err)
		}
		return fmt.Errorf("managed caddy exited unexpectedly")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		if rt.Caddy != nil {
			rt.Caddy.Stop()
		}
		if err := srv.Shutdown(shutdownCtx); err != nil &&
			err != http.ErrServerClosed {
			return fmt.Errorf("server shutdown error: %w", err)
		}
		if err := <-rt.ServeErrCh; err != nil &&
			err != http.ErrServerClosed {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	}
}
