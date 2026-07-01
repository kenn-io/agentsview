// ABOUTME: `agentsview mcp` subcommand — serves the read-only MCP tools
// ABOUTME: over the SessionService seam (stdio by default, or HTTP).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
	"go.kenn.io/agentsview/internal/service"
)

func newMCPCommand() *cobra.Command {
	var httpAddr string
	var httpAllowInsecure bool

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server exposing read-only session retrieval tools",
		Long: `Start an MCP (Model Context Protocol) server over stdio (default) or
StreamableHTTP, exposing read-only tools for searching and reading
recorded agent sessions: search_sessions, list_sessions,
get_session_overview, get_messages, search_content, and
get_usage_summary.

The server reads through the daemon path. By default each tool call talks to
the local agentsview daemon, starting it when needed so a long-lived MCP server
can recover after the daemon exits due to idleness. Use --server to target an
explicit daemon URL.

Add to your MCP client config (e.g. Claude Desktop):
  {
    "mcpServers": {
      "agentsview": {
        "command": "agentsview",
        "args": ["mcp"]
      }
    }
  }`,
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, cleanup, err := resolveMCPService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			// The CLI runs commands with a plain context (no signal
			// handling), so install our own: a long-lived MCP server must
			// shut down cleanly on SIGINT/SIGTERM.
			ctx, stop := signal.NotifyContext(
				cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			opts := mcpserver.ServeOptions{Service: svc, Version: version}

			var serveErr error
			if httpAddr != "" {
				addr, err := normalizeMCPHTTPAddr(httpAddr, httpAllowInsecure)
				if err != nil {
					return err
				}
				// A non-loopback listener must be authenticated, or it is
				// an unauthenticated remote read surface over the session
				// archive. Loopback binds stay local-trust (no listener
				// auth), matching the daemon.
				cfg, err := config.LoadPFlags(cmd.Flags())
				if err != nil {
					return fmt.Errorf("loading config: %w", err)
				}
				// Provision a token when auth is required but none exists
				// yet, matching serve/pg/duckdb so enabling require_auth is
				// sufficient for first-time authenticated startup.
				if cfg.RequireAuth && cfg.AuthToken == "" {
					if err := cfg.EnsureAuthToken(); err != nil {
						return fmt.Errorf("provisioning auth token: %w", err)
					}
				}
				token, err := mcpListenerAuth(addr, cfg.AuthToken, cfg.RequireAuth)
				if err != nil {
					return err
				}
				opts.Token = token
				serveErr = mcpserver.ServeHTTP(ctx, opts, addr)
			} else {
				serveErr = mcpserver.ServeStdio(ctx, opts)
			}
			// A SIGINT/SIGTERM-triggered shutdown cancels ctx; that is a
			// clean stop, not a failure, so it should not exit non-zero.
			if errors.Is(serveErr, context.Canceled) {
				return nil
			}
			return serveErr
		},
	}

	cmd.Flags().StringVar(&httpAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8085) "+
			"instead of stdio. Bare port forms (':8085', '8085') bind to "+
			"loopback only; non-loopback hosts require --http-allow-insecure.")
	cmd.Flags().BoolVar(&httpAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address. A non-loopback bind "+
			"requires a configured auth token (auth_token in config.toml, or "+
			"enable require_auth) and then enforces Authorization: Bearer on "+
			"every request. Only expose it on trusted networks (Tailscale, "+
			"VPN-only) or behind an authenticating reverse proxy.")

	// Transport-selection flags, mirroring the `session` command.
	// Implicit local SQLite reads are daemon-backed; explicit --server
	// and --pg select their requested remote/read-store backends.
	cmd.Flags().String("server", "", "Remote daemon URL")
	cmd.Flags().String("server-token-file", "",
		"File containing bearer token for explicit --server requests")
	cmd.Flags().Bool("pg", false,
		"Read session data from configured PostgreSQL")

	return cmd
}

// resolveMCPService constructs the SessionService used by the long-lived
// MCP server. The implicit local path is intentionally daemon-only and
// lazy: every operation re-resolves the daemon transport so a tool call can
// wake the daemon after it exits due to idleness.
func resolveMCPService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	if remote != "" {
		if pgReadRequested(cmd) {
			return nil, nil, errors.New(
				"--server and --pg are mutually exclusive",
			)
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, nil, err
		}
		return service.NewHTTPBackend(remote, token, false),
			func() {}, nil
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
	if err != nil {
		return nil, nil, err
	}
	if usePG {
		return newPGReadService(cfg, pgCfg)
	}
	return newMCPDaemonService(cfg), func() {}, nil
}

type mcpDaemonService struct {
	mu  sync.Mutex
	cfg config.Config
}

func newMCPDaemonService(cfg config.Config) service.SessionService {
	return &mcpDaemonService{cfg: cfg}
}

func (s *mcpDaemonService) daemonService(
	ctx context.Context,
) (service.SessionService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.cfg
	tr, err := ensureTransportContext(
		ctx, &cfg, transportIntentArchiveWrite, 0,
	)
	if err != nil {
		return nil, err
	}
	if tr.Mode != transportHTTP {
		return nil, errors.New(
			"agentsview mcp requires a daemon; refusing direct archive access",
		)
	}
	s.cfg.AuthToken = cfg.AuthToken
	return service.NewHTTPBackend(tr.URL, cfg.AuthToken, tr.ReadOnly), nil
}

