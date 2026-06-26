package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pricing"
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
	ReadOnlyDaemon       archiveQueryReadOnlyDaemonPolicy
	DirectReadOnlyAction string
}

type archiveQueryBackend interface {
	ActivityReport(context.Context, ActivityReportConfig) (activity.Report, error)
	DailyUsage(context.Context, dailyUsageQuery) (db.DailyUsageResult, error)
	SessionUsage(context.Context, string) (*sessionUsageOutput, int, error)
}

type dailyUsageQuery struct {
	Filter         db.UsageFilter
	NoDefaultRange bool
	Breakdowns     bool
	SessionCounts  bool
}

func resolveArchiveQueryBackend(
	ctx context.Context,
	policy archiveQueryPolicy,
) (archiveQueryBackend, func(), error) {
	cfg, err := config.LoadMinimal()
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
		tr, err := resolveArchiveQueryTransport(&cfg, policy)
		if err != nil {
			return nil, nil, fmt.Errorf("detecting daemon: %w", err)
		}
		if tr.Mode == transportHTTP {
			switch {
			case !tr.ReadOnly,
				policy.ReadOnlyDaemon == archiveQueryUseReadOnlyDaemon:
				return daemonArchiveQueryBackend{tr: tr, authToken: cfg.AuthToken},
					func() {}, nil
			case policy.ReadOnlyDaemon == archiveQueryRejectReadOnlyDaemon:
				return nil, nil, readOnlySessionUsageDaemonError(tr.URL)
			case policy.ReadOnlyDaemon == archiveQuerySkipReadOnlyDaemon:
				// Fall through to the local read-only archive below.
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
	return localArchiveQueryBackend{
		cfg:           cfg,
		database:      database,
		offline:       policy.Offline,
		skipFreshData: policy.NoSync || policy.Offline,
	}, cleanup, nil
}

func resolveArchiveQueryTransport(
	cfg *config.Config,
	policy archiveQueryPolicy,
) (transport, error) {
	if policy.AutoStart && !policy.NoSync {
		return ensureTransport(cfg, transportIntentArchiveWrite, 0)
	}
	return detectTransport(cfg.DataDir, cfg.AuthToken, 0)
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

func openArchiveQueryDB(
	ctx context.Context,
	cfg config.Config,
	readOnly bool,
) (*db.DB, *writeOwnerLock, error) {
	if readOnly {
		database, err := openReadOnlyDB(cfg)
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
) (db.DailyUsageResult, error) {
	return fetchHTTPDailyUsage(ctx, b.tr, b.authToken, query)
}

func (b daemonArchiveQueryBackend) SessionUsage(
	ctx context.Context,
	sessionID string,
) (*sessionUsageOutput, int, error) {
	return httpSessionUsageData(ctx, b.tr.URL, b.authToken, sessionID)
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
) (db.DailyUsageResult, error) {
	ensureFreshData(ctx, b.cfg, b.database, b.skipFreshData)
	ensureUsagePricing(
		b.database, b.offline, b.cfg.CustomModelPricing,
	)
	filter := localDailyUsageFilter(query)
	return b.database.GetDailyUsage(ctx, filter)
}

func localDailyUsageFilter(query dailyUsageQuery) db.UsageFilter {
	filter := query.Filter
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
	sessionID string,
) (*sessionUsageOutput, int, error) {
	applyCustomPricing(b.database, b.cfg)
	ensureUsagePricing(b.database, b.offline, b.cfg.CustomModelPricing)

	resolvedID, known := resolveRawSessionID(
		ctx, b.database, b.cfg.AgentDirs, sessionID,
	)

	if known && !b.skipFreshData {
		engine := sync.NewEngine(b.database, sync.EngineConfig{
			AgentDirs:               b.cfg.AgentDirs,
			Machine:                 "local",
			BlockedResultCategories: b.cfg.ResultContentBlockedCategories,
		})
		if syncErr := engine.SyncSingleSessionContext(
			ctx, resolvedID,
		); syncErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: sync failed: %v\n", syncErr)
		}
	}

	u, err := b.database.GetSessionUsage(ctx, resolvedID)
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying session usage: %w", err)
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	if len(u.UnpricedModels) > 0 && !b.offline {
		refreshed, refErr := refreshPricingIfStale(
			b.database, pricing.FetchLiteLLMPricing,
			pricingRefreshCooldown, time.Now(),
		)
		if refErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing refresh failed: %v\n", refErr)
		} else if refreshed {
			if u2, e := b.database.GetSessionUsage(
				ctx, resolvedID,
			); e == nil && u2 != nil {
				u = u2
			}
		}
	}
	if u.Agent == "" {
		if def, ok := parser.AgentByPrefix(u.SessionID); ok {
			u.Agent = string(def.Type)
		}
	}
	return &sessionUsageOutput{
		SessionUsage:  *u,
		ServerRunning: false,
	}, usageExitCode(u), nil
}

func closeArchiveQueryBackend(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}
