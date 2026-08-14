package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
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

	port, err := server.FindAvailablePort(cfg.Host, cfg.Port)
	if err != nil {
		return cfg, err
	}
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
		ctx, cfg, srv, 5*time.Second, serveErrCh,
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

// waitForBackendReady waits until the started HTTP server proves that the
// readiness request reached this Server instance. The proof uses a temporary
// server-held secret, so a colliding listener never receives the persistent
// bearer token.
func waitForBackendReady(
	ctx context.Context,
	cfg config.Config,
	srv *server.Server,
	timeout time.Duration,
	errCh <-chan error,
) error {
	if err := srv.EnableStartupProbe(); err != nil {
		return err
	}
	defer srv.DisableStartupProbe()

	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort(
		probeHostForDial(cfg.Host), strconv.Itoa(cfg.Port),
	)
	probeURL := "http://" + address + srv.StartupProbePath()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   200 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
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
		challenge, expectedProof, err := srv.StartupProbeChallenge()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return fmt.Errorf("create startup probe request: %w", err)
		}
		server.SetStartupProbeChallenge(req, challenge)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				err = fmt.Errorf(
					"startup probe on %s returned status %d",
					address, resp.StatusCode,
				)
			} else if !server.ValidStartupProbeResponse(resp, expectedProof) {
				err = fmt.Errorf("startup probe on %s returned an invalid proof", address)
			} else {
				return nil
			}
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
