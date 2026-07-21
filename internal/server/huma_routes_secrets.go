package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) registerSecretsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/secrets", "Secrets")

	get(s, group, "", "List secret findings", s.humaListSecrets)
	stream(s, group, http.MethodPost, "/scan", "Scan secrets", s.humaScanSecrets)
}

type secretListInput struct {
	Project    string `query:"project" doc:"Filter by project"`
	Agent      string `query:"agent" doc:"Filter by agent"`
	DateFrom   string `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo     string `query:"date_to" format:"date" doc:"Filter end date"`
	Rule       string `query:"rule" doc:"Filter by secret rule"`
	Confidence string `query:"confidence" doc:"Filter by confidence"`
	Reveal     bool   `query:"reveal" doc:"Return unredacted matches for localhost callers"`
	Limit      int    `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor     int    `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

func (s *Server) humaListSecrets(
	ctx context.Context,
	in *secretListInput,
) (*jsonOutput[*service.SecretFindingList], error) {
	if in.Reveal && !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden, "reveal is only permitted from localhost")
	}
	if err := validateDateFilterValues("", in.DateFrom, in.DateTo, ""); err != nil {
		return nil, err
	}
	res, err := s.sessions.ListSecrets(ctx, service.SecretListFilter{
		Project:    in.Project,
		Agent:      in.Agent,
		DateFrom:   in.DateFrom,
		DateTo:     in.DateTo,
		Rule:       in.Rule,
		Confidence: in.Confidence,
		Reveal:     in.Reveal,
		Limit:      in.Limit,
		Cursor:     in.Cursor,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if res.Findings == nil {
		res.Findings = []db.SecretFindingRow{}
	}
	return &jsonOutput[*service.SecretFindingList]{Body: res}, nil
}

type scanSecretsInput struct {
	Backfill bool   `query:"backfill" doc:"Backfill all matching sessions"`
	Project  string `query:"project" doc:"Filter by project"`
	Agent    string `query:"agent" doc:"Filter by agent"`
	DateFrom string `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo   string `query:"date_to" format:"date" doc:"Filter end date"`
}

func (s *Server) humaScanSecrets(
	ctx context.Context,
	in *scanSecretsInput,
) (*huma.StreamResponse, error) {
	if err := validateDateFilterValues("", in.DateFrom, in.DateTo, ""); err != nil {
		return nil, err
	}
	if s.engine == nil {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiErrorResponse{Message: "streaming not supported"})
			return
		}
		summary, err := s.sessions.ScanSecrets(ctx, service.SecretScanInput{
			Backfill: in.Backfill,
			Project:  in.Project,
			Agent:    in.Agent,
			DateFrom: in.DateFrom,
			DateTo:   in.DateTo,
		}, func(p service.SecretScanProgress) {
			stream.SendJSON("progress", p)
		})
		if err != nil {
			// The scan commits per-session results as it walks, so a
			// failure or cancellation partway through has already
			// changed eligibility for everything it scanned.
			if summary != nil && summary.Scanned > 0 {
				s.notifySessionMutation()
			}
			stream.SendJSON("error", map[string]string{"error": err.Error()})
			return
		}
		// A completed scan changes extraction eligibility in both
		// directions — new findings retract generated entries, fresh
		// clean stamps make sessions extractable — and no sync activity
		// follows a delegated scan to surface either.
		s.notifySessionMutation()
		stream.SendJSON("summary", summary)
	}}, nil
}
