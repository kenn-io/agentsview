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
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
)

type PGPushConfig struct {
	Full            bool
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
	NoVectors       bool
}

type PGStatusConfig struct {
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
}

type pgTargetSelection struct {
	Name                   string
	PG                     config.PGConfig
	IsDefault              bool
	SyncStateTarget        string
	MigrateLegacySyncState bool
}

func (s pgTargetSelection) label() string {
	if s.Name == "" {
		return "default"
	}
	if s.IsDefault {
		return s.Name + " (default)"
	}
	return s.Name
}

func (s pgTargetSelection) syncOptions(
	projects, excludeProjects []string,
	vectorSource postgres.VectorPushSource,
) postgres.SyncOptions {
	return postgres.SyncOptions{
		Projects:               projects,
		ExcludeProjects:        excludeProjects,
		SyncStateTarget:        s.SyncStateTarget,
		MigrateLegacySyncState: s.MigrateLegacySyncState,
		VectorSource:           vectorSource,
	}
}

// pgVectorPushSource returns the vectors.db push source to attach for this
// target, or nil when the vector push phase is gated off: the target opts out
// via push_vectors=false, the caller passed --no-vectors, or [vector] is
// disabled (newVectorPushSource itself returns nil then). A nil source leaves
// postgres.Sync's vector phase skipped.
func pgVectorPushSource(
	appCfg config.Config, target pgTargetSelection, cfg PGPushConfig,
) postgres.VectorPushSource {
	if !target.PG.PushVectorsEnabled() || cfg.NoVectors {
		return nil
	}
	return newVectorPushSource(appCfg)
}

func runPGPush(
	cfg PGPushConfig, targetName string,
) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := resolvePGTargetSelections(
		appCfg, targetName, cfg.AllTargets,
	)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
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
			fmt.Printf("Target: %s\n", target.label())
		}
		if err := runPGPushTarget(
			ctx, backend, appCfg, cfg, target,
		); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(
				failures,
				fmt.Sprintf("%s: %v", target.label(), err),
			)
			fmt.Fprintf(
				os.Stderr,
				"warning: pg push target %s failed: %v\n",
				target.label(), err,
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"%d pg target(s) failed: %s",
			len(failures),
			strings.Join(failures, "; "),
		)
	}
	return nil
}

func runPGPushTarget(
	ctx context.Context,
	backend archiveWriteBackend,
	appCfg config.Config,
	cfg PGPushConfig,
	target pgTargetSelection,
) error {
	target, err := resolvePGTargetConfig(appCfg, target)
	if err != nil {
		return err
	}
	if target.PG.URL == "" {
		return fmt.Errorf("url not configured")
	}

	projects, excludeProjects, err := resolvePushProjects(
		target.PG, cfg,
	)
	if err != nil {
		return err
	}

	result, err := backend.PGPush(
		ctx, target, cfg, projects, excludeProjects,
	)
	if err != nil {
		return err
	}
	writePGPushSummary(os.Stdout, result)
	if result.Errors > 0 {
		return fmt.Errorf("%d session(s) failed", result.Errors)
	}
	return nil
}

