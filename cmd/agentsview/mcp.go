// ABOUTME: `agentsview mcp` subcommand — serves the read-only MCP tools
// ABOUTME: over the SessionService seam (stdio by default, or HTTP).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
	"go.kenn.io/kit/daemon"
)

func newMCPCommand() *cobra.Command {
	var httpAddr string
	var httpAllowInsecure bool
	var profileName string

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server exposing read-only session retrieval tools",
		Long: `Start an MCP (Model Context Protocol) server over stdio (default) or
StreamableHTTP, exposing read-only tools for searching and reading
recorded agent sessions: search_sessions, list_sessions,
get_session_overview, get_messages, get_memory_status, search_content,
get_usage_summary, get_tool_calls, get_recent_edits, get_child_sessions,
compare_usage, get_activity_report, get_tool_usage, get_quality_signals, and
search_docs, plus query_recall for distilled session knowledge and
get_friction_digest/list_friction_patterns for the Friction Log.
Use --profile memory to advertise only get_memory_status, search_content, and
get_messages for focused conversation-memory clients. The default full profile
is unchanged.

The server talks to the local agentsview daemon, starting it when needed, and
reuses the connection until the daemon restarts or a call fails. A failed call
is not retried. Use --server to target an explicit daemon URL.

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
			if err := applyMemoryTargetEnv(cmd, profileName); err != nil {
				return err
			}
			profile, err := mcpserver.ParseProfile(profileName)
			if err != nil {
				return err
			}
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

			opts := mcpserver.ServeOptions{
				Service: svc,
				Version: version,
				Profile: profile,
			}

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
				cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
				if err != nil {
					return fmt.Errorf("loading config: %w", err)
				}
				if err := cfg.ApplyDeploymentAuth(); err != nil {
					return fmt.Errorf("loading listener auth: %w", err)
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
				opts.DiscoveryDirectory = filepath.Join(cfg.DataDir, "mcp")
				opts.BackendURL, _ = cmd.Flags().GetString("server")
				if opts.BackendURL == "" && !pgReadRequested(cmd) {
					if runtime := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); runtime != nil {
						opts.BackendURL = urlFromDaemonRuntime(runtime)
					}
				}
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
	cmd.Flags().StringVar(&profileName, "profile", string(mcpserver.ProfileFull),
		"Tool profile to advertise (full or memory)")

	cmd.AddCommand(newMCPStatusCommand())
	return cmd
}

func applyMemoryTargetEnv(cmd *cobra.Command, profileName string) error {
	if strings.TrimSpace(profileName) != string(mcpserver.ProfileMemory) {
		return nil
	}
	explicit := false
	for _, name := range []string{"server", "server-token-file", "pg"} {
		if cmd.Flags().Changed(name) {
			explicit = true
			break
		}
	}
	if !explicit {
		for _, item := range []struct{ flag, env string }{
			{"server", "AGENTSVIEW_MEMORY_SERVER"},
			{"server-token-file", "AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE"},
			{"pg", "AGENTSVIEW_MEMORY_PG"},
		} {
			if value := strings.TrimSpace(os.Getenv(item.env)); value != "" {
				if err := cmd.Flags().Set(item.flag, value); err != nil {
					return fmt.Errorf("mcp: invalid %s: %w", item.env, err)
				}
			}
		}
	}
	server, err := cmd.Flags().GetString("server")
	if err != nil {
		return err
	}
	tokenFile, err := cmd.Flags().GetString("server-token-file")
	if err != nil {
		return err
	}
	if strings.TrimSpace(tokenFile) != "" && strings.TrimSpace(server) == "" {
		return errors.New(
			"mcp: --server-token-file or AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE requires --server or AGENTSVIEW_MEMORY_SERVER",
		)
	}
	return nil
}

// resolveMCPService constructs the SessionService used by the long-lived
// MCP server. The implicit local path is intentionally daemon-only and
// lazy: calls reuse the daemon transport while its record and identity match.
// The next call can wake the daemon after it exits due to idleness.
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
		capabilities, err := servicehttp.ProbeHTTPServerCapabilities(
			cmd.Context(), remote, token,
		)
		if err != nil {
			return nil, nil, err
		}
		return servicehttp.NewHTTPBackendForServer(remote, token, capabilities),
			func() {}, nil
	}
	cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeVector)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
	if err != nil {
		return nil, nil, err
	}
	if usePG {
		if err := cfg.ApplyDeploymentEmbeddings(); err != nil {
			return nil, nil, fmt.Errorf("loading deployment embeddings: %w", err)
		}
		return newPGReadService(cfg, pgCfg)
	}
	if err := cfg.ApplyDeploymentAuth(); err != nil {
		return nil, nil, fmt.Errorf("loading daemon auth: %w", err)
	}
	return newMCPDaemonService(cfg), func() {}, nil
}

type mcpDaemonService struct {
	mu       sync.Mutex // serializes daemon discovery and guards cfg
	cfg      config.Config
	selected atomic.Pointer[mcpDaemonSelection]
}

// mcpDaemonSelection is a resolved daemon backend. runtime is the record that
// proved the daemon's identity when the selection was cached.
type mcpDaemonSelection struct {
	backend service.SessionService
	runtime daemon.RuntimeRecord
}

func newMCPDaemonService(cfg config.Config) service.SessionService {
	return &mcpDaemonService{cfg: cfg}
}

func (s *mcpDaemonService) SupportsRecallQueries() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	runtime := FindDaemonRuntime(s.cfg.DataDir, s.cfg.AuthToken)
	return runtime == nil || !runtime.ReadOnly
}

// SupportsFriction asks a running read-only daemon, which may be a DuckDB or
// ClickHouse mirror without friction tables. A writable archive daemon, or
// the one started on demand, serves the friction reads.
func (s *mcpDaemonService) SupportsFriction() bool {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	runtime := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken)
	if runtime == nil || !runtime.ReadOnly {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capabilities, err := servicehttp.ProbeHTTPServerCapabilities(ctx, urlFromDaemonRuntime(runtime), cfg.AuthToken)
	return err == nil && capabilities.FrictionReadable
}

func (s *mcpDaemonService) FrictionDigest(
	ctx context.Context, date string,
) (*service.FrictionDigestView, error) {
	return mcpFrictionCall(ctx, s, func(fs service.FrictionService) (*service.FrictionDigestView, error) {
		return fs.FrictionDigest(ctx, date)
	})
}

func (s *mcpDaemonService) FrictionDigests(
	ctx context.Context, from, to string,
) ([]service.FrictionDigestListItem, error) {
	return mcpFrictionCall(ctx, s, func(fs service.FrictionService) ([]service.FrictionDigestListItem, error) {
		return fs.FrictionDigests(ctx, from, to)
	})
}

func (s *mcpDaemonService) FrictionFindings(
	ctx context.Context, f service.FrictionFindingFilter,
) (*service.FrictionFindingPage, error) {
	return mcpFrictionCall(ctx, s, func(fs service.FrictionService) (*service.FrictionFindingPage, error) {
		return fs.FrictionFindings(ctx, f)
	})
}

func (s *mcpDaemonService) FrictionPatterns(
	ctx context.Context, f service.FrictionPatternFilter,
) (*service.FrictionPatternPage, error) {
	return mcpFrictionCall(ctx, s, func(fs service.FrictionService) (*service.FrictionPatternPage, error) {
		return fs.FrictionPatterns(ctx, f)
	})
}

// mcpFrictionCall runs a Friction Log read on the selected daemon.
func mcpFrictionCall[T any](
	ctx context.Context, s *mcpDaemonService,
	call func(service.FrictionService) (T, error),
) (T, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (T, error) {
		fs, ok := svc.(service.FrictionService)
		if !ok {
			var zero T
			return zero, errors.New("daemon backend does not serve the Friction Log")
		}
		return call(fs)
	})
}

func (s *mcpDaemonService) daemonSelection(
	ctx context.Context,
) (*mcpDaemonSelection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sel := s.selected.Load(); sel != nil && sel.daemonUnchanged(s.cfg.DataDir) {
		return sel, nil
	}
	s.selected.Store(nil)
	cfg := s.cfg
	tr, err := ensureTransportContext(
		ctx, &cfg, transportIntentLongLived, 0,
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
	sel := &mcpDaemonSelection{
		backend: servicehttp.NewHTTPBackend(tr.URL, cfg.AuthToken, tr.ReadOnly, tr.BrowserURL),
	}
	if rec, ok := verifiedWritableDaemonRecord(tr); ok {
		sel.runtime = rec
		s.selected.Store(sel)
	}
	return sel, nil
}

// verifiedWritableDaemonRecord returns the runtime record of a writable daemon
// whose process identity matches. Only such a daemon can be checked cheaply on
// later calls, so read-only daemons, startup fallbacks, and unverifiable
// processes get full discovery on every call.
func verifiedWritableDaemonRecord(tr transport) (daemon.RuntimeRecord, bool) {
	if tr.Runtime == nil || tr.ReadOnly || tr.Runtime.RuntimeFallback {
		return daemon.RuntimeRecord{}, false
	}
	rec := tr.Runtime.Record
	return rec, runtimeRecordIdentityState(rec) == processCreateTimeMatch
}

// daemonUnchanged requires unchanged evidence before reusing cached credentials.
func (sel *mcpDaemonSelection) daemonUnchanged(dataDir string) bool {
	store := runtimeStore(dataDir)
	rec, err := store.Read(sel.runtime.SourcePath)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(rec, sel.runtime) &&
		runtimeRecordIdentityState(rec) == processCreateTimeMatch
}

// mcpDaemonCall runs one operation on the selected daemon. A failure clears
// the selection so the next call rediscovers the daemon, unless the failure
// shows the daemon is still serving: the caller gave up, or the daemon
// rejected the request with a 4xx answer.
func mcpDaemonCall[T any](
	ctx context.Context,
	s *mcpDaemonService,
	call func(service.SessionService) (T, error),
) (T, error) {
	sel, err := s.daemonSelection(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	result, err := call(sel.backend)
	if err != nil && ctx.Err() == nil && !servicehttp.IsRequestRejected(err) {
		// Compare-and-swap keeps a replacement that another call selected
		// while this one was failing, and never waits for that discovery.
		s.selected.CompareAndSwap(sel, nil)
	}
	return result, err
}

func (s *mcpDaemonService) Get(
	ctx context.Context, id string,
) (*service.SessionDetail, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionDetail, error) {
		return svc.Get(ctx, id)
	})
}

func (s *mcpDaemonService) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) ([]string, error) {
		return svc.FindSessionIDsByPartial(ctx, partial, limit)
	})
}

func (s *mcpDaemonService) FindSessionIDsByRawSuffix(
	ctx context.Context, raw string, limit int,
) ([]string, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) ([]string, error) {
		return svc.FindSessionIDsByRawSuffix(ctx, raw, limit)
	})
}

func (s *mcpDaemonService) List(
	ctx context.Context, f service.ListFilter,
) (*service.SessionList, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionList, error) {
		return svc.List(ctx, f)
	})
}

func (s *mcpDaemonService) Messages(
	ctx context.Context, id string, f service.MessageFilter,
) (*service.MessageList, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.MessageList, error) {
		return svc.Messages(ctx, id, f)
	})
}

func (s *mcpDaemonService) ToolCalls(
	ctx context.Context, id string,
) (*service.ToolCallList, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.ToolCallList, error) {
		return svc.ToolCalls(ctx, id)
	})
}

func (s *mcpDaemonService) ChildSessions(
	ctx context.Context, id string,
) ([]db.Session, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) ([]db.Session, error) {
		return svc.ChildSessions(ctx, id)
	})
}

func (s *mcpDaemonService) RecentEdits(
	ctx context.Context, f service.RecentEditsFilter,
) (*db.RecentEditsResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.RecentEditsResult, error) {
		return svc.RecentEdits(ctx, f)
	})
}

func (s *mcpDaemonService) ActivityReport(
	ctx context.Context, req service.ActivityReportRequest,
) (*activity.Report, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*activity.Report, error) {
		return svc.ActivityReport(ctx, req)
	})
}

func (s *mcpDaemonService) ToolAnalytics(
	ctx context.Context, req service.AnalyticsRequest,
) (*db.ToolsAnalyticsResponse, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.ToolsAnalyticsResponse, error) {
		return svc.ToolAnalytics(ctx, req)
	})
}

func (s *mcpDaemonService) SignalAnalytics(
	ctx context.Context, req service.AnalyticsRequest,
) (*db.SignalsAnalyticsResponse, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.SignalsAnalyticsResponse, error) {
		return svc.SignalAnalytics(ctx, req)
	})
}

func (s *mcpDaemonService) SignalSessions(
	ctx context.Context, req service.SignalSessionsRequest,
) (*db.SignalSessionsResponse, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.SignalSessionsResponse, error) {
		return svc.SignalSessions(ctx, req)
	})
}

func (s *mcpDaemonService) Sync(
	ctx context.Context, in service.SyncInput,
) (*service.SessionDetail, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionDetail, error) {
		return svc.Sync(ctx, in)
	})
}

func (s *mcpDaemonService) Watch(
	ctx context.Context, id string,
) (<-chan service.Event, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (<-chan service.Event, error) {
		return svc.Watch(ctx, id)
	})
}

func (s *mcpDaemonService) Stats(
	ctx context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionStats, error) {
		return svc.Stats(ctx, f)
	})
}

func (s *mcpDaemonService) Search(
	ctx context.Context, req service.SearchRequest,
) (*service.SessionSearchResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionSearchResult, error) {
		return svc.Search(ctx, req)
	})
}

func (s *mcpDaemonService) SearchContent(
	ctx context.Context, req service.ContentSearchRequest,
) (*service.ContentSearchResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.ContentSearchResult, error) {
		return svc.SearchContent(ctx, req)
	})
}

func (s *mcpDaemonService) MemoryStatus(
	ctx context.Context,
) (service.MemoryStatus, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (service.MemoryStatus, error) {
		return service.GetMemoryStatus(ctx, svc)
	})
}

func (s *mcpDaemonService) UsageSummary(
	ctx context.Context, req service.UsageRequest,
) (*service.UsageSummaryResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.UsageSummaryResult, error) {
		return svc.UsageSummary(ctx, req)
	})
}

func (s *mcpDaemonService) SessionUsage(
	ctx context.Context, req service.SessionUsageRequest,
) (*service.SessionUsageReport, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SessionUsageReport, error) {
		return svc.SessionUsage(ctx, req)
	})
}

func (s *mcpDaemonService) UsagePairwiseComparison(
	ctx context.Context, req service.UsagePairwiseComparisonRequest,
) (*service.UsagePairwiseComparisonResponse, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.UsagePairwiseComparisonResponse, error) {
		return svc.UsagePairwiseComparison(ctx, req)
	})
}

func (s *mcpDaemonService) ListRecallEntries(
	ctx context.Context, f service.RecallFilter,
) (*service.RecallList, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.RecallList, error) {
		return svc.ListRecallEntries(ctx, f)
	})
}

func (s *mcpDaemonService) GetRecallEntry(
	ctx context.Context, id string,
) (*db.RecallEntry, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.RecallEntry, error) {
		return svc.GetRecallEntry(ctx, id)
	})
}

func (s *mcpDaemonService) QueryRecallEntries(
	ctx context.Context, req service.RecallQuery,
) (*service.RecallQueryResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.RecallQueryResult, error) {
		return svc.QueryRecallEntries(ctx, req)
	})
}

func (s *mcpDaemonService) ImportRecallEntries(
	ctx context.Context, r io.Reader, opts db.RecallImportOptions,
) (*db.RecallImportResult, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*db.RecallImportResult, error) {
		return svc.ImportRecallEntries(ctx, r, opts)
	})
}

func (s *mcpDaemonService) ListSecrets(
	ctx context.Context, f service.SecretListFilter,
) (*service.SecretFindingList, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SecretFindingList, error) {
		return svc.ListSecrets(ctx, f)
	})
}

func (s *mcpDaemonService) ScanSecrets(
	ctx context.Context, in service.SecretScanInput,
	progress func(service.SecretScanProgress),
) (*service.SecretScanSummary, error) {
	return mcpDaemonCall(ctx, s, func(svc service.SessionService) (*service.SecretScanSummary, error) {
		return svc.ScanSecrets(ctx, in, progress)
	})
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
