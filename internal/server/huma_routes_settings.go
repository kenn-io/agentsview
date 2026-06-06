package server

func (s *Server) registerSettingsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/settings", "Settings")

	get(s, group, "", "Get settings", s.humaGetSettings)
	put(s, group, "", "Update settings", s.humaUpdateSettings)
	get(s, group, "/worktree-mappings", "List worktree mappings", s.humaListWorktreeMappings)
	post(s, group, "/worktree-mappings", "Create worktree mapping", s.humaCreateWorktreeMapping)
	put(s, group, "/worktree-mappings/{id}", "Update worktree mapping", s.humaUpdateWorktreeMapping)
	deleteRoute(s, group, "/worktree-mappings/{id}", "Delete worktree mapping", s.humaDeleteWorktreeMapping)
	post(s, group, "/worktree-mappings/apply", "Apply worktree mappings", s.humaApplyWorktreeMappings)
}
