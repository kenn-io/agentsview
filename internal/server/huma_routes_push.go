package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/postgres"
)

func (s *Server) registerPushRoutes() {
	group := newRouteGroup(s.api, "/api/v1/push", "Push")

	registerRoute(
		group, http.MethodPost, "/pg",
		"Push to PostgreSQL", s.humaPGPush,
	)
	registerRoute(
		group, http.MethodPost, "/duckdb",
		"Push to DuckDB", s.humaDuckDBPush,
	)
}

type daemonPushInput struct {
	Body daemonPushRequest
}

type daemonPushRequest struct {
	Full            bool                 `json:"full"`
	Projects        []string             `json:"projects,omitempty"`
	ExcludeProjects []string             `json:"exclude_projects,omitempty"`
	PG              *config.PGConfig     `json:"pg,omitempty"`
	DuckDB          *config.DuckDBConfig `json:"duckdb,omitempty"`
}

type pgPushOutput struct {
	Body postgres.PushResult
}

type duckDBPushOutput struct {
	Body duckdbsync.PushResult
}

func (s *Server) localPushTarget() (*db.DB, error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(
			http.StatusNotImplemented,
			"not available in remote mode",
		)
	}
	return local, nil
}

func (s *Server) pgPushConfig(req daemonPushRequest) (config.PGConfig, error) {
	if req.PG != nil {
		return *req.PG, nil
	}
	return s.cfg.ResolvePG()
}

func (s *Server) duckDBPushConfig(
	req daemonPushRequest,
) (config.DuckDBConfig, error) {
	if req.DuckDB != nil {
		return *req.DuckDB, nil
	}
	return s.cfg.ResolveDuckDB()
}

func (s *Server) humaPGPush(
	ctx context.Context,
	in *daemonPushInput,
) (*pgPushOutput, error) {
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	pgCfg, err := s.pgPushConfig(in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	if pgCfg.URL == "" {
		return nil, apiError(http.StatusBadRequest, "pg push: url not configured")
	}

	engine := s.syncEngineForLocal(local)
	var result postgres.PushResult
	_, err = engine.SyncThenRun(ctx, in.Body.Full, nil,
		func(forceFull bool) error {
			ps, err := postgres.New(
				pgCfg.URL, pgCfg.Schema, local,
				pgCfg.MachineName, pgCfg.AllowInsecure,
				postgres.SyncOptions{
					Projects:        in.Body.Projects,
					ExcludeProjects: in.Body.ExcludeProjects,
				},
			)
			if err != nil {
				return err
			}
			defer ps.Close()
			if err := ps.EnsureSchema(ctx); err != nil {
				return err
			}
			result, err = ps.Push(ctx, forceFull, nil)
			return err
		})
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &pgPushOutput{Body: result}, nil
}

func (s *Server) humaDuckDBPush(
	ctx context.Context,
	in *daemonPushInput,
) (*duckDBPushOutput, error) {
	local, err := s.localPushTarget()
	if err != nil {
		return nil, err
	}
	duckCfg, err := s.duckDBPushConfig(in.Body)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}

	engine := s.syncEngineForLocal(local)
	var result duckdbsync.PushResult
	_, err = engine.SyncThenRun(ctx, in.Body.Full, nil,
		func(forceFull bool) error {
			syncer, err := duckdbsync.New(
				duckCfg.Path, local, duckCfg.MachineName,
				duckdbsync.SyncOptions{
					Projects:        in.Body.Projects,
					ExcludeProjects: in.Body.ExcludeProjects,
				},
			)
			if err != nil {
				return err
			}
			defer syncer.Close()
			if err := syncer.EnsureSchema(ctx); err != nil {
				return err
			}
			result, err = syncer.Push(ctx, forceFull, nil)
			return err
		})
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &duckDBPushOutput{Body: result}, nil
}
