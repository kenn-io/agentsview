package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/update"
)

type serveReplacementAction int

const (
	serveReplacementNone serveReplacementAction = iota
	serveReplacementUseExisting
	serveReplacementAuto
	serveReplacementExplicit
	serveReplacementRefuse
)

type serveReplacementOptions struct {
	Replace        bool
	NoSyncExplicit bool
}

type serveReplacementDecision struct {
	Action           serveReplacementAction
	Runtime          *DaemonRuntime
	CompatibilityErr error
	Reason           string
}

var foregroundServeLaunchLocks sync.Map

func prepareForegroundServeDaemon(
	cfg *config.Config, opts serveReplacementOptions,
) (bool, func(), error) {
	noRelease := func() {}
	if cfg == nil {
		return false, noRelease, errors.New("nil config")
	}
	decision := decideServeDaemonReplacement(*cfg, opts)
	switch decision.Action {
	case serveReplacementNone:
		if runningAsBackgroundChild() {
			return true, noRelease, nil
		}
		// A normal foreground start marks the daemon as starting after this
		// function returns. Hold the launch lock first so background
		// replacement cannot stop an incumbent during that handoff.
		releaseLaunchLock, err := acquireForegroundServeLaunchLock(*cfg)
		if err != nil {
			return false, noRelease, err
		}
		return true, releaseLaunchLock, nil
	case serveReplacementUseExisting:
		rt := decision.Runtime
		if rt != nil {
			fmt.Printf(
				"agentsview already running at %s (pid %d)\n",
				urlFromDaemonRuntime(rt), rt.Record.PID,
			)
		}
		return false, noRelease, nil
	case serveReplacementAuto, serveReplacementExplicit:
		if err := checkForegroundReplacementDataVersion(
			*cfg, decision,
		); err != nil {
			return false, noRelease, err
		}
		releaseReplacementLock, err := acquireForegroundServeLaunchLock(*cfg)
		if err != nil {
			return false, noRelease, err
		}

		ownsStartLock, acquiredStartLock := markDaemonStarting(cfg.DataDir)
		if !ownsStartLock {
			releaseReplacementLock()
			return false, noRelease, fmt.Errorf(
				"agentsview serve startup is already in progress; " +
					"wait for it to finish or run `agentsview serve status`",
			)
		}
		fmt.Println("Replacing agentsview daemon")
		for _, line := range serveDaemonReplacementLines(decision) {
			fmt.Println(line)
		}
		if !opts.NoSyncExplicit {
			adoptDaemonRuntimeLaunchOptions(cfg, decision.Runtime)
		}
		if err := stopDaemonRuntimeForUpgrade(*cfg, decision.Runtime); err != nil {
			if acquiredStartLock {
				UnmarkDaemonStarting(cfg.DataDir)
			}
			releaseReplacementLock()
			return false, noRelease, err
		}
		return true, releaseReplacementLock, nil
	case serveReplacementRefuse:
		return false, noRelease, errors.New(strings.Join(
			serveDaemonConflictLines(decision), "\n",
		))
	default:
		return false, noRelease, fmt.Errorf("unknown serve replacement action %d",
			decision.Action)
	}
}

func acquireForegroundServeLaunchLock(cfg config.Config) (func(), error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	lock, ok := acquireBackgroundLaunchLock(cfg.DataDir)
	if !ok {
		return nil, fmt.Errorf(
			"agentsview serve --background is already in progress; " +
				"wait for it to finish or run `agentsview serve status`",
		)
	}
	path := backgroundLaunchLockPath(cfg.DataDir)
	foregroundServeLaunchLocks.Store(path, struct{}{})
	return sync.OnceFunc(func() {
		foregroundServeLaunchLocks.Delete(path)
		_ = lock.Unlock()
	}), nil
}

func ownsForegroundServeLaunchLock(dataDir string) bool {
	_, ok := foregroundServeLaunchLocks.Load(
		backgroundLaunchLockPath(dataDir),
	)
	return ok
}

