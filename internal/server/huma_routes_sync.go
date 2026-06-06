package server

import "net/http"

func (s *Server) registerSyncRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Sync")

	stream(s, group, http.MethodPost, "/sync", "Trigger sync", s.humaTriggerSync)
	stream(s, group, http.MethodPost, "/resync", "Trigger full resync", s.humaTriggerResync)
	get(s, group, "/sync/status", "Get sync status", s.humaSyncStatus)
	post(s, group, "/sessions/sync", "Sync a session", s.humaSyncSession)
}
