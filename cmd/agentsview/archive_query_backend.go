package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
	"go.kenn.io/agentsview/internal/sync"
)

type archiveQueryReadOnlyDaemonPolicy int

const (
	archiveQueryUseReadOnlyDaemon archiveQueryReadOnlyDaemonPolicy = iota
	archiveQuerySkipReadOnlyDaemon
	archiveQueryRejectReadOnlyDaemon
)

type archiveQueryPolicy struct {
	Offline              bool
	NoSync               bool
	AutoStart            bool
	SkipInitialSync      bool
	ReadOnlyDaemon       archiveQueryReadOnlyDaemonPolicy
	DirectReadOnlyAction string
}

type archiveQueryBackend interface {
	ActivityReport(context.Context, ActivityReportConfig) (activity.Report, error)
	DailyUsage(context.Context, dailyUsageQuery) (dailyUsageReport, error)
	SessionUsage(context.Context, sessionUsageQuery) (*sessionUsageOutput, int, error)
	MachineLabels(context.Context) (service.MachineLabelCatalog, error)
}

// sessionUsageQuery selects the session and the attribution scope for
// `session usage`. OwnOnly restores the pre-rollup behavior of reporting
// just the named transcript's own rows. NoSync skips source refreshes while
// preserving the selected attribution scope.
type sessionUsageQuery struct {
	SessionID string
	OwnOnly   bool
	NoSync    bool
}

// dailyUsageReport is a daily usage result plus the sessions its token and
// cost totals leave out as billed to plan usage and the sessions whose
// token counts miss turns live capture did not see.
type dailyUsageReport struct {
	db.DailyUsageResult
	PlanOnlySessions       int
	PartlyCapturedSessions int
}

type dailyUsageQuery struct {
	Progress       func(string)
	Filter         db.UsageFilter
	NoDefaultRange bool
	Breakdowns     bool
	SessionCounts  bool
}

func resolveArchiveQueryBackend(
	ctx context.Context,
	policy archiveQueryPolicy,
) (archiveQueryBackend, func(), error) {
	cfg, err := config.LoadMinimalFor(config.ScopeLocal)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	return resolveArchiveQueryBackendWithConfig(ctx, cfg, policy)
}

func resolveArchiveQueryBackendWithConfig(
	ctx context.Context,
	cfg config.Config,
	policy archiveQueryPolicy,
) (archiveQueryBackend, func(), error) {
	if !policy.Offline {
		tr, err := resolveArchiveQueryTransport(ctx, &cfg, policy)
		if err != nil {
			return nil, nil, fmt.Errorf("detecting daemon: %w", err)
		}
		if tr.Mode == transportHTTP {
			switch {
			case !tr.ReadOnly,
				policy.ReadOnlyDaemon == archiveQueryUseReadOnlyDaemon:
				if policy.AutoStart && !policy.SkipInitialSync && !policy.NoSync && !tr.ReadOnly {
					progress := newResyncProgressPrinter(os.Stderr, time.Now)
					_, err := postDaemonPush[sync.SyncStats](ctx, tr, cfg.AuthToken,
						startupSyncOperation, apiclient.DaemonPushRequest{}, progress.Print)
					progress.Finish()
					if err != nil {
						return nil, nil, fmt.Errorf("waiting for startup sync: %w", err)
					}
				}
				return daemonArchiveQueryBackend{tr: tr, authToken: cfg.AuthToken},
					func() {}, nil
			case policy.ReadOnlyDaemon == archiveQueryRejectReadOnlyDaemon:
				return nil, nil, readOnlySessionUsageDaemonError(tr.URL)
			case policy.ReadOnlyDaemon == archiveQuerySkipReadOnlyDaemon:
				return nil, nil, readOnlyArchiveQueryDaemonError(
					tr.URL, policy.DirectReadOnlyAction,
				)
			default:
				return nil, nil, fmt.Errorf(
					"unknown read-only daemon policy %d",
					policy.ReadOnlyDaemon,
				)
			}
		}
		if tr.DirectReadOnly {
			return nil, nil, directReadOnlyArchiveQueryError(tr, policy)
		}
	}

	database, writeLock, err := openArchiveQueryDB(ctx, cfg, policy.Offline)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { closeWriteDB(database, writeLock) }
	archiveOnly, err := database.IsArchiveOnly(ctx)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	return localArchiveQueryBackend{
		cfg:           cfg,
		database:      database,
		offline:       policy.Offline,
		skipFreshData: policy.NoSync || policy.Offline || archiveOnly,
	}, cleanup, nil
}

