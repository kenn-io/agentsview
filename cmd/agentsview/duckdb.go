package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
)

type DuckDBPushConfig struct {
	Full            bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
}

type DuckDBQuackServeConfig struct {
	Bind          string
	Path          string
	Token         string
	AllowInsecure bool
}

func runDuckDBPush(cfg DuckDBPushConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	projects, excludeProjects, err := resolveDuckDBPushProjects(duckCfg, cfg)
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		fatal("duckdb push: %v", err)
	}
	syncStateTarget := duckdbsync.SyncStateTargetForConfig(duckCfg)
	writeDuckDBPushPlan(
		os.Stdout, duckCfg, cfg, projects, excludeProjects, syncStateTarget,
	)

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		fatal("opening writer: %v", err)
	}
	defer cleanup()

	if cfg.Watch {
		fmt.Printf(
			"agentsview duckdb watch: pushing to DuckDB "+
				"(debounce %s, floor %s)\n",
			cfg.Debounce, cfg.Interval,
		)
		if err := backend.DuckDBPushWatch(
			ctx, duckCfg, cfg, projects, excludeProjects,
			cfg.Debounce, cfg.Interval,
		); err != nil {
			fatal("duckdb watch: %v", err)
		}
		return
	}

	result, err := backend.DuckDBPush(
		ctx, duckCfg, cfg, projects, excludeProjects,
	)
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	writeDuckDBPushDiagnostics(os.Stdout, result)
	fmt.Printf(
		"Pushed %d sessions, %d messages to DuckDB in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		result.Duration.Round(time.Millisecond),
	)
	if result.Errors > 0 {
		fatal("duckdb push: %d session(s) failed", result.Errors)
	}
}

func writeDuckDBPushPlan(
	w io.Writer,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
	syncStateTarget string,
) {
	target := "local file " + duckCfg.Path
	mode := "incremental"
	if cfg.Full {
		mode = "full"
	}
	scope := "default"
	if syncStateTarget != "" {
		scope = syncStateTarget
	}
	fmt.Fprintf(
		w,
		"DuckDB push target: %s; machine %q; mode %s; sync scope %s\n",
		target, duckCfg.MachineName, mode, scope,
	)
	fmt.Fprintf(
		w, "DuckDB push filters: %s\n",
		formatDuckDBPushFilters(projects, excludeProjects),
	)
}

func writeDuckDBPushDiagnostics(w io.Writer, result duckdbsync.PushResult) {
	if result.Diagnostics.Cutoff == "" {
		return
	}
	fmt.Fprintf(
		w,
		"DuckDB push source: local %s; candidates %s; skipped unchanged %s; stale deleted %d\n",
		formatDuckDBPushSessionCounts(result.Diagnostics.LocalSessions),
		formatDuckDBPushSessionCounts(result.Diagnostics.CandidateSessions),
		formatDuckDBPushSessionCounts(result.Diagnostics.SkippedUnchangedSessions),
		result.Diagnostics.DeletedStaleSessions,
	)
	fmt.Fprintf(
		w,
		"DuckDB push wrote: sessions %s, messages %d\n",
		formatDuckDBPushSessionCounts(result.Diagnostics.PushedSessions),
		result.MessagesPushed,
	)
}

func formatDuckDBPushFilters(projects []string, excludeProjects []string) string {
	switch {
	case len(projects) > 0:
		return "include projects " + strings.Join(projects, ", ")
	case len(excludeProjects) > 0:
		return "exclude projects " + strings.Join(excludeProjects, ", ")
	default:
		return "all projects"
	}
}

func formatDuckDBPushSessionCounts(counts duckdbsync.PushSessionCounts) string {
	if len(counts.ByAgent) == 0 {
		return fmt.Sprintf("%d", counts.Total)
	}
	agents := make([]string, 0, len(counts.ByAgent))
	for agent := range counts.ByAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	parts := make([]string, 0, len(agents))
	for _, agent := range agents {
		parts = append(parts, fmt.Sprintf("%s=%d", agent, counts.ByAgent[agent]))
	}
	return fmt.Sprintf("%d (%s)", counts.Total, strings.Join(parts, ", "))
}

func duckDBLongRunningSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func runDuckDBStatus() {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb status: %v", err)
	}
	lastPush := ""
	syncStateTarget := duckdbsync.SyncStateTargetForConfig(duckCfg)
	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		if duckCfg.URL == "" {
			fatal("opening database: %v", err)
		}
		log.Printf(
			"warning: reading local duckdb status watermark: %v",
			err,
		)
	} else {
		defer database.Close()
		lastPush, err = duckdbsync.ReadLastPushAt(database, syncStateTarget)
		if err != nil {
			log.Printf("warning: reading duckdb last push: %v", err)
			lastPush = ""
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	status, err := duckdbsync.ReadStatusFromConfig(ctx, duckCfg, lastPush)
	if err != nil {
		fatal("duckdb status: %v", err)
	}
	fmt.Printf("Machine:         %s\n", status.Machine)
	fmt.Printf("Last push:       %s\n", valueOrNever(status.LastPushAt))
	fmt.Printf("DuckDB sessions: %d\n", status.DuckDBSessions)
	fmt.Printf("DuckDB messages: %d\n", status.DuckDBMessages)
}

func loadDuckDBServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadDuckDBServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

func runDuckDBServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("duckdb serve: generating auth token: %v", err)
		}
	}
	if err := validateServeConfig(appCfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	if duckCfg.URL == "" && duckCfg.Path == "" {
		fatal("duckdb serve: path or url not configured")
	}

	applyClassifierConfig(appCfg)
	store, err := duckdbsync.NewStoreFromConfig(duckCfg)
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	defer store.Close()
	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}
	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(appCfg.CursorSecret)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		store.SetCursorSecret(secret)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	if duckCfg.URL == "" {
		if err := duckdbsync.EnsureSchema(ctx, store.DB()); err != nil {
			fatal("duckdb serve: schema migration failed: %v", err)
		}
	}
	var schemaErr error
	if duckCfg.URL == "" {
		schemaErr = duckdbsync.CheckSchemaCompat(ctx, store.DB())
	} else {
		schemaErr = duckdbsync.CheckSchemaCompatViaQuack(ctx, store.DB())
	}
	if schemaErr != nil {
		if duckCfg.URL == "" {
			fatal("duckdb serve: schema incompatible: %v\n"+
				"Run 'agentsview duckdb push --full' to repopulate the mirror.", schemaErr)
		}
		fatal("duckdb serve: schema incompatible: %v", schemaErr)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          "duckdb-serve",
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
			ReadOnly:  true,
		}),
		server.WithDataDir(appCfg.DataDir),
		server.WithBaseContext(ctx),
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)
	rt, err := startServerWithOptionalCaddy(ctx, appCfg, srv, rtOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("duckdb serve: %v", err)
	}
	if _, sfErr := writeDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr,
			"duckdb serve daemon may not be discoverable by CLI", "",
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}
	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Println("Auth enabled. Token is configured.")
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (duckdb read-only) at %s\n",
			version,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (duckdb read-only) backend at %s, public at %s\n",
			version,
			rt.LocalURL,
			rt.PublicURL,
		)
	}
	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("duckdb serve: %v", err)
	}
}

