package server

func (s *Server) registerAnalyticsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/analytics", "Analytics")

	get(s, group, "/summary", "Get analytics summary", s.humaAnalyticsSummary)
	get(s, group, "/activity", "Get analytics activity", s.humaAnalyticsActivity)
	get(s, group, "/heatmap", "Get analytics heatmap", s.humaAnalyticsHeatmap)
	get(s, group, "/projects", "Get analytics by project", s.humaAnalyticsProjects)
	get(s, group, "/hour-of-week", "Get analytics by hour of week", s.humaAnalyticsHourOfWeek)
	get(s, group, "/sessions", "Get session shape analytics", s.humaAnalyticsSessionShape)
	get(s, group, "/velocity", "Get velocity analytics", s.humaAnalyticsVelocity)
	get(s, group, "/tools", "Get tool analytics", s.humaAnalyticsTools)
	get(s, group, "/top-sessions", "Get top sessions", s.humaAnalyticsTopSessions)
	get(s, group, "/signals", "Get signal analytics", s.humaAnalyticsSignals)
}

func (s *Server) registerTrendsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/trends", "Trends")

	get(s, group, "/terms", "Get trend terms", s.humaTrendsTerms)
}

func (s *Server) registerUsageRoutes() {
	group := newRouteGroup(s.api, "/api/v1/usage", "Usage")

	get(s, group, "/summary", "Get usage summary", s.humaUsageSummary)
	get(s, group, "/top-sessions", "Get top usage sessions", s.humaUsageTopSessions)
}
