package server

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

type recentEditsInput struct {
	Limit   int    `query:"limit" minimum:"1" maximum:"200" default:"50" doc:"Max files per page"`
	Offset  int    `query:"offset" minimum:"0" default:"0" doc:"Files to skip"`
	Project string `query:"project" doc:"Filter by project"`
	Search  string `query:"search" doc:"Filter by file path substring (case-insensitive)"`
}

func (s *Server) registerRecentEditsRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "RecentEdits")
	get(s, group, "/recent-edits", "List recent edits", s.humaRecentEdits)
}

func (s *Server) humaRecentEdits(
	ctx context.Context, in *recentEditsInput,
) (*jsonOutput[db.RecentEditsResult], error) {
	res, err := s.db.RecentEdits(ctx, db.RecentEditsParams{
		Project:         in.Project,
		Search:          in.Search,
		Limit:           in.Limit,
		Offset:          in.Offset,
		MaxEditsPerFile: 20,
	})
	if err != nil {
		return nil, serverError(err)
	}
	return &jsonOutput[db.RecentEditsResult]{Body: res}, nil
}