func runDuckDBQuackServe(cfg DuckDBQuackServeConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	if cfg.Path != "" {
		duckCfg.Path = cfg.Path
	}
	if cfg.AllowInsecure {
		duckCfg.AllowInsecure = true
	}
	if err := duckdbsync.ValidateQuackServeURI(
		cfg.Bind, duckCfg.AllowInsecure,
	); err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	token, err := resolveQuackServeToken(cfg.Token, duckCfg.Token)
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}

	conn, err := duckdbsync.Open(duckCfg.Path)
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	if err := duckdbsync.EnsureSchema(ctx, conn); err != nil {
		fatal("duckdb quack serve: schema migration failed: %v", err)
	}
	if err := duckdbsync.CheckSchemaCompat(ctx, conn); err != nil {
		fatal("duckdb quack serve: schema incompatible: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSTALL quack"); err != nil {
		fatal("duckdb quack serve: installing quack: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "LOAD quack"); err != nil {
		fatal("duckdb quack serve: loading quack: %v", err)
	}
	identifyQuackNode(ctx, conn, duckCfg.MachineName)

	info, err := startQuackServer(
		ctx, conn, cfg.Bind, token, duckCfg.AllowInsecure,
	)
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	defer func() {
		if _, stopErr := conn.ExecContext(
			context.Background(), `CALL quack_stop(?)`, cfg.Bind,
		); stopErr != nil {
			log.Printf("warning: could not stop Quack server: %v", stopErr)
		}
	}()

	writeDuckDBQuackServeStartup(os.Stdout, duckDBQuackServeStartup{
		Path: duckCfg.Path,
		Bind: cfg.Bind,
		Info: info,
	})

	<-ctx.Done()
}

type duckDBQuackServeStartup struct {
	Path string
	Bind string
	Info quackServeInfo
}

func writeDuckDBQuackServeStartup(
	out io.Writer,
	startup duckDBQuackServeStartup,
) {
	fmt.Fprintf(out, "DuckDB file: %s\n", startup.Path)
	if startup.Info.ListenURI != "" {
		fmt.Fprintf(out, "Quack URI:   %s\n", startup.Info.ListenURI)
	} else {
		fmt.Fprintf(out, "Quack URI:   %s\n", startup.Bind)
	}
	if startup.Info.HTTPURL != "" {
		fmt.Fprintf(out, "HTTP URL:    %s\n", startup.Info.HTTPURL)
	}
	fmt.Fprintln(out, "Token:       configured")
	fmt.Fprintln(out, "Press Ctrl+C to stop.")
}

func resolveQuackServeToken(
	flagToken, configuredToken string,
) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if configuredToken != "" {
		return configuredToken, nil
	}
	return "", fmt.Errorf(
		"token is required; set --token, AGENTSVIEW_DUCKDB_TOKEN, or [duckdb].token",
	)
}

func identifyQuackNode(ctx context.Context, conn *sql.DB, machine string) {
	meta := fmt.Sprintf(
		`{"version":%q,"commit":%q,"build_date":%q}`,
		version, commit, buildDate,
	)
	_, err := conn.ExecContext(ctx,
		`CALL quack_identify(?, ?, ?, ?, ?)`,
		"agentsview", "agentsview", machine, "", meta,
	)
	if err != nil {
		log.Printf("warning: could not identify Quack node: %v", err)
	}
}

type quackServeInfo struct {
	ListenURI string
	HTTPURL   string
}

func startQuackServer(
	ctx context.Context, conn *sql.DB, bind, token string, allowOther bool,
) (quackServeInfo, error) {
	query := `SELECT listen_uri, listen_url FROM quack_serve(?, token => ?)`
	args := []any{bind, token}
	if allowOther {
		query = `SELECT listen_uri, listen_url FROM quack_serve(?, token => ?, allow_other_hostname => ?)`
		args = append(args, allowOther)
	}
	var listenURI, httpURL sql.NullString
	if err := conn.QueryRowContext(ctx, query, args...).Scan(
		&listenURI, &httpURL,
	); err != nil {
		return quackServeInfo{}, fmt.Errorf("starting quack server: %w", err)
	}
	info := quackServeInfo{ListenURI: bind}
	if listenURI.Valid && listenURI.String != "" {
		info.ListenURI = listenURI.String
	}
	if httpURL.Valid {
		info.HTTPURL = httpURL.String
	}
	return info, nil
}

func resolveDuckDBPushProjects(
	duckCfg config.DuckDBConfig, cfg DuckDBPushConfig,
) (projects, exclude []string, err error) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		return nil, nil, fmt.Errorf(
			"--projects and --exclude-projects are mutually exclusive",
		)
	}
	if cfg.AllProjects &&
		(cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		return nil, nil, fmt.Errorf(
			"--all-projects cannot be combined with --projects or --exclude-projects",
		)
	}
	projects = duckCfg.Projects
	exclude = duckCfg.ExcludeProjects
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