func (s *mcpDaemonService) Get(
	ctx context.Context, id string,
) (*service.SessionDetail, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

func (s *mcpDaemonService) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.FindSessionIDsByPartial(ctx, partial, limit)
}

func (s *mcpDaemonService) List(
	ctx context.Context, f service.ListFilter,
) (*service.SessionList, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.List(ctx, f)
}

func (s *mcpDaemonService) Messages(
	ctx context.Context, id string, f service.MessageFilter,
) (*service.MessageList, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Messages(ctx, id, f)
}

func (s *mcpDaemonService) ToolCalls(
	ctx context.Context, id string,
) (*service.ToolCallList, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.ToolCalls(ctx, id)
}

func (s *mcpDaemonService) Sync(
	ctx context.Context, in service.SyncInput,
) (*service.SessionDetail, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Sync(ctx, in)
}

func (s *mcpDaemonService) Watch(
	ctx context.Context, id string,
) (<-chan service.Event, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Watch(ctx, id)
}

func (s *mcpDaemonService) Stats(
	ctx context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Stats(ctx, f)
}

func (s *mcpDaemonService) Search(
	ctx context.Context, req service.SearchRequest,
) (*service.SessionSearchResult, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.Search(ctx, req)
}

func (s *mcpDaemonService) SearchContent(
	ctx context.Context, req service.ContentSearchRequest,
) (*service.ContentSearchResult, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.SearchContent(ctx, req)
}

func (s *mcpDaemonService) UsageSummary(
	ctx context.Context, req service.UsageRequest,
) (*service.UsageSummaryResult, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.UsageSummary(ctx, req)
}

func (s *mcpDaemonService) UsagePairwiseComparison(
	ctx context.Context, req service.UsagePairwiseComparisonRequest,
) (*service.UsagePairwiseComparisonResponse, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.UsagePairwiseComparison(ctx, req)
}

func (s *mcpDaemonService) ListSecrets(
	ctx context.Context, f service.SecretListFilter,
) (*service.SecretFindingList, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.ListSecrets(ctx, f)
}

func (s *mcpDaemonService) ScanSecrets(
	ctx context.Context, in service.SecretScanInput,
	progress func(service.SecretScanProgress),
) (*service.SecretScanSummary, error) {
	svc, err := s.daemonService(ctx)
	if err != nil {
		return nil, err
	}
	return svc.ScanSecrets(ctx, in, progress)
}

// mcpListenerAuth decides the bearer token the MCP HTTP listener must
// enforce for the given (already-normalized) bind address. A loopback bind
// is local-trust and runs without listener auth (empty token) UNLESS
// require_auth is set, which forces authentication on every bind so a
// forwarded loopback port (reverse proxy, SSH tunnel) is never an
// unauthenticated surface. A non-loopback bind, or any bind under
// require_auth, must be authenticated: it returns the configured token, or
// an error when none is set, so the network-reachable surface is never
// unauthenticated.
func mcpListenerAuth(addr, configuredToken string, requireAuth bool) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parsing --http address %q: %w", addr, err)
	}
	if isLoopbackHost(host) && !requireAuth {
		return "", nil
	}
	if configuredToken == "" {
		return "", fmt.Errorf(
			"--http %q requires an auth token but none is configured; set "+
				"auth_token in config.toml (or enable require_auth) so the MCP "+
				"server enforces Authorization: Bearer, or bind a loopback "+
				"address without require_auth",
			addr)
	}
	return configuredToken, nil
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose the unauthenticated MCP server on a non-loopback
// interface unless the user has explicitly opted in.
//
// Forms accepted:
//   - "8085"            -> "127.0.0.1:8085" (loopback)
//   - ":8085"           -> "127.0.0.1:8085" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8085"  -> unchanged (loopback, allowed)
//   - "[::1]:8085"      -> unchanged (loopback, allowed)
//   - "192.168.1.5:8085", "0.0.0.0:8085", "host.local:8085" -> rejected
//     unless allowInsecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure bool) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", errors.New("--http requires an address")
	}

	// Bare port: "8085" or ":8085".
	if !strings.Contains(trimmed, ":") {
		if _, convErr := strconv.Atoi(trimmed); convErr == nil {
			return "127.0.0.1:" + trimmed, nil
		}
		return "", fmt.Errorf("--http %q: not a port and not host:port", trimmed)
	}
	if strings.HasPrefix(trimmed, ":") {
		return "127.0.0.1" + trimmed, nil
	}

	host, _, splitErr := net.SplitHostPort(trimmed)
	if splitErr != nil {
		return "", fmt.Errorf("--http %q: %w", trimmed, splitErr)
	}

	// isLoopbackHost (shared with managed_caddy.go) treats an empty host as
	// NOT loopback, which guards the "[]:8085" footgun where an empty host
	// passes net.SplitHostPort yet binds to all interfaces.
	if isLoopbackHost(host) {
		return trimmed, nil
	}
	if !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"--http-allow-insecure (the MCP server has no built-in "+
				"authentication; only opt in on trusted networks or behind "+
				"an authenticating reverse proxy)", trimmed)
	}
	return trimmed, nil
}
