package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
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
	Error      string              `json:"error,omitempty"`
}

var runRemoteSync = func(
	ctx context.Context,
	rs *ssh.RemoteSync,
) (remotesync.SyncStats, error) {
	return rs.Run(ctx)
}

var runHTTPRemoteSync = func(
	ctx context.Context,
	cfg config.Config,
	local *db.DB,
	rh config.RemoteHost,
	full bool,
	progress func(syncpkg.Progress),
) (remotesync.SyncStats, error) {
	token := rh.Token
	if token == "" {
		return remotesync.SyncStats{}, fmt.Errorf(
			"http remote sync token is required for host %q",
			rh.Host,
		)
	}
	return remotesync.HTTPSync{
		Host:                    rh.Host,
		URL:                     rh.URL,
		Token:                   token,
		Full:                    full,
		DataDir:                 cfg.DataDir,
		DB:                      local,
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
		Progress:                progress,
	}.Run(ctx)
}

type preparedHTTPRebuild interface {
	BorrowRebuildContributors() ([]syncpkg.RebuildContributor, func(), error)
	Close() error
}

var prepareHTTPRebuild = func(
	ctx context.Context,
	syncs []remotesync.HTTPSync,
) (preparedHTTPRebuild, error) {
	return remotesync.PrepareHTTPSyncs(ctx, syncs)
}

type preparedHTTPRebuildLease struct {
	prepared preparedHTTPRebuild
	release  func()
}

func (l *preparedHTTPRebuildLease) Close() error {
	if l == nil {
		return nil
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
	return l.prepared.Close()
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
		SourceMachines:          s.cfg.SourceMachines,
		IncludeCwdPrefixes:      s.cfg.SyncIncludeCwdPrefixes,
		Machine:                 s.cfg.LocalMachineName,
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
	stats, _ := engine.SyncThenRun(
		ctx, false, progress, func(bool) error { return nil },
	)
	return stats
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
	stats, _ := engine.SyncThenRun(
		ctx, true, progress, func(bool) error { return nil },
	)
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
	hosts, err := s.resolveRemoteSyncHosts(ctx, in.Body.Hosts)
	if err != nil {
		return nil, err
	}
	req := in.Body
	req.Hosts = hosts

	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		if strings.Contains(hctx.Header("Accept"), "text/event-stream") {
			stream, ok := newHumaSSEStream(hctx)
			if ok {
				response := s.runRemoteSyncRequest(
					ctx, local, engine, req,
					func(progress syncpkg.Progress) {
						stream.SendJSON("progress", progress)
					},
				)
				stream.SendJSON("done", response)
				return
			}
		}
		writeHumaJSON(
			hctx, http.StatusOK,
			s.runRemoteSyncRequest(ctx, local, engine, req, nil),
		)
	}}, nil
}

func (s *Server) resolveRemoteSyncHosts(
	ctx context.Context,
	hosts []config.RemoteHost,
) ([]config.RemoteHost, error) {
	if len(hosts) == 0 {
		return nil, apiError(http.StatusBadRequest, "at least one remote host is required")
	}
	configured := make(map[remoteHostIdentity]config.RemoteHost, len(s.cfg.RemoteHosts))
	for _, h := range s.cfg.RemoteHosts {
		configured[remoteHostIdentityFrom(h)] = h
	}
	resolved := make([]config.RemoteHost, 0, len(hosts))
	for _, h := range hosts {
		if stored, ok := configured[remoteHostIdentityFrom(h)]; ok {
			resolved = append(resolved, stored)
			continue
		}
		if !isLocalhostContext(ctx) {
			return nil, apiError(
				http.StatusForbidden,
				fmt.Sprintf(
					"remote host %q is not configured in remote_hosts",
					h.Host,
				),
			)
		}
		if h.Transport == config.RemoteTransportHTTP || h.URL != "" || h.Token != "" {
			return nil, apiError(
				http.StatusForbidden,
				"ad hoc HTTP remote sync requires a configured remote_hosts entry",
			)
		}
		resolved = append(resolved, h)
	}
	if err := (config.Config{RemoteHosts: resolved}).ValidateRemoteHosts(); err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	return resolved, nil
}

