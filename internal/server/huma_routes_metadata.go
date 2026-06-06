package server

func (s *Server) registerHealthRoutes() {
	group := newRouteGroup(s.api, "/api", "Health")

	get(s, group, "/ping", "Ping daemon", s.humaPing)
}

func (s *Server) registerMetadataRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Metadata")

	get(s, group, "/projects", "List projects", s.humaListProjects)
	get(s, group, "/machines", "List machines", s.humaListMachines)
	get(s, group, "/agents", "List agents", s.humaListAgents)
	get(s, group, "/stats", "Get stats", s.humaGetStats)
	get(s, group, "/version", "Get server version", s.humaGetVersion)
	get(s, group, "/update/check", "Check for updates", s.humaCheckUpdate)
}