// pgPushProgressStage buckets progress reports into the display stages that
// each own one progress line: daemon-side setup, fingerprinting, the session
// push, and the vector push.
func pgPushProgressStage(p postgres.PushProgress) string {
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

// newPGPushProgressPrinter returns a progress renderer that updates the
// current stage's line in place and finishes it with a newline when the push
// moves to the next stage, so each completed stage stays in the scrollback
// instead of being overwritten by the next one. Every render carries the
// elapsed time since the printer was created (push start), so long stages
// show how long the push has been running. Renders end with an
// erase-to-end-of-line so a shorter render fully replaces a longer one
// instead of leaving its tail behind.
func newPGPushProgressPrinter() func(postgres.PushProgress) {
	lastStage := ""
	start := time.Now()
	return func(p postgres.PushProgress) {
		stage := pgPushProgressStage(p)
		if lastStage != "" && stage != lastStage {
			fmt.Println()
		}
		lastStage = stage
		fmt.Printf("\r%s (%s elapsed)\x1b[K",
			pgPushProgressLine(p), time.Since(start).Round(time.Second))
	}
}

// pgPushProgressLine renders one progress report as the visible line text for
// its stage; newPGPushProgressPrinter owns the in-place terminal handling.
func pgPushProgressLine(p postgres.PushProgress) string {
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

func writePGPushSummary(w io.Writer, result postgres.PushResult) {
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
			"Warning: skipped %d session(s) owned by another PostgreSQL push marker\n",
			result.SkippedConflicts,
		)
		writePGVectorPushSummary(w, result.Vectors)
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
	writePGVectorPushSummary(w, result.Vectors)
}

// writePGVectorPushSummary prints the vector push phase outcome: a skip
// reason when the phase did not run, otherwise per-session and per-doc
// counters. An ownership conflict count is surfaced separately, analogous to
// the session-level SkippedConflicts warning, since those sessions were left
// untouched on PG because another machine's marker owns them.
func writePGVectorPushSummary(w io.Writer, v postgres.VectorPushResult) {
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
			"Warning: skipped %d vector session(s) owned by another PostgreSQL push marker\n",
			v.Conflicts,
		)
	}
	if v.SessionsDeferred > 0 {
		fmt.Fprintf(
			w,
			"Warning: deferred vectors for %d session(s) whose session push failed; the next successful push sends them\n",
			v.SessionsDeferred,
		)
	}
}

func runPGStatus(targetName string, cfg PGStatusConfig) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := resolvePGTargetSelections(
		appCfg, targetName, cfg.AllTargets,
	)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		log.Printf(
			"warning: reading local pg status watermark: %v",
			err,
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
			fmt.Printf("Target: %s\n", target.label())
		}
		if err := runPGStatusTarget(database, appCfg, target, cfg); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(
				failures,
				fmt.Sprintf("%s: %v", target.label(), err),
			)
			fmt.Fprintf(
				os.Stderr,
				"warning: pg status target %s failed: %v\n",
				target.label(), err,
			)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf(
			"%d pg target(s) failed: %s",
			len(failures),
			strings.Join(failures, "; "),
		)
	}
	return nil
}

func runPGStatusTarget(
	database *db.DB,
	appCfg config.Config,
	target pgTargetSelection,
	cfg PGStatusConfig,
) error {
	target, err := resolvePGTargetConfig(appCfg, target)
	if err != nil {
		return err
	}
	if target.PG.URL == "" {
		return fmt.Errorf("url not configured")
	}
	projects, excludeProjects, err := resolvePushProjects(
		target.PG,
		PGPushConfig{
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

	lastPush := ""
	if database != nil {
		lastPush, err = postgres.ReadLastPushAt(
			database,
			target.SyncStateTarget,
			projects,
			excludeProjects,
			target.MigrateLegacySyncState,
		)
		if err != nil {
			log.Printf(
				"warning: reading last_push_at: %v", err,
			)
			lastPush = ""
		}
	}
	status, err := postgres.ReadStatus(
		ctx,
		target.PG.URL,
		target.PG.Schema,
		target.PG.MachineName,
		target.PG.AllowInsecure,
		lastPush,
	)
	if err != nil {
		return err
	}
	fmt.Printf("Machine:     %s\n", status.Machine)
	fmt.Printf("Last push:   %s\n",
		valueOrNever(status.LastPushAt))
	fmt.Printf("PG sessions: %d\n", status.PGSessions)
	fmt.Printf("PG messages: %d\n", status.PGMessages)
	return nil
}

func loadPGServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadPGServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

func runPGServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("pg serve: generating auth token: %v", err)
		}
	}

	if err := validateServeConfig(appCfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg serve: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg serve: url not configured")
	}

	applyClassifierConfig(appCfg)
	store, err := postgres.NewStore(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		fatal("pg serve: %v", err)
	}
	defer store.Close()

	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	if err := postgres.EnsureSchema(
		ctx, store.DB(), pgCfg.Schema,
	); err != nil {
		if !postgres.IsReadOnlyError(err) {
			fatal("pg serve: schema migration failed: %v", err)
		}
	}

	if err := postgres.CheckSchemaCompat(
		ctx, store.DB(),
	); err != nil {
		fatal("pg serve: schema incompatible: %v\n"+
			"Drop and recreate the PG schema, then run "+
			"'agentsview pg push --full' to repopulate.", err)
	}
	if err := postgres.CheckDataVersionCompat(
		ctx, store.DB(),
	); err != nil {
		fatal("pg serve: %v", err)
	}
	if err := wirePGVectorSearch(ctx, appCfg, store, "pg serve"); err != nil {
		fatal("pg serve: %v", err)
	}
	if err := store.DetectInsightGenerationAvailability(
		ctx,
	); err != nil {
		fatal(
			"pg serve: probing insight generation capability: %v",
			err,
		)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          "pg-serve",
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		fatal("pg serve: %v", err)
	}

	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:                    version,
			Commit:                     commit,
			BuildDate:                  buildDate,
			ReadOnly:                   true,
			InsightGenerationAvailable: store.InsightGenerationAvailable(),
		}),
		server.WithDataDir(appCfg.DataDir),
		server.WithBaseContext(ctx),
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)

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
		fatal("pg serve: %v", err)
	}

	// Write the kit runtime record so CLI commands can discover this
	// daemon. ReadOnly=true marks it as pg serve (read-only)
	// so clients can select an appropriate transport.
	if _, sfErr := writeDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr,
			"pg serve daemon may not be discoverable by CLI", "",
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}

	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Println("Auth enabled. Token is configured.")
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (pg read-only) at %s\n",
			version,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (pg read-only) backend at %s, public at %s\n",
			version,
			rt.LocalURL,
			rt.PublicURL,
		)
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("pg serve: %v", err)
	}
}