type remoteHostIdentity struct {
	Host string
	User string
	Port int
}

func remoteHostIdentityFrom(h config.RemoteHost) remoteHostIdentity {
	return remoteHostIdentity{
		Host: h.Host,
		User: h.User,
		Port: h.Port,
	}
}

func (s *Server) runRemoteSyncRequest(
	ctx context.Context,
	local *db.DB,
	engine *syncpkg.Engine,
	req remoteSyncRequest,
	progress func(syncpkg.Progress),
) remoteSyncResponse {
	var localStats *syncpkg.SyncStats
	failures := make([]remoteSyncFailure, 0)
	var remoteStats remotesync.SyncStats
	var blocked error
	if req.IncludeLocal {
		httpHosts, sshHosts := partitionRemoteHosts(req.Hosts)
		outerOwnsHTTP := len(httpHosts) > 0
		coordinatedRun := func() (remotesync.SyncStats, error) {
			if !outerOwnsHTTP {
				stats, err := engine.SyncThenRun(
					ctx, req.Full, progress, func(forceFull bool) error {
						failures, remoteStats, blocked = s.runRemoteSyncHostsOwned(
							ctx, local, req.Hosts, forceFull, progress, true,
						)
						return blocked
					},
				)
				localStats = &stats
				return remotesync.SyncStats{}, err
			}
			stats, err := engine.SyncThenRunWithRebuild(
				ctx, req.Full, progress,
				func() (
					syncpkg.RebuildOptions,
					syncpkg.RebuildCleanup,
					error,
				) {
					prepared, err := prepareHTTPHosts(
						ctx, s.cfg, local, httpHosts, progress,
					)
					if err != nil {
						return syncpkg.RebuildOptions{}, prepared, err
					}
					if prepared == nil {
						return syncpkg.RebuildOptions{}, nil, nil
					}
					contributors, release, err := prepared.BorrowRebuildContributors()
					if err != nil {
						return syncpkg.RebuildOptions{}, prepared, err
					}
					return syncpkg.RebuildOptions{Contributors: contributors},
						&preparedHTTPRebuildLease{
							prepared: prepared,
							release:  release,
						}, nil
				},
				func(forceFull, rebuilt bool) error {
					hosts := req.Hosts
					if rebuilt {
						hosts = sshHosts
					}
					failures, remoteStats, blocked = s.runRemoteSyncHostsOwned(
						ctx, local, hosts, forceFull, progress, !outerOwnsHTTP,
					)
					return blocked
				},
			)
			localStats = &stats
			return remotesync.SyncStats{}, err
		}
		var coordinatorErr error
		if outerOwnsHTTP {
			_, coordinatorErr = s.httpRemoteCleanupRegistry.Run(coordinatedRun)
		} else {
			_, coordinatorErr = coordinatedRun()
		}
		if coordinatorErr != nil {
			if failure, ok := httpCoordinatorFailure(httpHosts, coordinatorErr); ok {
				log.Printf("remote sync %s: %v", failure.Host.Host, coordinatorErr)
				failures = append(failures, failure)
				blocked = nil
			} else {
				blocked = coordinatorErr
			}
		} else if localStats != nil && localStats.Aborted {
			if ctxErr := ctx.Err(); ctxErr != nil {
				blocked = ctxErr
			} else {
				blocked = syncpkg.ErrUnifiedRebuildAborted
			}
		}
	} else {
		httpHosts, _ := partitionRemoteHosts(req.Hosts)
		outerOwnsHTTP := len(httpHosts) > 0
		exclusiveRun := func() (remotesync.SyncStats, error) {
			err := engine.RunExclusive(func() error {
				failures, remoteStats, blocked = s.runRemoteSyncHostsOwned(
					ctx, local, req.Hosts, req.Full, progress, !outerOwnsHTTP,
				)
				return blocked
			})
			return remotesync.SyncStats{}, err
		}
		var coordinatorErr error
		if outerOwnsHTTP {
			_, coordinatorErr = s.httpRemoteCleanupRegistry.Run(exclusiveRun)
		} else {
			_, coordinatorErr = exclusiveRun()
		}
		if coordinatorErr != nil {
			if failure, ok := httpCoordinatorFailure(httpHosts, coordinatorErr); ok {
				log.Printf("remote sync %s: %v", failure.Host.Host, coordinatorErr)
				failures = append(failures, failure)
				blocked = nil
			} else {
				blocked = coordinatorErr
			}
		}
	}
	s.emitRemoteSyncChanged(remoteStats)

	return remoteSyncResponse{
		LocalStats: localStats,
		Failures:   failures,
		Error:      remoteSyncTopLevelError(blocked),
	}
}

