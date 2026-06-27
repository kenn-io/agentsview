package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

type artifactFolderPusher struct {
	appCfg        config.Config
	database      *db.DB
	engine        *syncpkg.Engine
	target        string
	origin        string
	token         string
	gcGrace       time.Duration
	gcEnabled     bool
	onDataChanged func()
}

func (p *artifactFolderPusher) push(
	ctx context.Context, reason pushReason,
) error {
	if p.engine != nil {
		p.engine.SyncAll(ctx, nil)
	}
	res, err := syncArtifactFolder(
		ctx, p.appCfg, p.database, p.target, p.origin, p.token, p.onDataChanged,
	)
	if err != nil {
		return err
	}
	log.Printf(
		"artifact watch: exported %d sessions, imported %d sessions, %d messages, %d metadata events (%s)",
		res.ExportedSessions, res.ImportedSessions, res.ImportedMessages,
		res.ImportedMetadata, reason,
	)
	// Collect superseded artifacts on the periodic floor and at startup, not on
	// every debounced change, to keep frequent edits cheap.
	if p.gcEnabled && (reason == reasonStartup || reason == reasonInterval) {
		autoGCAfterFolderSync(ctx, p.appCfg.DataDir, p.target, p.gcGrace)
	}
	return nil
}

// runSyncWatch runs continuous artifact folder sync: an initial local sync and
// artifact exchange, then debounced file-change exchanges and a periodic floor.
func runSyncWatch(cfg SyncConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFileNamed(appCfg.DataDir, "artifact-watch.log")

	if cfg.ArtifactFolder == "" {
		fatal("artifact watch: folder target is required")
	}

	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultWatchInterval
	}

	lockPath, err := (daemon.RuntimeStore{
		Dir:    appCfg.DataDir,
		Prefix: "artifact-watch",
	}).LockPath()
	if err != nil {
		fatal("artifact watch: %v", err)
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		fatal("artifact watch: locking %s: %v", lockPath, err)
	}
	if !locked {
		fatal("artifact watch: already locked (%s)", lockPath)
	}
	defer func() {
		if rerr := lock.Unlock(); rerr != nil {
			log.Printf("artifact watch: releasing lock: %v", rerr)
		}
	}()

	applyClassifierConfig(appCfg)
	database, writeLock := mustOpenWriteDB(context.Background(), appCfg)
	defer closeWriteDB(database, writeLock)

	for _, def := range parser.Registry {
		if !appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(appCfg.ResolveDirs(def.Type), string(def.Type))
	}
	cleanResyncTemp(appCfg.DBPath)

	origin, err := appCfg.EnsureArtifactOriginID()
	if err != nil {
		fatal("artifact watch origin: %v", err)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})

	didResync := cfg.Full || database.NeedsResync()
	if didResync {
		engine.ResyncAll(ctx, nil)
	} else {
		engine.SyncAll(ctx, nil)
	}
	if ctx.Err() != nil {
		return
	}

	pusher := &artifactFolderPusher{
		appCfg:    appCfg,
		database:  database,
		engine:    engine,
		target:    cfg.ArtifactFolder,
		origin:    origin,
		token:     artifactPeerToken(cfg),
		gcGrace:   cfg.GCGrace,
		gcEnabled: !cfg.NoGC,
	}

	log.Printf(
		"artifact watch: starting (origin=%q target=%q debounce=%s interval=%s)",
		origin, cfg.ArtifactFolder, debounce, interval,
	)
	fmt.Printf(
		"agentsview sync --watch: syncing artifacts as %q to %s "+
			"(debounce %s, floor %s)\n",
		origin, cfg.ArtifactFolder, debounce, interval,
	)

	if err := pusher.push(ctx, reasonStartup); err != nil {
		log.Printf("artifact watch: initial sync failed: %v", err)
	}

	runWatchedSink(ctx, watchedSinkConfig{
		AppConfig: appCfg,
		Engine:    engine,
		Debounce:  debounce,
		Interval:  interval,
		LogPrefix: "artifact watch",
		Push: func(c context.Context, r pushReason) error {
			return pusher.push(c, r)
		},
	})
}