func resolveArchiveQueryTransport(
	ctx context.Context,
	cfg *config.Config,
	policy archiveQueryPolicy,
) (transport, error) {
	if policy.AutoStart && !policy.NoSync {
		// Queries that skip startup ingestion can read committed data
		// while the daemon syncs after readiness.
		cfg.SkipInitialSync = policy.SkipInitialSync
		return ensureTransportContext(ctx, cfg, transportIntentArchiveWrite, 0)
	}
	if policy.NoSync {
		cfg.NoSync = true
	}
	return ensureTransportContext(ctx, cfg, transportIntentRead, 0)
}

func directReadOnlyArchiveQueryError(
	tr transport,
	policy archiveQueryPolicy,
) error {
	reason := tr.DirectReason
	if reason == "" {
		reason = errLocalDaemonUnreachable.Error()
	}
	action := policy.DirectReadOnlyAction
	if action == "" {
		action = "query directly"
	}
	return fmt.Errorf("%s; refusing to %s", reason, action)
}

func readOnlyArchiveQueryDaemonError(url string, action string) error {
	if action == "" {
		action = "query directly"
	}
	return fmt.Errorf(
		"daemon at %s is read-only; stop it to %s",
		url, action,
	)
}

func openArchiveQueryDB(
	ctx context.Context,
	cfg config.Config,
	readOnly bool,
) (*db.DB, *writeOwnerLock, error) {
	if readOnly {
		database, err := openReadOnlyDB(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("opening database: %w", err)
		}
		return database, nil, nil
	}
	database, writeLock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}
	return database, writeLock, nil
}

type daemonArchiveQueryBackend struct {
	tr        transport
	authToken string
}

func (b daemonArchiveQueryBackend) ActivityReport(
	ctx context.Context,
	cfg ActivityReportConfig,
) (activity.Report, error) {
	if cfg.Timezone == "" {
		cfg.Timezone = localTimezone()
	}
	if cfg.Preset != "custom" && cfg.From == "" && cfg.Date == "" {
		cfg.Date = todayIn(cfg.Timezone)
	}
	return fetchHTTPActivityReport(ctx, b.tr, b.authToken, cfg)
}

func (b daemonArchiveQueryBackend) DailyUsage(
	ctx context.Context,
	query dailyUsageQuery,
) (dailyUsageReport, error) {
	return fetchHTTPDailyUsage(ctx, b.tr, b.authToken, query)
}

func (b daemonArchiveQueryBackend) SessionUsage(
	ctx context.Context,
	query sessionUsageQuery,
) (*sessionUsageOutput, int, error) {
	return httpSessionUsageData(ctx, b.tr.URL, b.authToken, query)
}

func (b daemonArchiveQueryBackend) MachineLabels(
	ctx context.Context,
) (service.MachineLabelCatalog, error) {
	return service.MachineLabels(
		ctx,
		servicehttp.NewHTTPBackend(b.tr.URL, b.authToken, b.tr.ReadOnly, ""),
	)
}

type localArchiveQueryBackend struct {
	cfg           config.Config
	database      *db.DB
	offline       bool
	skipFreshData bool
}

func (b localArchiveQueryBackend) ActivityReport(
	ctx context.Context,
	cfg ActivityReportConfig,
) (activity.Report, error) {
	ensureFreshData(ctx, b.cfg, b.database, b.skipFreshData)
	return resolveActivityReportPriced(
		cfg, b.database, b.cfg.CustomModelPricing,
	)
}

func (b localArchiveQueryBackend) DailyUsage(
	ctx context.Context,
	query dailyUsageQuery,
) (dailyUsageReport, error) {
	ensureFreshData(ctx, b.cfg, b.database, b.skipFreshData)
	ensureUsagePricing(
		b.database, b.offline, b.cfg.CustomModelPricing,
	)
	filter := localDailyUsageFilter(query)
	var err error
	filter.Machine, err = db.ResolveMachineFilter(ctx, b.database, filter.Machine)
	if err != nil {
		return dailyUsageReport{}, err
	}
	result, err := b.database.GetDailyUsage(ctx, filter)
	if err != nil {
		return dailyUsageReport{}, err
	}
	planUsage, err := b.database.GetPlanUsageSessionCounts(ctx, filter)
	if err != nil {
		return dailyUsageReport{}, err
	}
	return dailyUsageReport{
		DailyUsageResult:       result,
		PlanOnlySessions:       planUsage.PlanOnly,
		PartlyCapturedSessions: planUsage.PartlyCaptured,
	}, nil
}

