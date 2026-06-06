package server

import "net/http"

func (s *Server) registerSearchRoutes() {
	group := newRouteGroup(s.api, "/api/v1/search", "Search")

	get(s, group, "", "Search sessions", s.humaSearch)
	get(s, group, "/content", "Search session content", s.humaSearchContent)
}

func (s *Server) registerSecretsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/secrets", "Secrets")

	get(s, group, "", "List secret findings", s.humaListSecrets)
	stream(s, group, http.MethodPost, "/scan", "Scan secrets", s.humaScanSecrets)
}
