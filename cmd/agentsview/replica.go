package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// ReplicaPushConfig carries the `<backend> push` flags plus the watch loop's
// internal per-push scope.
type ReplicaPushConfig struct {
	Full            bool
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
	NoVectors       bool
	// ScopeVectorsToChangedSessions is set internally by the watch
	// loop for change-triggered pushes; it has no CLI flag.
	ScopeVectorsToChangedSessions bool
	// LastReconciledVectorGeneration is set internally by the watch
	// loop so a scoped push can promote to generation-wide when the
	// active generation id has changed; it has no CLI flag.
	LastReconciledVectorGeneration int64
	// WatchBatch and WatchRecovery are internal watch-loop scope. Explicit
	// pushes leave them nil and retain the historical unscoped sync.
	WatchBatch    *syncpkg.WatchBatch
	WatchRecovery *syncpkg.WatchRecoveryScope
}

type ReplicaStatusConfig struct {
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
}

// replicaServeExtras is implemented by a replica registered in
// replicaBackends that contributes extra HTTP capabilities when it serves,
// such as PostgreSQL's raw-upload ingestion and vector search. A replica
// without extras serves the plain db.Store surface.
type replicaServeExtras interface {
	serveOptions(
		ctx context.Context, appCfg config.Config,
		target storage.ReplicaTarget, store storage.ReplicaStore,
	) ([]server.Option, func() error, error)
}

// replicaVectorPushSource returns the vectors.db push source to attach for
// this target, or nil when the vector push phase is gated off: the target
// opts out via push_vectors=false, the caller passed --no-vectors, or
// [vector] is disabled (newVectorPushSource itself returns nil then). A nil
// source leaves the pusher's vector phase skipped.
func replicaVectorPushSource(
	appCfg config.Config, target storage.ConfiguredReplica, cfg ReplicaPushConfig,
) storage.VectorPushSource {
	if !target.Target.PushVectors || cfg.NoVectors {
		return nil
	}
	return newVectorPushSource(appCfg)
}

func runReplicaPush(
	backend storage.Replica, cfg ReplicaPushConfig, targetName string,
) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := storage.SelectTargets(
		backend, appCfg, targetName, cfg.AllTargets,
	)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	writer, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		return fmt.Errorf("opening writer: %w", err)
	}
	defer cleanup()

	var failures []string
	for i, target := range targets {
		if len(targets) > 1 || target.Name != "" {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Target: %s\n", target.Label())
		}
		if err := runReplicaPushTarget(
			ctx, backend, writer, appCfg, cfg, target,
		); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(
				failures,
				fmt.Sprintf("%s: %v", target.Label(), err),
			)
			fmt.Fprintf(
				os.Stderr,
				"warning: %s push target %s failed: %v\n",
				backend.Name(), target.Label(), err,
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"%d %s target(s) failed: %s",
			len(failures), backend.Name(),
			strings.Join(failures, "; "),
		)
	}
	return nil
}

func runReplicaPushTarget(
	ctx context.Context,
	backend storage.Replica,
	writer archiveWriteBackend,
	appCfg config.Config,
	cfg ReplicaPushConfig,
	ref storage.ReplicaTargetRef,
) error {
	target, err := backend.ResolveTarget(appCfg, ref)
	if err != nil {
		return err
	}
	if target.Target.URL == "" {
		return errors.New("url not configured")
	}
	if err := backend.ValidateTarget(target.Target); err != nil {
		return err
	}

	projects, excludeProjects, err := resolvePushProjects(target, cfg)
	if err != nil {
		return err
	}

	result, err := writer.ReplicaPush(
		ctx, backend, target, cfg, projects, excludeProjects,
	)
	if err != nil {
		return err
	}
	writeReplicaPushSummary(os.Stdout, backend.DisplayName(), result)
	if result.Errors > 0 {
		return fmt.Errorf("%d session(s) failed", result.Errors)
	}
	return nil
}

// replicaPushProgressStage buckets progress reports into the display stages
// that each own one progress line: daemon-side setup, fingerprinting, the
// session push, and the vector push.
func replicaPushProgressStage(p storage.PushProgress) string {
	switch {
	case p.Phase == "preparing" && p.SessionsTotal == 0:
		return "setup"
	case p.Phase == "preparing":
		return "fingerprints"
	case p.Phase == "vectors":
		return "vectors"
	default:
		return "sessions"
	}
}

