package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/ssh"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func (s *Server) registerSyncRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Sync")

	stream(s, group, http.MethodPost, "/sync", "Trigger sync", s.humaTriggerSync)
	stream(s, group, http.MethodPost, "/resync", "Trigger full resync", s.humaTriggerResync)
	get(s, group, "/sync/status", "Get sync status", s.humaSyncStatus)
	stream(
		s, group, http.MethodPost, "/sync/remotes",
		"Sync remote hosts", s.humaSyncRemotes, streamJSONResponse(),
	)
	postLong(s, group, "/sessions/sync", "Sync a session", s.humaSyncSession)
}

type syncStatusResponse struct {
	LastSync string             `json:"last_sync"`
	Stats    *syncpkg.SyncStats `json:"stats"`
	Progress *syncpkg.Progress  `json:"progress,omitempty"`
}

type sessionSyncInput struct {
	Body service.SyncInput
}

type remoteSyncInput struct {
	Body remoteSyncRequest
}

type remoteSyncRequest struct {
	Full         bool                `json:"full"`
	IncludeLocal bool                `json:"include_local"`
	Hosts        []config.RemoteHost `json:"hosts"`
}

type remoteSyncFailure struct {
	Host config.RemoteHost `json:"host"`
	Err  string            `json:"error"`
}

type remoteSyncResponse struct {
	LocalStats *syncpkg.SyncStats  `json:"local_stats,omitempty"`
	Failures   []remoteSyncFailure `json:"failures,omitempty"`
}

var runRemoteSync = func(
	ctx context.Context,
	rs *ssh.RemoteSync,
) (ssh.SyncStats, error) {
	return rs.Run(ctx)
}

func (s *Server) humaSyncStatus(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[syncStatusResponse], error) {
	engine := s.syncStatusEngine()
	if engine == nil {
		return &jsonOutput[syncStatusResponse]{Body: syncStatusResponse{}}, nil
	}
	lastSync := engine.LastSync()
	stats := engine.LastSyncStats()
	var lastSyncStr string
	if !lastSync.IsZero() {
		lastSyncStr = lastSync.Format(time.RFC3339)
	}
	var progress *syncpkg.Progress
	if p, ok := engine.CurrentProgress(); ok {
		progress = &p
	}
	return &jsonOutput[syncStatusResponse]{
		Body: syncStatusResponse{
			LastSync: lastSyncStr,
			Stats:    &stats,
			Progress: progress,
		},
	}, nil
}

func (s *Server) syncStatusEngine() *syncpkg.Engine {
	if s.engine != nil {
		return s.engine
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDemandEngine
}

func (s *Server) syncEngineForRequest() (*syncpkg.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return s.syncEngineForLocal(local), nil
}

func (s *Server) syncEngineForLocal(local *db.DB) *syncpkg.Engine {
	if s.engine != nil {
		return s.engine
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onDemandEngine != nil {
		return s.onDemandEngine
	}
	var emitter syncpkg.Emitter
	if s.broadcaster != nil {
		emitter = s.broadcaster
	}
	s.onDemandEngine = syncpkg.NewEngine(local, syncpkg.EngineConfig{
		AgentDirs:               s.cfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: s.cfg.ResultContentBlockedCategories,
		Emitter:                 emitter,
	})
	return s.onDemandEngine
}

func (s *Server) humaTriggerSync(
	ctx context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	engine, err := s.syncEngineForRequest()
	if err != nil {
		return nil, err
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats := s.runSyncWithResyncFallback(ctx, engine, nil)
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats := s.runSyncWithResyncFallback(ctx, engine, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) runSyncWithResyncFallback(
	ctx context.Context, engine *syncpkg.Engine,
	progress func(syncpkg.Progress),
) syncpkg.SyncStats {
	local, ok := s.db.(*db.DB)
	if ok && local.NeedsResync() {
		return s.runResyncWithFallback(ctx, engine, progress)
	}
	return engine.SyncAll(ctx, progress)
}

func (s *Server) humaTriggerResync(
	ctx context.Context,
	_ *emptyInput,
) (*huma.StreamResponse, error) {
	engine, err := s.syncEngineForRequest()
	if err != nil {
		return nil, err
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			stats := s.runResyncWithFallback(ctx, engine, nil)
			writeHumaJSON(hctx, http.StatusOK, stats)
			return
		}
		stats := s.runResyncWithFallback(ctx, engine, func(p syncpkg.Progress) {
			stream.SendJSON("progress", p)
		})
		stream.SendJSON("done", stats)
	}}, nil
}

func (s *Server) runResyncWithFallback(
	ctx context.Context, engine *syncpkg.Engine,
	progress func(syncpkg.Progress),
) syncpkg.SyncStats {
	stats := engine.ResyncAll(ctx, progress)
	if stats.Aborted && ctx.Err() == nil {
		return engine.SyncAll(ctx, progress)
	}
	return stats
}

func (s *Server) humaSyncRemotes(
	ctx context.Context,
	in *remoteSyncInput,
) (*huma.StreamResponse, error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	engine := s.syncEngineForLocal(local)
	if len(in.Body.Hosts) == 0 {
		return nil, apiError(http.StatusBadRequest, "at least one remote host is required")
	}
	hosts, err := s.authorizeRemoteSyncHosts(ctx, in.Body.Hosts)
	if err != nil {
		return nil, err
	}
	req := in.Body
	req.Hosts = hosts

	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		writeHumaJSON(hctx, http.StatusOK, s.runRemoteSyncRequest(ctx, local, engine, req))
	}}, nil
}

