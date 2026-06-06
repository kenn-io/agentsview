package server

import "net/http"

func (s *Server) registerSessionRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Sessions")

	get(s, group, "/sessions", "List sessions", s.humaListSessions)
	get(s, group, "/sessions/sidebar-index", "List sidebar sessions", s.humaSidebarSessionIndex)
	get(s, group, "/sessions/{id}", "Get session", s.humaGetSession)
	get(s, group, "/sessions/{id}/messages", "List session messages", s.humaGetMessages)
	get(s, group, "/sessions/{id}/tool-calls", "List session tool calls", s.humaToolCalls)
	get(s, group, "/sessions/{id}/children", "List child sessions", s.humaGetChildSessions)
	get(s, group, "/sessions/{id}/activity", "Get session activity", s.humaGetSessionActivity)
	get(s, group, "/sessions/{id}/timing", "Get session timing", s.humaSessionTiming)
	get(s, group, "/sessions/{id}/usage", "Get session usage", s.humaSessionUsage)
	stream(s, group, http.MethodGet, "/sessions/{id}/watch", "Watch session events", s.humaWatchSession)
	stream(s, group, http.MethodGet, "/events", "Watch server events", s.humaEvents)
	raw(s, group, http.MethodGet, "/sessions/{id}/export", "Export session as HTML", s.humaExportSession)
	raw(s, group, http.MethodGet, "/sessions/{id}/md", "Export session as Markdown", s.humaMarkdownSession)
	post(s, group, "/sessions/{id}/publish", "Publish session", s.humaPublishSession)
	post(s, group, "/sessions/{id}/resume", "Resume session", s.humaResumeSession)
	get(s, group, "/sessions/{id}/directory", "Get session directory", s.humaGetSessionDir)
	get(s, group, "/sessions/{id}/search", "Search within a session", s.humaSearchSession)
	post(s, group, "/sessions/{id}/open", "Open session directory", s.humaOpenSession)
	post(s, group, "/sessions/upload", "Upload a session export", s.humaUploadSession)
	patch(s, group, "/sessions/{id}/rename", "Rename session", s.humaRenameSession)
	deleteRoute(s, group, "/sessions/{id}", "Delete session", s.humaDeleteSession)
	post(s, group, "/sessions/{id}/restore", "Restore session", s.humaRestoreSession)
	deleteRoute(s, group, "/sessions/{id}/permanent", "Permanently delete session", s.humaPermanentDeleteSession)
	get(s, group, "/trash", "List trash", s.humaListTrash)
	deleteRoute(s, group, "/trash", "Empty trash", s.humaEmptyTrash)
}

func (s *Server) registerOpenersRoutes() {
	group := newRouteGroup(s.api, "/api/v1/openers", "Openers")

	get(s, group, "", "List openers", s.humaListOpeners)
}