// newReplicaPushProgressPrinter returns a progress renderer that updates the
// current stage's line in place and finishes it with a newline when the push
// moves to the next stage, so each completed stage stays in the scrollback
// instead of being overwritten by the next one. Every render carries the
// elapsed time since the printer was created (push start), so long stages
// show how long the push has been running. Renders end with an
// erase-to-end-of-line so a shorter render fully replaces a longer one
// instead of leaving its tail behind.
func newReplicaPushProgressPrinter() func(storage.PushProgress) {
	lastStage := ""
	start := time.Now()
	return func(p storage.PushProgress) {
		stage := replicaPushProgressStage(p)
		if lastStage != "" && stage != lastStage {
			fmt.Println()
		}
		lastStage = stage
		fmt.Printf("\r%s (%s elapsed)\x1b[K",
			replicaPushProgressLine(p), time.Since(start).Round(time.Second))
	}
}

// replicaPushProgressLine renders one progress report as the visible line
// text for its stage; newReplicaPushProgressPrinter owns the in-place
// terminal handling.
func replicaPushProgressLine(p storage.PushProgress) string {
	if p.Phase == "preparing" {
		if p.SessionsTotal == 0 {
			return "Preparing push (sync state, metadata, fingerprints)..."
		}
		return fmt.Sprintf(
			"Preparing... %d/%d sessions fingerprinted",
			p.SessionsDone, p.SessionsTotal,
		)
	}
	if p.Phase == "vectors" {
		return fmt.Sprintf(
			"Pushing vectors... %d/%d sessions scanned, %d chunks",
			p.VectorSessionsDone, p.VectorSessionsTotal,
			p.VectorChunksPushed,
		)
	}
	if p.SkippedConflicts > 0 {
		return fmt.Sprintf(
			"Pushing... %d/%d sessions, %d messages, %d ownership conflicts skipped",
			p.SessionsDone, p.SessionsTotal,
			p.MessagesDone, p.SkippedConflicts,
		)
	}
	return fmt.Sprintf(
		"Pushing... %d/%d sessions, %d messages",
		p.SessionsDone, p.SessionsTotal,
		p.MessagesDone,
	)
}

func writeReplicaPushSummary(
	w io.Writer, displayName string, result storage.PushResult,
) {
	dur := result.Duration.Round(time.Millisecond)
	errSuffix := ""
	if result.Errors > 0 {
		errSuffix = fmt.Sprintf(", %d error(s)", result.Errors)
	}
	if result.SkippedConflicts > 0 {
		fmt.Fprintf(
			w,
			"Pushed %d sessions, %d messages, skipped %d ownership conflict(s)%s in %s\n",
			result.SessionsPushed,
			result.MessagesPushed,
			result.SkippedConflicts,
			errSuffix,
			dur,
		)
		fmt.Fprintf(
			w,
			"Warning: skipped %d session(s) owned by another %s push marker\n",
			result.SkippedConflicts, displayName,
		)
		writeReplicaVectorPushSummary(w, displayName, result.Vectors)
		return
	}
	fmt.Fprintf(
		w,
		"Pushed %d sessions, %d messages%s in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		errSuffix,
		dur,
	)
	if result.SkippedUnchanged > 0 {
		fmt.Fprintf(w, "Skipped %d unchanged session(s)\n", result.SkippedUnchanged)
	}
	if result.DeletedStale > 0 {
		fmt.Fprintf(w, "Removed %d stale session(s)\n", result.DeletedStale)
	}
	writeReplicaVectorPushSummary(w, displayName, result.Vectors)
}

// writeReplicaVectorPushSummary prints the vector push phase outcome: a skip
// reason when the phase did not run, otherwise per-session and per-doc
// counters. An ownership conflict count is surfaced separately, analogous to
// the session-level SkippedConflicts warning, since those sessions were left
// untouched on the replica because another machine's marker owns them.
func writeReplicaVectorPushSummary(
	w io.Writer, displayName string, v storage.VectorPushResult,
) {
	if v.Skipped {
		if v.SkippedReason != "" {
			fmt.Fprintf(w, "Vectors: skipped (%s)\n", v.SkippedReason)
		}
		return
	}
	fmt.Fprintf(
		w,
		"Vectors: %d session(s) pushed, %d unchanged, %d docs, %d chunks\n",
		v.SessionsPushed, v.SessionsUnchanged, v.DocsPushed, v.ChunksPushed,
	)
	if v.Conflicts > 0 {
		fmt.Fprintf(
			w,
			"Warning: skipped %d vector session(s) owned by another %s push marker\n",
			v.Conflicts, displayName,
		)
	}
	if v.SessionsDeferred > 0 {
		fmt.Fprintf(
			w,
			"Warning: deferred vectors for %d session(s); the next generation-wide reconciliation sends them\n",
			v.SessionsDeferred,
		)
	}
}