func remoteSyncTopLevelError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded.Error()
	}
	if errors.Is(err, syncpkg.ErrUnifiedRebuildAborted) {
		return syncpkg.ErrUnifiedRebuildAborted.Error()
	}
	if isHTTPRemoteCoordinatorError(err) {
		return remotesync.FailureSummary(err)
	}
	return "local sync failed"
}

func partitionRemoteHosts(
	hosts []config.RemoteHost,
) (httpHosts, sshHosts []config.RemoteHost) {
	for _, host := range hosts {
		if host.Transport == config.RemoteTransportHTTP {
			httpHosts = append(httpHosts, host)
		} else {
			sshHosts = append(sshHosts, host)
		}
	}
	return httpHosts, sshHosts
}

func prepareHTTPHosts(
	ctx context.Context,
	cfg config.Config,
	local *db.DB,
	hosts []config.RemoteHost,
	progress func(syncpkg.Progress),
) (preparedHTTPRebuild, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	syncs := make([]remotesync.HTTPSync, 0, len(hosts))
	for _, host := range hosts {
		if host.Token == "" {
			return nil, &remotesync.HostError{
				Host:      host.Host,
				Operation: "authenticate",
				Err:       errors.New("HTTP remote sync token is required"),
			}
		}
		syncs = append(syncs, remotesync.HTTPSync{
			Host:                    host.Host,
			URL:                     host.URL,
			Token:                   host.Token,
			Full:                    true,
			DataDir:                 cfg.DataDir,
			DB:                      local,
			BlockedResultCategories: cfg.ResultContentBlockedCategories,
			Progress:                progress,
		})
	}
	return prepareHTTPRebuild(ctx, syncs)
}

func httpCoordinatorFailure(
	hosts []config.RemoteHost,
	err error,
) (remoteSyncFailure, bool) {
	var pending *remotesync.PendingCleanupError
	if errors.As(err, &pending) {
		return remoteSyncFailure{}, false
	}
	primary := primaryRemoteCoordinatorError(err)
	var hostName string
	summaryErr := primary
	var contributorErr *syncpkg.RebuildContributorError
	if errors.As(primary, &contributorErr) {
		hostName = contributorErr.Contributor
		summaryErr = contributorErr.Err
	} else {
		var hostErr *remotesync.HostError
		if errors.As(primary, &hostErr) {
			hostName = hostErr.Host
		}
	}
	for _, host := range hosts {
		if host.Host == hostName {
			return remoteSyncFailure{
				Host: remoteSyncFailureHost(host),
				Err:  remotesync.FailureSummary(summaryErr),
			}, true
		}
	}
	return remoteSyncFailure{}, false
}

func primaryRemoteCoordinatorError(err error) error {
	for err != nil {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			var first error
			for _, child := range joined.Unwrap() {
				if child != nil {
					first = child
					break
				}
			}
			if first == nil {
				return err
			}
			err = first
			continue
		}
		switch err.(type) {
		case *syncpkg.RebuildContributorError, *remotesync.HostError:
			return err
		}
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			return err
		}
		err = unwrapped
	}
	return nil
}