func (b localArchiveQueryBackend) MachineLabels(
	ctx context.Context,
) (service.MachineLabelCatalog, error) {
	labels, err := b.database.GetMachineLabels(ctx)
	if err != nil {
		return nil, err
	}
	return service.MachineLabelCatalog(labels), nil
}

func localDailyUsageFilter(query dailyUsageQuery) db.UsageFilter {
	filter := query.Filter
	filter.Progress = query.Progress
	filter.Breakdowns = query.Breakdowns
	filter.SkipSessionCounts = !query.SessionCounts
	if filter.Timezone == "" {
		filter.Timezone = "UTC"
	}
	if query.NoDefaultRange {
		return filter
	}
	filter.From, filter.To = defaultUsageDateRange(
		filter.From, filter.To, time.Now(),
	)
	return filter
}

func (b localArchiveQueryBackend) SessionUsage(
	ctx context.Context,
	query sessionUsageQuery,
) (*sessionUsageOutput, int, error) {
	applyCustomPricing(b.database, b.cfg)
	ensureUsagePricing(b.database, b.offline, b.cfg.CustomModelPricing)

	resolvedID, known := resolveRawSessionID(
		ctx, b.database, b.cfg.AgentDirs, query.SessionID,
	)

	stale := b.database.NeedsResync()
	refreshIncomplete := stale && !b.skipFreshData && !query.NoSync
	if known && !b.skipFreshData && !query.NoSync && !stale {
		engine := sync.NewEngine(ctx, b.database, sync.EngineConfig{
			AgentDirs:               b.cfg.AgentDirs,
			SourceMachines:          b.cfg.SourceMachines,
			ProviderMetadata:        b.cfg.ProviderMetadata,
			DisabledAgents:          b.cfg.DisabledAgents,
			IncludeCwdPrefixes:      b.cfg.SyncIncludeCwdPrefixes,
			ScanProtectedPaths:      b.cfg.ScanProtectedPaths,
			Machine:                 b.cfg.InstallationID,
			BlockedResultCategories: b.cfg.ResultContentBlockedCategories,
			ArchiveContent:          b.cfg.ArchiveContent,
		})
		var syncErr error
		if query.OwnOnly {
			syncErr = engine.SyncSingleSessionContext(ctx, resolvedID)
		} else {
			syncErr = engine.SyncSessionWithSubagentsContext(ctx, resolvedID)
		}
		if syncErr != nil {
			refreshIncomplete = true
			fmt.Fprintf(os.Stderr,
				"warning: usage refresh incomplete; showing usage currently stored in the archive: %v\n", syncErr)
		}
		// Flush pending debounced signal recomputes before the
		// usage query reads the session.
		engine.Close()
	}

	load := func() (*service.SessionUsageReport, error) {
		return service.LoadSessionUsage(ctx, b.database, service.SessionUsageRequest{
			ID: resolvedID, OwnOnly: query.OwnOnly,
		})
	}

	u, err := load()
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying session usage: %w", err)
	}
	if u == nil {
		if stale && known && !b.skipFreshData && !query.NoSync {
			return nil, tokenUseExitErr, errors.New(
				"session usage is not yet available; retry after the archive rebuild finishes",
			)
		}
		return nil, tokenUseExitNotFound, nil
	}
	if stale && !b.skipFreshData && !query.NoSync {
		fmt.Fprintln(os.Stderr, "warning: archive data version changed; showing stored usage until the archive is rebuilt")
	}
	if len(u.UnpricedModels) > 0 && !b.offline {
		refreshed, refErr := pricingrefresh.RefreshIfStale(
			b.database, pricing.FetchCatalog,
			pricingrefresh.RefreshCooldown, time.Now(),
		)
		if refErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing refresh failed: %v\n", refErr)
		}
		// A degraded refresh (see pricing.FetchCatalog) stores rows and
		// reports an error at once, so re-read whenever rows changed.
		if refreshed {
			if u2, e := load(); e == nil && u2 != nil {
				u = u2
			}
		}
	}
	out := &sessionUsageOutput{
		SessionUsageReport: *u, RefreshIncomplete: refreshIncomplete,
	}
	return out, usageExitCode(out), nil
}

func closeArchiveQueryBackend(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}