func checkForegroundReplacementDataVersion(
	cfg config.Config, decision serveReplacementDecision,
) error {
	if cfg.DBPath == "" {
		return nil
	}
	switch decision.Action {
	case serveReplacementAuto, serveReplacementExplicit:
	default:
		return nil
	}
	return db.CheckDataVersion(cfg.DBPath)
}

func decideServeDaemonReplacement(
	cfg config.Config, opts serveReplacementOptions,
) serveReplacementDecision {
	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
		!rt.ReadOnly {
		return decideCompatibleServeDaemonReplacement(rt, opts)
	}

	rt, compatErr := findIncompatibleWritableDaemonRuntime(
		cfg.DataDir, cfg.AuthToken,
	)
	if rt == nil {
		if opts.Replace {
			if rt := findConfirmedUnreachableWritableDaemonRuntime(cfg); rt != nil {
				return serveReplacementDecision{
					Action:  serveReplacementExplicit,
					Runtime: rt,
					Reason:  "replacement requested with --replace",
				}
			}
		}
		return serveReplacementDecision{Action: serveReplacementNone}
	}
	return decideIncompatibleServeDaemonReplacement(rt, compatErr, opts)
}

func findConfirmedUnreachableWritableDaemonRuntime(
	cfg config.Config,
) *DaemonRuntime {
	for _, rec := range liveDaemonRecords(cfg.DataDir) {
		rt := daemonRuntimeFromRecord(rec)
		if rt.ReadOnly {
			continue
		}
		if daemonRecordPingConfirmed(rec, cfg.AuthToken) {
			continue
		}
		if !stopTargetConfirmed(rec, cfg.AuthToken) {
			continue
		}
		return rt
	}
	return nil
}

func decideCompatibleServeDaemonReplacement(
	rt *DaemonRuntime, opts serveReplacementOptions,
) serveReplacementDecision {
	decision := serveReplacementDecision{Runtime: rt}
	if opts.Replace {
		decision.Action = serveReplacementExplicit
		decision.Reason = "replacement requested with --replace"
		return decision
	}
	if shouldUpgradeDaemonRuntime(rt, version) {
		decision.Action = serveReplacementAuto
		decision.Reason = serveDaemonOlderReason(rt)
		return decision
	}
	if rt.Record.Version != version {
		decision.Action = serveReplacementRefuse
		decision.Reason = serveDaemonRefusalReason(rt, nil)
		return decision
	}
	decision.Action = serveReplacementUseExisting
	decision.Reason = "compatible writable daemon is already running"
	return decision
}

func decideIncompatibleServeDaemonReplacement(
	rt *DaemonRuntime, compatErr error, opts serveReplacementOptions,
) serveReplacementDecision {
	decision := serveReplacementDecision{
		Runtime:          rt,
		CompatibilityErr: compatErr,
	}
	if opts.Replace {
		decision.Action = serveReplacementExplicit
		decision.Reason = "replacement requested with --replace"
		return decision
	}
	if shouldUpgradeIncompatibleDaemonRuntime(rt, version) {
		decision.Action = serveReplacementAuto
		decision.Reason = serveDaemonOlderReason(rt)
		return decision
	}
	decision.Action = serveReplacementRefuse
	decision.Reason = serveDaemonRefusalReason(rt, compatErr)
	return decision
}

func serveDaemonOlderReason(rt *DaemonRuntime) string {
	daemonVersion := serveDaemonVersion(rt)
	if rt == nil || rt.Record.Version == "" {
		return fmt.Sprintf(
			"daemon version is unknown and treated as older than current "+
				"binary version %s",
			serveCurrentVersion(),
		)
	}
	return fmt.Sprintf(
		"daemon version %s is older than current binary version %s",
		daemonVersion, serveCurrentVersion(),
	)
}

