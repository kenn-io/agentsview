package server

import "net/http"

func (s *Server) registerInsightsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/insights", "Insights")

	get(s, group, "", "List insights", s.humaListInsights)
	get(s, group, "/{id}", "Get insight", s.humaGetInsight)
	deleteRoute(s, group, "/{id}", "Delete insight", s.humaDeleteInsight)
	stream(s, group, http.MethodPost, "/generate", "Generate insight", s.humaGenerateInsight)
}
