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
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/servicehttp"
	"go.kenn.io/agentsview/internal/skills"
	"go.kenn.io/agentsview/internal/storage"
)

const (
	memorySessionStartTimeout = 1900 * time.Millisecond
	memoryRefreshDebounce     = 250 * time.Millisecond

	// hookBinaryRecord is the file (under the default data directory,
	// always ~/.agentsview regardless of overrides) that records the
	// absolute path of the agentsview binary, so the plugin's SessionStart
	// hook and MCP launcher can invoke a trusted binary instead of
	// resolving PATH inside an untrusted repository.
	hookBinaryRecord = "registered-bin"
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
	var hook bool
	var pluginRoot string
	cmd := &cobra.Command{
		Use:   "session-start",
		Short: "Run the bounded memory lifecycle action for a session start",
		Long: "Queue a local refresh, wake a hosted contributor's existing " +
			"push owner, or check a hosted reader target. The command returns " +
			"within two seconds without waiting for archive reconciliation.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := runMemorySessionStartCommand(cmd, pluginRoot)
			if hook && err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "agentsview memory hook: %v\n", err)
				return nil
			}
			return err
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
	cmd.Flags().BoolVar(&hook, "hook", false,
		"Report failures without preventing the agent session from starting")
	cmd.Flags().StringVar(&pluginRoot, "plugin-root", "",
		"Native plugin root used to diagnose duplicate standalone skills")
	return cmd
}

func runMemorySessionStartCommand(
	cmd *cobra.Command, pluginRoot string,
) error {
	// Register the absolute path of this executable so the plugin's
	// SessionStart hook can invoke a trusted binary instead of resolving
	// PATH inside an untrusted repository. Deliberate invocation is what
	// registers: the hook itself only ever reaches this binary through the
	// registered path or a trusted (repository-free) context. Registration
	// always targets the default data directory — the exact path the hook
	// reads — because a data-directory override can be set by the same
	// repository the hook must not trust.
	if exe, exeErr := os.Executable(); exeErr == nil {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			dir := filepath.Join(home, ".agentsview")
			if mkErr := os.MkdirAll(dir, 0o700); mkErr == nil {
				if recordErr := os.WriteFile(
					filepath.Join(dir, hookBinaryRecord),
					[]byte(exe), 0o600,
				); recordErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"agentsview memory: could not record hook binary path: %v\n",
						recordErr)
				}
			}
		}
	}
	serverFromEnv, err := applyMemorySessionEnv(cmd)
	if err != nil {
		return err
	}
	mode, _ := cmd.Flags().GetString("mode")
	target, _ := cmd.Flags().GetString("target")
	server, _ := cmd.Flags().GetString("server")
	serverTokenFile, _ := cmd.Flags().GetString("server-token-file")
	pg, _ := cmd.Flags().GetBool("pg")

	if strings.TrimSpace(pluginRoot) != "" {
		reportMemoryPluginConflicts(cmd.ErrOrStderr(), pluginRoot)
	}
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
		token, err := resolveSessionServerToken(cmd, serverFromEnv)
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
}

func applyMemorySessionEnv(cmd *cobra.Command) (bool, error) {
	serverFromEnv := false
	for _, name := range []string{"mode", "target", "server", "server-token-file", "pg"} {
		if cmd.Flags().Changed(name) {
			return false, nil
		}
	}
	for _, item := range []struct{ flag, env string }{
		{"mode", "AGENTSVIEW_MEMORY_MODE"},
		{"target", "AGENTSVIEW_MEMORY_TARGET"},
	} {
		if value := strings.TrimSpace(os.Getenv(item.env)); value != "" {
			if err := cmd.Flags().Set(item.flag, value); err != nil {
				return false, fmt.Errorf("memory session-start: invalid %s: %w", item.env, err)
			}
		}
	}
	mode, err := cmd.Flags().GetString("mode")
	if err != nil {
		return false, err
	}
	// Hosted contributors can use AGENTSVIEW_MEMORY_PG for the MCP read
	// target while their hook still wakes the existing push owner. Only a
	// hosted-reader lifecycle probe consumes read-target environment values.
	if memorySessionStartMode(strings.TrimSpace(mode)) != memoryModeHostedReader {
		return false, nil
	}
	for _, item := range []struct{ flag, env string }{
		{"server", "AGENTSVIEW_MEMORY_SERVER"},
		{"server-token-file", "AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE"},
		{"pg", "AGENTSVIEW_MEMORY_PG"},
	} {
		if value := strings.TrimSpace(os.Getenv(item.env)); value != "" {
			if err := cmd.Flags().Set(item.flag, value); err != nil {
				return false, fmt.Errorf("memory session-start: invalid %s: %w", item.env, err)
			}
			if item.flag == "server" {
				serverFromEnv = true
			}
		}
	}
	return serverFromEnv, nil
}

// resolveSessionServerToken resolves the bearer token for a hosted-reader
// availability probe. An explicitly flagged server accepts the standard
// token chain (--server-token-file, then the global AGENTSVIEW_SERVER_TOKEN).
// An environment-selected server must receive no credential: project
// settings can inject the endpoint, and both the global token fallback and
// an environment-selected token file would disclose archive credentials to
// whoever injected it. The probe runs unauthenticated and a diagnostic
// says so, so the user can switch to explicit flags.
func resolveSessionServerToken(
	cmd *cobra.Command, serverFromEnv bool,
) (string, error) {
	if !serverFromEnv {
		return explicitServerToken(cmd)
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"agentsview memory: environment-selected server runs unauthenticated; "+
			"pass --server and --server-token-file explicitly to authenticate")
	return "", nil
}

func reportMemoryPluginConflicts(out io.Writer, pluginRoot string) {
	if _, err := os.Stat(filepath.Join(pluginRoot, "skills",
		"agentsview-finding-history", "SKILL.md")); err != nil {
		fmt.Fprintf(out, "agentsview memory: plugin skill unavailable: %v\n", err)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(out, "agentsview memory: cannot check standalone skills: %v\n", err)
		return
	}
	for _, harness := range skills.AllHarnesses() {
		pkg, renderErr := skills.RenderPackage(harness, version, skills.Remote{})
		if renderErr != nil || len(pkg) == 0 {
			continue
		}
		path := filepath.Join(home, pkg[0].RelativePath)
		existing, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			fmt.Fprintf(out, "agentsview memory: cannot inspect %s: %v\n", path, readErr)
			continue
		}
		state := skills.Classify(existing, pkg[0])
		switch state {
		case skills.StateMissing:
			continue
		case skills.StateCurrent, skills.StateStale:
			fmt.Fprintf(out, "agentsview memory: native plugin duplicates managed standalone skill %s; remove the standalone copy\n", path)
		case skills.StateModified, skills.StateForeign:
			fmt.Fprintf(out, "agentsview memory: native plugin conflicts with user-managed skill %s; preserved it for manual resolution\n", path)
		}
	}
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

		refresh()
		// A request arriving while refresh ran may describe work this pass
		// already scanned -- or work that landed after its source was
		// processed, which the pass did not cover. The signal carries no
		// boundary information, so leave it buffered and let the loop run
		// another debounced pass instead of assuming the last one was
		// sufficient; a pass over an unchanged archive is cheap and the
		// debounce still coalesces parallel SessionStart hooks.
	}
}
