package server

import "net/http"

func (s *Server) registerImportRoutes() {
	group := newRouteGroup(s.api, "/api/v1/import", "Import")

	stream(s, group, http.MethodPost, "/claude-ai", "Import Claude.ai archive", s.humaImportClaudeAI)
	stream(s, group, http.MethodPost, "/chatgpt", "Import ChatGPT archive", s.humaImportChatGPT)
}

func (s *Server) registerAssetRoutes() {
	group := newRouteGroup(s.api, "/api/v1/assets", "Assets")

	raw(s, group, http.MethodGet, "/{filename}", "Get imported asset", s.humaGetAsset)
}
