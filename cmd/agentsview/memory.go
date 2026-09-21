// ABOUTME: Fast lifecycle entry points used by conversation-memory packages.
// ABOUTME: SessionStart dispatches bounded local or hosted owner work.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/servicehttp"
	"go.kenn.io/agentsview/internal/storage"
)

const (
	memorySessionStartTimeout = 1900 * time.Millisecond
	memoryRefreshDebounce     = 250 * time.Millisecond
)

type memorySessionStartMode string

const (
	memoryModeLocal             memorySessionStartMode = "local"
	memoryModeHostedContributor memorySessionStartMode = "hosted-contributor"
	memoryModeHostedReader      memorySessionStartMode = "hosted-reader"
)

type memorySessionStartRequest struct {
	Mode        memorySessionStartMode
	Target      string
	Server      string
	ServerToken string
	PG          bool
}

var (
	runMemorySessionStart             = executeMemorySessionStart
	requestMemoryLocalRefresh         = requestLocalMemoryRefresh
	requestMemoryContributorRefresh   = requestHostedContributorRefresh
	checkMemoryHostedReader           = checkHostedReaderAvailability
	probeMemoryHostedReaderHTTP       = probeHostedReaderHTTP
	probeMemoryHostedReaderPostgreSQL = probeHostedReaderPostgreSQL
	notifyMemoryReplicaWatch          = notifyReplicaWatchLifecycle
)

func newMemoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "memory",
		Short:        "Manage conversation-memory integration",
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMemorySessionStartCommand())
	return cmd
}

func newMemorySessionStartCommand() *cobra.Command {
	var mode string
	var target string
	var server string
	var serverTokenFile string
	var pg bool
	cmd := &cobra.Command{
		Use:   "session-start",
		Short: "Run the bounded memory lifecycle action for a session start",
		Long: "Queue a local refresh, wake a hosted contributor's existing " +
			"push owner, or check a hosted reader target. The command returns " +
			"within two seconds without waiting for archive reconciliation.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv("AGENTSVIEW_DISABLE_AUTO_SYNC") == "1" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"agentsview memory: automatic sync disabled by "+
						"AGENTSVIEW_DISABLE_AUTO_SYNC=1")
				return nil
			}
			req := memorySessionStartRequest{
				Mode:   memorySessionStartMode(strings.TrimSpace(mode)),
				Target: strings.TrimSpace(target),
				Server: strings.TrimSpace(server),
				PG:     pg,
			}
			if err := req.validate(strings.TrimSpace(serverTokenFile) != ""); err != nil {
				return err
			}
			if req.Server != "" {
				token, err := explicitServerToken(cmd)
				if err != nil {
					return err
				}
				req.ServerToken = token
			}
			ctx, cancel := context.WithTimeout(
				cmd.Context(), memorySessionStartTimeout,
			)
			defer cancel()
			return runMemorySessionStart(ctx, req)
		},
	}
	cmd.Flags().StringVar(&mode, "mode", string(memoryModeLocal),
		"Lifecycle role: local, hosted-contributor, or hosted-reader")
	cmd.Flags().StringVar(&target, "target", "",
		"Named PostgreSQL target for a hosted lifecycle role")
	cmd.Flags().StringVar(&server, "server", "",
		"Remote daemon URL for hosted-reader mode")
	cmd.Flags().StringVar(&serverTokenFile, "server-token-file", "",
		"File containing bearer token for --server")
	cmd.Flags().BoolVar(&pg, "pg", false,
		"Check the configured PostgreSQL target in hosted-reader mode")
	return cmd
}

func (r memorySessionStartRequest) validate(tokenFileSet bool) error {
	switch r.Mode {
	case memoryModeLocal:
		if r.Target != "" || r.Server != "" || tokenFileSet || r.PG {
			return errors.New(
				"memory session-start: local mode does not accept hosted target flags",
			)
		}
	case memoryModeHostedContributor:
		if r.Server != "" || tokenFileSet || r.PG {
			return errors.New(
				"memory session-start: hosted-contributor mode accepts only --target",
			)
		}
	case memoryModeHostedReader:
		if (r.Server != "") == r.PG {
			return errors.New(
				"memory session-start: hosted-reader mode requires exactly one of --server or --pg",
			)
		}
		if tokenFileSet && r.Server == "" {
			return errors.New(
				"memory session-start: --server-token-file requires --server",
			)
		}
		if r.Target != "" && !r.PG {
			return errors.New(
				"memory session-start: --target requires --pg in hosted-reader mode",
			)
		}
	default:
		return fmt.Errorf(
			"memory session-start: unknown --mode %q (want local, hosted-contributor, or hosted-reader)",
			r.Mode,
		)
	}
	return nil
}

func executeMemorySessionStart(
	ctx context.Context, req memorySessionStartRequest,
) error {
	switch req.Mode {
	case memoryModeLocal:
		return requestMemoryLocalRefresh(ctx)
	case memoryModeHostedContributor:
		return requestMemoryContributorRefresh(ctx, req.Target)
	case memoryModeHostedReader:
		return checkMemoryHostedReader(ctx, req)
	default:
		return fmt.Errorf("memory session-start: unsupported mode %q", req.Mode)
	}
}

