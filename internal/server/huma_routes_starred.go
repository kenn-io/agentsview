package server

func (s *Server) registerStarredRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Starred")

	get(s, group, "/starred", "List starred sessions", s.humaListStarred)
	put(s, group, "/sessions/{id}/star", "Star session", s.humaStarSession)
	deleteRoute(s, group, "/sessions/{id}/star", "Unstar session", s.humaUnstarSession)
	post(s, group, "/starred/bulk", "Bulk star sessions", s.humaBulkStar)
}
