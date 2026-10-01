package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger/spoolrun"
)

func (s *Server) registerLedgerSpoolRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/ledger/spool")
	configureRouteGroup(group, "Ledger")
	s.postLong(group, "/ingest", "Ingest jilog spool segments into the local ledger",
		s.humaLedgerSpoolIngest)
}

type ledgerSpoolIngestInput struct {
	Body struct {
		Zone string `json:"zone,omitempty" doc:"Only this [[ledger.zones]] id; default all zones"`
	}
}

func (s *Server) humaLedgerSpoolIngest(
	ctx context.Context, in *ledgerSpoolIngestInput,
) (*jsonOutput[spoolrun.LedgerSpoolIngestRun], error) {
	if !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden, "ledger spool ingest is only permitted from localhost")
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	s.mu.RLock()
	ledgerCfg, dataDir := s.cfg.Ledger, s.cfg.DataDir
	s.mu.RUnlock()
	zones, err := spoolrun.ZonesFromConfig(ledgerCfg, in.Body.Zone, dataDir)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	// Hold the engine's exclusive section, as PR 13's ledger-import job
	// does, so a resync cannot swap the archive mid-ingest.
	exclusive := func(work func() error) error { return work() }
	if s.engine != nil {
		exclusive = s.engine.RunExclusive
	}
	var run spoolrun.LedgerSpoolIngestRun
	err = exclusive(func() error {
		var ierr error
		run, ierr = spoolrun.IngestZones(ctx, zones, local)
		return ierr
	})
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[spoolrun.LedgerSpoolIngestRun]{Body: run}, nil
}