func (s *Server) authorizeRemoteSyncHosts(
	ctx context.Context,
	hosts []config.RemoteHost,
) ([]config.RemoteHost, error) {
	if err := (config.Config{RemoteHosts: hosts}).ValidateRemoteHosts(); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if isLocalhostContext(ctx) {
		return hosts, nil
	}

	allowed := make(map[config.RemoteHost]struct{}, len(s.cfg.RemoteHosts))
	for _, h := range s.cfg.RemoteHosts {
		allowed[h] = struct{}{}
	}
	for _, h := range hosts {
		if _, ok := allowed[h]; !ok {
			return nil, apiError(
				http.StatusForbidden,
				fmt.Sprintf(
					"remote host %q is not configured in remote_hosts",
					h.Host,
				),
			)
		}
	}
	return hosts, nil
}

func (s *Server) runRemoteSyncRequest(
	ctx context.Context,
	local *db.DB,
	engine *syncpkg.Engine,
	req remoteSyncRequest,
) remoteSyncResponse {
	var localStats *syncpkg.SyncStats
	failures := make([]remoteSyncFailure, 0)
	var remoteStats ssh.SyncStats
	run := func(full bool) {
		failures, remoteStats = s.runRemoteSyncHosts(
			ctx, local, req.Hosts, full,
		)
	}
	if req.IncludeLocal {
		stats, _ := engine.SyncThenRun(ctx, req.Full, nil,
			func(forceFull bool) error {
				run(forceFull)
				return nil
			})
		localStats = &stats
	} else {
		_ = engine.RunExclusive(func() error {
			run(req.Full)
			return nil
		})
	}
	s.emitRemoteSyncChanged(remoteStats)

	return remoteSyncResponse{
		LocalStats: localStats,
		Failures:   failures,
	}
}

func (s *Server) runRemoteSyncHosts(
	ctx context.Context,
	local *db.DB,
	hosts []config.RemoteHost,
	full bool,
) ([]remoteSyncFailure, ssh.SyncStats) {
	failures := make([]remoteSyncFailure, 0)
	var totals ssh.SyncStats
	for _, rh := range hosts {
		rs := &ssh.RemoteSync{
			Host:                    rh.Host,
			User:                    rh.User,
			Port:                    rh.Port,
			Full:                    full,
			DB:                      local,
			BlockedResultCategories: s.cfg.ResultContentBlockedCategories,
		}
		stats, err := runRemoteSync(ctx, rs)
		totals.SessionsSynced += stats.SessionsSynced
		totals.SessionsTotal += stats.SessionsTotal
		totals.Skipped += stats.Skipped
		totals.Failed += stats.Failed
		if err != nil {
			failures = append(failures, remoteSyncFailure{
				Host: rh,
				Err:  err.Error(),
			})
		}
	}
	return failures, totals
}

func (s *Server) emitRemoteSyncChanged(stats ssh.SyncStats) {
	if s.broadcaster == nil || stats.SessionsSynced == 0 {
		return
	}
	s.broadcaster.Emit("sessions")
}

func (s *Server) sessionSyncService() service.SessionService {
	if s.engine == nil {
		if local, ok := s.db.(*db.DB); ok {
			return service.NewDirectBackend(
				local, s.syncEngineForLocal(local),
			)
		}
	}
	return s.sessions
}

func (s *Server) humaSyncSession(
	ctx context.Context,
	in *sessionSyncInput,
) (*jsonOutput[*service.SessionDetail], error) {
	if (in.Body.Path == "" && in.Body.ID == "") ||
		(in.Body.Path != "" && in.Body.ID != "") {
		return nil, apiError(http.StatusBadRequest, "exactly one of 'path' or 'id' is required")
	}
	if err := s.resyncBeforeSessionSync(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	detail, err := s.sessionSyncService().Sync(ctx, in.Body)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			return nil, apiError(http.StatusConflict, err.Error())
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[*service.SessionDetail]{Body: detail}, nil
}

func (s *Server) resyncBeforeSessionSync(ctx context.Context) error {
	local, ok := s.db.(*db.DB)
	if !ok || !local.NeedsResync() {
		return nil
	}
	engine, err := s.syncEngineForRequest()
	if err != nil {
		return err
	}
	s.runResyncWithFallback(ctx, engine, nil)
	return ctx.Err()
}