func runReplicaStatus(
	ctx context.Context, backend storage.Replica, targetName string,
	cfg ReplicaStatusConfig,
) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := storage.SelectTargets(
		backend, appCfg, targetName, cfg.AllTargets,
	)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	database, err := openReadOnlyDB(ctx, appCfg)
	if err != nil {
		log.Printf(
			"warning: reading local %s status watermark: %v",
			backend.Name(), err,
		)
		database = nil
	}
	if database != nil {
		defer database.Close()
	}

	var failures []string
	for i, target := range targets {
		if len(targets) > 1 || target.Name != "" {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Target: %s\n", target.Label())
		}
		if err := runReplicaStatusTarget(
			backend, database, appCfg, target, cfg,
		); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(
				failures,
				fmt.Sprintf("%s: %v", target.Label(), err),
			)
			fmt.Fprintf(
				os.Stderr,
				"warning: %s status target %s failed: %v\n",
				backend.Name(), target.Label(), err,
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"%d %s target(s) failed: %s",
			len(failures), backend.Name(),
			strings.Join(failures, "; "),
		)
	}
	return nil
}

func runReplicaStatusTarget(
	backend storage.Replica,
	database *db.DB,
	appCfg config.Config,
	ref storage.ReplicaTargetRef,
	cfg ReplicaStatusConfig,
) error {
	target, err := backend.ResolveTarget(appCfg, ref)
	if err != nil {
		return err
	}
	if target.Target.URL == "" {
		return errors.New("url not configured")
	}
	projects, excludeProjects, err := resolvePushProjects(
		target,
		ReplicaPushConfig{
			ProjectsFlag:    cfg.ProjectsFlag,
			ExcludeProjects: cfg.ExcludeProjects,
			AllProjects:     cfg.AllProjects,
		},
	)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	status, err := backend.Status(
		ctx, database, target, projects, excludeProjects,
	)
	if err != nil {
		return err
	}
	writeReplicaStatus(os.Stdout, status)
	return nil
}

// writeReplicaStatus prints one label/value row per line with the values
// aligned one column past the widest label.
func writeReplicaStatus(w io.Writer, status storage.ReplicaStatus) {
	width := 0
	for _, row := range status.Rows {
		width = max(width, len(row.Label))
	}
	for _, row := range status.Rows {
		fmt.Fprintf(w, "%-*s %s\n", width, row.Label, row.Value)
	}
}

func loadReplicaServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadRemoteServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

type replicaServeStartup struct {
	cfg     config.Config
	ctx     context.Context
	rtOpts  serveRuntimeOptions
	srv     *server.Server
	cleanup func()
}

var prepareReplicaServe = prepareReplicaServeImpl

