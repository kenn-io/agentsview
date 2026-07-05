package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) registerSearchRoutes() {
	group := newRouteGroup(s.api, "/api/v1/search", "Search")

	get(s, group, "", "Search sessions", s.humaSearch)
	get(s, group, "/content", "Search session content", s.humaSearchContent)
}

type searchSort string

type contentSearchMode string

type searchInput struct {
	Query   string     `query:"q" required:"true" doc:"Search query"`
	Project string     `query:"project" doc:"Filter by project"`
	Sort    searchSort `query:"sort" enum:"relevance,recency" default:"relevance" doc:"Sort order"`
	Limit   int        `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor  int        `query:"cursor" minimum:"0" doc:"Pagination cursor"`
}

type contentSearchInput struct {
	Pattern          string            `query:"pattern" required:"true" doc:"Pattern to search for"`
	Mode             contentSearchMode `query:"mode" enum:"substring,regex,fts,semantic,hybrid" doc:"Search mode"`
	In               string            `query:"in" doc:"Comma-separated content sources"`
	ExcludeSystem    bool              `query:"exclude_system" doc:"Exclude system messages"`
	Reveal           bool              `query:"reveal" doc:"Return unredacted secret matches for localhost callers"`
	Project          string            `query:"project" doc:"Filter by project"`
	ExcludeProject   string            `query:"exclude_project" doc:"Exclude a project"`
	Machine          string            `query:"machine" doc:"Filter by machine"`
	GitBranch        string            `query:"git_branch" doc:"Filter by git branch; opaque (project, branch) tokens from the /branches endpoint"`
	Agent            string            `query:"agent" doc:"Filter by agent"`
	Date             string            `query:"date" format:"date" doc:"Filter to a single YYYY-MM-DD date"`
	DateFrom         string            `query:"date_from" format:"date" doc:"Filter start date"`
	DateTo           string            `query:"date_to" format:"date" doc:"Filter end date"`
	ActiveSince      string            `query:"active_since" format:"date-time" doc:"Filter sessions active since this RFC3339 timestamp"`
	IncludeChildren  bool              `query:"include_children" doc:"Include child sessions"`
	IncludeAutomated bool              `query:"include_automated" doc:"Include automated sessions"`
	IncludeOneShot   bool              `query:"include_one_shot" doc:"Include one-shot sessions"`
	Limit            int               `query:"limit" minimum:"0" doc:"Maximum number of results"`
	Cursor           int               `query:"cursor" minimum:"0" doc:"Pagination cursor"`
	Context          int               `query:"context" doc:"Include N messages of context before and after each match (max 10)"`
}

func (s *Server) humaSearch(
	ctx context.Context,
	in *searchInput,
) (*jsonOutput[searchResponse], error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, apiError(http.StatusBadRequest, "query required")
	}
	res, err := s.sessions.Search(ctx, service.SearchRequest{
		Query:   query,
		Project: in.Project,
		Sort:    string(in.Sort),
		Cursor:  in.Cursor,
		Limit:   in.Limit,
	})
	if err != nil {
		if errors.Is(err, service.ErrSearchUnavailable) {
			return nil, apiError(http.StatusNotImplemented, "search not available")
		}
		var inputErr *db.SearchInputError
		if errors.As(err, &inputErr) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, serverError(err)
	}
	return &jsonOutput[searchResponse]{
		Body: searchResponse{
			Query:   query,
			Results: res.Results,
			Count:   len(res.Results),
			Next:    res.NextCursor,
		},
	}, nil
}

func (s *Server) humaSearchContent(
	ctx context.Context,
	in *contentSearchInput,
) (*jsonOutput[*service.ContentSearchResult], error) {
	if in.Reveal && !isLocalhostContext(ctx) {
		return nil, apiError(http.StatusForbidden, "reveal is only permitted from localhost")
	}
	var sources []string
	if in.In != "" {
		sources = strings.Split(in.In, ",")
	}
	if err := validateDateFilterValues(in.Date, in.DateFrom, in.DateTo, in.ActiveSince); err != nil {
		return nil, err
	}
	res, err := s.sessions.SearchContent(ctx, service.ContentSearchRequest{
		Pattern:          in.Pattern,
		Mode:             string(in.Mode),
		Sources:          sources,
		ExcludeSystem:    in.ExcludeSystem,
		Reveal:           in.Reveal,
		Project:          in.Project,
		ExcludeProject:   in.ExcludeProject,
		Machine:          in.Machine,
		GitBranch:        in.GitBranch,
		Agent:            in.Agent,
		Date:             in.Date,
		DateFrom:         in.DateFrom,
		DateTo:           in.DateTo,
		ActiveSince:      in.ActiveSince,
		IncludeChildren:  in.IncludeChildren,
		IncludeAutomated: in.IncludeAutomated,
		IncludeOneShot:   in.IncludeOneShot,
		Limit:            in.Limit,
		Cursor:           in.Cursor,
		Context:          in.Context,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		if errors.Is(err, db.ErrSemanticUnavailable) {
			return nil, apiError(http.StatusNotImplemented, err.Error())
		}
		var inputErr *db.SearchInputError
		if errors.As(err, &inputErr) {
			return nil, apiError(http.StatusBadRequest, err.Error())
		}
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if res.Matches == nil {
		res.Matches = []db.ContentMatch{}
	}
	return &jsonOutput[*service.ContentSearchResult]{Body: res}, nil
}