func serveDaemonRefusalReason(
	rt *DaemonRuntime, compatErr error,
) string {
	if rt != nil && (rt.API > daemonAPIVersion ||
		rt.Data > db.CurrentDataVersion()) {
		return fmt.Sprintf(
			"daemon API/data version is ahead of this binary "+
				"(daemon API %d, data %d; current API %d, data %d)",
			rt.API, rt.Data, daemonAPIVersion, db.CurrentDataVersion(),
		)
	}
	if update.IsDevBuildVersion(version) {
		return fmt.Sprintf(
			"current binary version %s is a dev build; dev builds do not "+
				"replace running daemons automatically",
			serveCurrentVersion(),
		)
	}
	if rt != nil && update.IsNewer(rt.Record.Version, version) {
		return fmt.Sprintf(
			"daemon version %s is newer than current binary version %s",
			serveDaemonVersion(rt), serveCurrentVersion(),
		)
	}
	if compatErr != nil {
		return fmt.Sprintf(
			"current binary version %s is not newer than daemon version %s "+
				"and cannot automatically replace the incompatible daemon: %v",
			serveCurrentVersion(), serveDaemonVersion(rt), compatErr,
		)
	}
	return fmt.Sprintf(
		"current binary version %s is not newer than daemon version %s",
		serveCurrentVersion(), serveDaemonVersion(rt),
	)
}

func serveDaemonConflictLines(decision serveReplacementDecision) []string {
	lines := serveDaemonDecisionLines(
		"agentsview found a running writable daemon that will not be replaced.",
		decision,
	)
	if decision.Runtime == nil {
		return lines
	}
	switch decision.Action {
	case serveReplacementRefuse:
		return append(lines,
			"Run `agentsview serve --replace` to replace it, or "+
				"`agentsview serve stop` to stop it first.",
		)
	case serveReplacementUseExisting:
		return append(lines,
			"Using the existing daemon. Run `agentsview serve stop` "+
				"to stop it first.",
		)
	default:
		return append(lines,
			"Run `agentsview serve stop` to stop it first.",
		)
	}
}

func serveDaemonReplacementLines(decision serveReplacementDecision) []string {
	lines := serveDaemonDecisionLines(
		"agentsview will replace a running writable daemon.",
		decision,
	)
	if decision.Runtime == nil {
		return lines
	}
	return append(lines,
		"Run `agentsview serve stop` to stop it manually instead.",
	)
}

func serveDaemonDecisionLines(
	header string, decision serveReplacementDecision,
) []string {
	rt := decision.Runtime
	if rt == nil {
		return []string{"No writable agentsview daemon is running."}
	}
	lines := []string{
		header,
		fmt.Sprintf("  url:             %s", urlFromDaemonRuntime(rt)),
		fmt.Sprintf("  pid:             %d", rt.Record.PID),
		fmt.Sprintf("  daemon version:  %s", serveDaemonVersion(rt)),
		fmt.Sprintf("  binary version:  %s", serveCurrentVersion()),
		fmt.Sprintf(
			"  API version:     daemon %d, current %d",
			rt.API, daemonAPIVersion,
		),
		fmt.Sprintf(
			"  data version:    daemon %d, current %d",
			rt.Data, db.CurrentDataVersion(),
		),
	}
	if decision.CompatibilityErr != nil {
		lines = append(lines,
			fmt.Sprintf("  compatibility:   %v", decision.CompatibilityErr),
		)
	}
	if decision.Reason != "" {
		lines = append(lines, fmt.Sprintf("  reason:          %s", decision.Reason))
	}
	return lines
}

func serveDaemonVersion(rt *DaemonRuntime) string {
	if rt == nil || rt.Record.Version == "" {
		return "(unknown)"
	}
	return rt.Record.Version
}

func serveCurrentVersion() string {
	if version == "" {
		return "(unknown)"
	}
	return version
}
