package server

func (s *Server) registerConfigRoutes() {
	group := newRouteGroup(s.api, "/api/v1/config", "Config")

	get(s, group, "/github", "Get GitHub config", s.humaGetGithubConfig)
	post(s, group, "/github", "Set GitHub config", s.humaSetGithubConfig)
	get(s, group, "/terminal", "Get terminal config", s.humaGetTerminalConfig)
	post(s, group, "/terminal", "Set terminal config", s.humaSetTerminalConfig)
}