func isHTTPRemoteCoordinatorError(err error) bool {
	if err == nil {
		return false
	}
	var pending *remotesync.PendingCleanupError
	if errors.As(err, &pending) {
		return true
	}
	primary := primaryRemoteCoordinatorError(err)
	var contributor *syncpkg.RebuildContributorError
	if errors.As(primary, &contributor) {
		return true
	}
	var host *remotesync.HostError
	return errors.As(primary, &host)
}

func (s *Server) runRemoteSyncHosts(
	ctx context.Context,
	local *db.DB,
	hosts []config.RemoteHost,
	full bool,
	progress func(syncpkg.Progress),
) ([]remoteSyncFailure, remotesync.SyncStats, error) {
	return s.runRemoteSyncHostsOwned(
		ctx, local, hosts, full, progress, true,
	)
}

type httpCleanupRetrier interface {
	RetryCleanup() error
}

// runRemoteSyncHostsOwned optionally acquires the server's HTTP cleanup
// registry per host. A false value is only valid while the caller already
// owns that registry around the entire operation; retryable cleanup errors are
// then returned immediately so the outer owner can retain them.
func (s *Server) runRemoteSyncHostsOwned(
	ctx context.Context,
	local *db.DB,
	hosts []config.RemoteHost,
	full bool,
	progress func(syncpkg.Progress),
	acquireHTTPRegistry bool,
) ([]remoteSyncFailure, remotesync.SyncStats, error) {
	failures := make([]remoteSyncFailure, 0)
	var totals remotesync.SyncStats
	for _, rh := range hosts {
		var stats remotesync.SyncStats
		var err error
		switch rh.Transport {
		case "", config.RemoteTransportSSH:
			rs := &ssh.RemoteSync{
				Host:                    rh.Host,
				User:                    rh.User,
				Port:                    rh.Port,
				Full:                    full,
				DB:                      local,
				BlockedResultCategories: s.cfg.ResultContentBlockedCategories,
				Progress:                progress,
			}
			stats, err = runRemoteSync(ctx, rs)
		case config.RemoteTransportHTTP:
			runHTTP := func() (remotesync.SyncStats, error) {
				return runHTTPRemoteSync(
					ctx, s.cfg, local, rh, full, progress,
				)
			}
			if acquireHTTPRegistry {
				stats, err = s.httpRemoteCleanupRegistry.Run(runHTTP)
			} else {
				stats, err = runHTTP()
			}
		default:
			err = fmt.Errorf("invalid remote transport %q", rh.Transport)
		}
		totals.SessionsSynced += stats.SessionsSynced
		totals.SessionsTotal += stats.SessionsTotal
		totals.Skipped += stats.Skipped
		totals.Failed += stats.Failed
		if err != nil {
			var pending *remotesync.PendingCleanupError
			if errors.As(err, &pending) {
				return failures, totals, pending
			}
			// The raw error can embed the remote URL and response
			// bodies, so it goes only to the local log; the API
			// response carries the sanitized summary.
			log.Printf("remote sync %s: %v", rh.Host, err)
			if !acquireHTTPRegistry &&
				rh.Transport == config.RemoteTransportHTTP {
				var cleanup httpCleanupRetrier
				if errors.As(err, &cleanup) {
					return failures, totals, &remotesync.HostError{
						Host: rh.Host, Operation: "sync", Err: err,
					}
				}
			}
			failures = append(failures, remoteSyncFailure{
				Host: remoteSyncFailureHost(rh),
				Err:  remoteSyncFailureError(rh, err),
			})
		}
	}
	return failures, totals, nil
}

func remoteSyncFailureHost(rh config.RemoteHost) config.RemoteHost {
	return config.RemoteHost{
		Host: rh.Host,
		User: rh.User,
		Port: rh.Port,
	}
}

func remoteSyncFailureError(rh config.RemoteHost, err error) string {
	if rh.Transport == config.RemoteTransportHTTP {
		return remotesync.FailureSummary(err)
	}
	return err.Error()
}

func (s *Server) emitRemoteSyncChanged(stats remotesync.SyncStats) {
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