func prepareReplicaServeImpl(
	backend storage.Replica, appCfg config.Config, basePath string,
) (replicaServeStartup, error) {
	name := backend.Name()
	if err := validateServeConfig(appCfg); err != nil {
		return replicaServeStartup{}, fmt.Errorf("invalid serve config: %w", err)
	}

	target, err := storage.DefaultTarget(backend, appCfg)
	if err != nil {
		return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
	}
	if target.Target.URL == "" {
		return replicaServeStartup{}, fmt.Errorf("%s serve: url not configured", name)
	}

	applyClassifierConfig(appCfg)
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt, syscall.SIGTERM,
	)
	store, err := backend.OpenServeStore(ctx, target.Target)
	if err != nil {
		stop()
		return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
	}
	cleanupStore := func() { _ = store.Close() }
	if err := applyRequiredCursorSecret(store, appCfg); err != nil {
		stop()
		cleanupStore()
		return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
	}
	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}

	var closeExtras func() error
	cleanup := func() {
		stop()
		if closeExtras != nil {
			if err := closeExtras(); err != nil {
				log.Printf("warning: closing %s serve extras: %v", name, err)
			}
		}
		cleanupStore()
	}
	if err := wireReplicaVectorSearch(ctx, appCfg, backend, store, name+" serve"); err != nil {
		cleanup()
		return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          name + "-serve",
		BasePath:      basePath,
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(ctx, appCfg, rtOpts)
	if err != nil {
		cleanup()
		return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
	}

	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:                    version,
			Commit:                     commit,
			BuildDate:                  buildDate,
			ReadOnly:                   true,
			InsightGenerationAvailable: supportsInsightGeneration(store),
		}),
		server.WithDataDir(appCfg.DataDir),
		server.WithBaseContext(ctx),
	}
	if extras, ok := backend.(replicaServeExtras); ok {
		extraOpts, closeFn, err := extras.serveOptions(ctx, appCfg, target.Target, store)
		if err != nil {
			cleanup()
			return replicaServeStartup{}, fmt.Errorf("%s serve: %w", name, err)
		}
		closeExtras = closeFn
		opts = append(opts, extraOpts...)
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(rtOpts.BasePath))
	}
	return replicaServeStartup{
		cfg: appCfg, ctx: ctx, rtOpts: rtOpts,
		srv: server.New(appCfg, store, nil, opts...), cleanup: cleanup,
	}, nil
}

// supportsInsightGeneration reports whether a replica store proved at startup
// that it can persist generated insights.
func supportsInsightGeneration(store storage.ReplicaStore) bool {
	capable, ok := store.(interface{ InsightGenerationAvailable() bool })
	return ok && capable.InsightGenerationAvailable()
}

func runReplicaServe(backend storage.Replica, appCfg config.Config, basePath string) {
	name := backend.Name()
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("%s serve: generating auth token: %v", name, err)
		}
	}

	startup, err := prepareReplicaServe(backend, appCfg, basePath)
	if err != nil {
		fatal("%v", err)
	}
	defer startup.cleanup()
	appCfg = startup.cfg
	ctx := startup.ctx
	rtOpts := startup.rtOpts
	srv := startup.srv

	rt, err := startServerWithOptionalCaddy(
		ctx,
		appCfg,
		srv,
		rtOpts,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("%s serve: %v", name, err)
	}

	// Write the kit runtime record so CLI commands can discover this
	// daemon. ReadOnly=true marks it as a replica serve (read-only)
	// so clients can select an appropriate transport.
	if writeReplicaServeRuntimeRecord(name, rt) {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}

	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Println("Auth enabled. Token is configured.")
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (%s read-only) at %s\n",
			version, name,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (%s read-only) listening at %s, browser URL: %s\n",
			version, name,
			rt.LocalURL,
			rt.PublicURL,
		)
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("%s serve: %v", name, err)
	}
}

func writeReplicaServeRuntimeRecord(name string, rt *serveRuntime) bool {
	if _, sfErr := writeDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, rt.PublicURL, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr,
			name+" serve daemon may not be discoverable by CLI", "",
		)
		return false
	}
	return true
}

// resolvePushProjects merges the target's configured project scope with the
// command's flags. --projects and --exclude-projects each replace the
// configured scope; --all-projects clears it.
func resolvePushProjects(
	target storage.ConfiguredReplica, cfg ReplicaPushConfig,
) (projects, exclude []string, err error) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		return nil, nil, errors.New("--projects and --exclude-projects are mutually exclusive")
	}
	if cfg.AllProjects &&
		(cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		return nil, nil, errors.New("--all-projects cannot be combined with " +
			"--projects or --exclude-projects",
		)
	}
	projects = target.Projects
	exclude = target.ExcludeProjects
	if cfg.AllProjects {
		projects = nil
		exclude = nil
	}
	if cfg.ProjectsFlag != "" {
		projects = splitProjectList(cfg.ProjectsFlag)
		exclude = nil
	}
	if cfg.ExcludeProjects != "" {
		exclude = splitProjectList(cfg.ExcludeProjects)
		projects = nil
	}
	if len(projects) > 0 && len(exclude) > 0 {
		return nil, nil, errors.New("projects and exclude_projects are mutually exclusive")
	}
	return projects, exclude, nil
}

func splitProjectList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
