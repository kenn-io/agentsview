package server

func (s *Server) registerPinRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Pins")

	get(s, group, "/pins", "List pins", s.humaListPins)
	get(s, group, "/sessions/{id}/pins", "List session pins", s.humaListSessionPins)
	post(s, group, "/sessions/{id}/messages/{messageId}/pin", "Pin message", s.humaPinMessage)
	deleteRoute(s, group, "/sessions/{id}/messages/{messageId}/pin", "Unpin message", s.humaUnpinMessage)
}