func resolvePushProjects(
	pgCfg config.PGConfig, cfg PGPushConfig,
) (projects, exclude []string, err error) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		return nil, nil, fmt.Errorf(
			"--projects and --exclude-projects are mutually exclusive",
		)
	}
	if cfg.AllProjects &&
		(cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		return nil, nil, fmt.Errorf(
			"--all-projects cannot be combined with " +
				"--projects or --exclude-projects",
		)
	}
	projects = pgCfg.Projects
	exclude = pgCfg.ExcludeProjects
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
		return nil, nil, fmt.Errorf(
			"projects and exclude_projects are mutually exclusive",
		)
	}
	return projects, exclude, nil
}

func resolvePGTargetSelections(
	appCfg config.Config,
	targetName string,
	allTargets bool,
) ([]pgTargetSelection, error) {
	if allTargets && strings.TrimSpace(targetName) != "" {
		return nil, fmt.Errorf(
			"target name cannot be combined with --all",
		)
	}
	if len(appCfg.PGTargets) == 0 {
		if strings.TrimSpace(targetName) != "" {
			return nil, fmt.Errorf(
				"pg target %q is not configured; config uses a single legacy [pg] block",
				targetName,
			)
		}
		return []pgTargetSelection{{
			IsDefault: true,
		}}, nil
	}
	names, defaultName, err := appCfg.PGTargetNames()
	if err != nil {
		return nil, err
	}
	selections := make([]pgTargetSelection, 0, len(names))
	for _, name := range names {
		selection := pgTargetSelection{
			Name:                   name,
			IsDefault:              name == defaultName,
			SyncStateTarget:        name,
			MigrateLegacySyncState: name == defaultName,
		}
		selections = append(selections, selection)
	}
	if allTargets {
		return selections, nil
	}
	normalizedTarget := strings.TrimSpace(
		strings.ToLower(targetName),
	)
	if normalizedTarget == "" {
		return selections[:1], nil
	}
	for _, target := range selections {
		if target.Name == normalizedTarget {
			return []pgTargetSelection{target}, nil
		}
	}
	return nil, fmt.Errorf(
		"pg target %q is not configured",
		targetName,
	)
}

func resolvePGTargetConfig(
	appCfg config.Config,
	target pgTargetSelection,
) (pgTargetSelection, error) {
	var (
		pgCfg config.PGConfig
		err   error
	)
	if target.Name == "" {
		pgCfg, err = appCfg.ResolvePG()
	} else {
		pgCfg, err = appCfg.ResolvePGTarget(target.Name)
	}
	if err != nil {
		return pgTargetSelection{}, err
	}
	target.PG = pgCfg
	return target, nil
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
