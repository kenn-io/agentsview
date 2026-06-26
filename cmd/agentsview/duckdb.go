package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		fatal("opening writer: %v", err)
	}
	defer cleanup()

	result, err := backend.DuckDBPush(
		ctx, duckCfg, cfg, projects, excludeProjects,
	)
	if err != nil {
		fatal("duckdb push: %v", err)
	}
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

func runDuckDBStatus() {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb status: %v", err)
	}
	syncer, err := duckdbsync.New(
		duckCfg.Path, database, duckCfg.MachineName,
		duckdbsync.SyncOptions{},
	)
	if err != nil {
		fatal("duckdb status: %v", err)
	}
	defer syncer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	status, err := syncer.Status(ctx)
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
		context.Background(),
		os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	if duckCfg.URL == "" {
		if err := duckdbsync.EnsureSchema(ctx, store.DB()); err != nil {
			fatal("duckdb serve: schema migration failed: %v", err)
		}
	}
	if err := duckdbsync.CheckSchemaCompat(ctx, store.DB()); err != nil {
		fatal("duckdb serve: schema incompatible: %v\n"+
			"Run 'agentsview duckdb push --full' to repopulate the mirror.", err)
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
	if _, sfErr := WriteDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		log.Printf(
			"warning: could not write daemon runtime record: %v"+
				" (duckdb serve daemon may not be discoverable by CLI)",
			sfErr,
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}
	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Printf("Auth token: %s\n", rt.Cfg.AuthToken)
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
	token, generated, err := resolveQuackServeToken(
		cfg.Token, duckCfg.Token, generateQuackToken,
	)
	if err != nil {
		fatal("duckdb quack serve: generating token: %v", err)
	}

	conn, err := duckdbsync.Open(duckCfg.Path)
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
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

	fmt.Printf("DuckDB file: %s\n", duckCfg.Path)
	if info.ListenURI != "" {
		fmt.Printf("Quack URI:   %s\n", info.ListenURI)
	} else {
		fmt.Printf("Quack URI:   %s\n", cfg.Bind)
	}
	if info.HTTPURL != "" {
		fmt.Printf("HTTP URL:    %s\n", info.HTTPURL)
	}
	if generated {
		fmt.Printf("Token:       %s\n", token)
	} else {
		fmt.Println("Token:       configured")
	}
	fmt.Println("Press Ctrl+C to stop.")

	<-ctx.Done()
}

func resolveQuackServeToken(
	flagToken, configuredToken string,
	generate func() (string, error),
) (string, bool, error) {
	if flagToken != "" {
		return flagToken, false, nil
	}
	if configuredToken != "" {
		return configuredToken, false, nil
	}
	token, err := generate()
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

func generateQuackToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
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
	AuthToken string
}

func startQuackServer(
	ctx context.Context, conn *sql.DB, bind, token string, allowOther bool,
) (quackServeInfo, error) {
	query := `CALL quack_serve(?, token => ?)`
	args := []any{bind, token}
	if allowOther {
		query = `CALL quack_serve(?, token => ?, allow_other_hostname => ?)`
		args = append(args, allowOther)
	}
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		return quackServeInfo{}, fmt.Errorf("starting quack server: %w", err)
	}
	return quackServeInfo{ListenURI: bind, AuthToken: token}, nil
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