func requestHostedContributorRefresh(
	ctx context.Context, targetName string,
) error {
	cfg, target, err := resolveMemoryPGTarget(targetName)
	if err != nil {
		return err
	}
	backend := pgReplica{}
	if err := backend.ValidateTarget(target.Target); err != nil {
		return fmt.Errorf("memory session-start: invalid PostgreSQL target: %w", err)
	}
	if err := notifyMemoryReplicaWatch(
		ctx, cfg.DataDir, backend.Name(), target.Name,
	); err != nil {
		return fmt.Errorf("memory session-start: hosted contributor owner: %w", err)
	}
	return nil
}

func checkHostedReaderAvailability(
	ctx context.Context, req memorySessionStartRequest,
) error {
	if req.Server != "" {
		if err := probeMemoryHostedReaderHTTP(
			ctx, req.Server, req.ServerToken,
		); err != nil {
			return fmt.Errorf("memory session-start: hosted reader endpoint: %w", err)
		}
		return nil
	}

	_, target, err := resolveMemoryPGTarget(req.Target)
	if err != nil {
		return err
	}
	if err := probeMemoryHostedReaderPostgreSQL(ctx, target.Target); err != nil {
		return fmt.Errorf("memory session-start: hosted reader PostgreSQL target: %w", err)
	}
	return nil
}

func resolveMemoryPGTarget(
	targetName string,
) (config.Config, storage.ConfiguredReplica, error) {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return config.Config{}, storage.ConfiguredReplica{},
			fmt.Errorf("memory session-start: loading config: %w", err)
	}
	backend := pgReplica{}
	refs, err := storage.SelectTargets(backend, cfg, targetName, false)
	if err != nil {
		return config.Config{}, storage.ConfiguredReplica{},
			fmt.Errorf("memory session-start: resolving PostgreSQL target: %w", err)
	}
	target, err := backend.ResolveTarget(cfg, refs[0])
	if err != nil {
		return config.Config{}, storage.ConfiguredReplica{},
			fmt.Errorf("memory session-start: resolving PostgreSQL target: %w", err)
	}
	if target.Target.URL == "" {
		return config.Config{}, storage.ConfiguredReplica{}, errors.New(
			"memory session-start: PostgreSQL target URL is not configured",
		)
	}
	return cfg, target, nil
}

func probeHostedReaderHTTP(ctx context.Context, server, token string) error {
	_, err := servicehttp.ProbeHTTPServerCapabilities(ctx, server, token)
	return err
}

func probeHostedReaderPostgreSQL(
	ctx context.Context, target storage.ReplicaTarget,
) error {
	database, err := postgres.OpenContext(
		ctx, target.URL, target.Schema, target.AllowInsecure,
	)
	if err != nil {
		return err
	}
	return database.Close()
}

func requestLocalMemoryRefresh(ctx context.Context) error {
	cfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("memory session-start: loading config: %w", err)
	}
	tr, err := ensureTransportContext(
		ctx, &cfg, transportIntentArchiveWrite, memorySessionStartTimeout,
	)
	if err != nil {
		return fmt.Errorf("memory session-start: local daemon: %w", err)
	}
	if tr.Mode != transportHTTP || tr.ReadOnly {
		return errors.New(
			"memory session-start: writable local daemon is unavailable",
		)
	}
	if tr.Runtime != nil && tr.Runtime.NoSync {
		return errors.New(
			"memory session-start: local daemon was started with sync disabled",
		)
	}
	return postMemoryRefresh(ctx, tr.URL, cfg.AuthToken)
}

func postMemoryRefresh(ctx context.Context, baseURL, token string) error {
	url := strings.TrimSuffix(baseURL, "/") + "/api/v1/memory/refresh"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, http.NoBody,
	)
	if err != nil {
		return fmt.Errorf("memory session-start: create refresh request: %w", err)
	}
	req.Header.Set("Origin", daemonOriginURL(baseURL))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: memorySessionStartTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("memory session-start: request refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf(
		"memory session-start: refresh request returned HTTP %d: %s",
		resp.StatusCode, strings.TrimSpace(string(body)),
	)
}

type memoryRefreshQueue struct {
	requests chan struct{}
}

func newMemoryRefreshQueue() *memoryRefreshQueue {
	return &memoryRefreshQueue{requests: make(chan struct{}, 1)}
}

func (q *memoryRefreshQueue) Notify() {
	select {
	case q.requests <- struct{}{}:
	default:
	}
}

func runMemoryRefreshScheduler(
	ctx context.Context,
	requests <-chan struct{},
	debounce time.Duration,
	refresh func(),
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-requests:
		}

		timer := time.NewTimer(debounce)
	debounceLoop:
		for {
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-requests:
				// The bounded queue already records the pending refresh. Keep
				// the original deadline so a busy burst cannot postpone forever.
			case <-timer.C:
				break debounceLoop
			}
		}

		// Requests queued before this pass starts are covered by it: drain the
		// single pending signal so parallel SessionStart hooks stay one pass.
		// A request that arrives while refresh runs stays queued and schedules
		// the next debounced pass, because the pass about to run cannot have
		// covered its data — draining after refresh would silently drop it
		// and leave new transcript data stale until a later polling interval.
		select {
		case <-requests:
		default:
		}

		refresh()
	}
}
