package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

type artifactFolderPusher struct {
	appCfg        config.Config
	database      *db.DB
	engine        artifactWatchSyncer
	target        string
	origin        string
	token         string
	allowInsecure bool
	// baseline publishes first-run curation metadata (--init) on the next
	// push. It stays set until a push succeeds so a failed initial exchange
	// retries the baseline; AppendBaselineSnapshot skips already-covered
	// fields, making the retry idempotent. Pushes run on a single loop
	// goroutine, so no locking is needed.
	baseline      bool
	onDataChanged func()
}

type artifactWatchSyncer interface {
	SyncAll(context.Context, syncpkg.ProgressFunc) syncpkg.SyncStats
	FlushSignals()
}

func newArtifactWatchEngine(
	database *db.DB, appCfg config.Config,
) *syncpkg.Engine {
	return syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		IncludeCwdPrefixes:      appCfg.SyncIncludeCwdPrefixes,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})
}

func (p *artifactFolderPusher) push(
	ctx context.Context, reason pushReason,
) error {
	if reason == reasonShutdown {
		ctx = artifact.SuppressArtifactMaintenance(ctx)
	}
	if p.engine != nil {
		// Startup already performed a full sync, and watcher change bursts have
		// already applied their targeted paths. The periodic floor covers roots
		// that could not be watched, while shutdown discovers events still held
		// in the watcher's batching window before the final export.
		if reason == reasonInterval || reason == reasonShutdown {
			p.engine.SyncAll(ctx, nil)
		}
		// Export reads session rows outside a sync operation; flush
		// debounced signal recomputes so manifests carry current signals.
		p.engine.FlushSignals()
	}
	res, err := syncArtifactFolder(
		ctx, p.appCfg, p.database, p.target, p.origin, p.token,
		p.allowInsecure, p.baseline, p.onDataChanged,
	)
	if err != nil {
		return err
	}
	p.baseline = false
	log.Printf(
		"artifact watch: exported %d sessions, imported %d sessions, %d messages, %d metadata events (%s)",
		res.ExportedSessions, res.ImportedSessions, res.ImportedMessages,
		res.ImportedMetadata, reason,
	)
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

	origin, err := resolveArtifactOrigin(appCfg, database)
	if err != nil {
		fatal("artifact watch origin: %v", err)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	engine := newArtifactWatchEngine(database, appCfg)
	defer engine.Close()

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
		appCfg:        appCfg,
		database:      database,
		engine:        engine,
		target:        cfg.ArtifactFolder,
		origin:        origin,
		token:         artifactPeerToken(cfg),
		allowInsecure: cfg.AllowInsecure,
		baseline:      cfg.Init,
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
