package server

import "github.com/danielgtaylor/huma/v2"

func (s *Server) registerTypedAPIRoutes() {
	s.api.UseMiddleware(humaRequestInfoMiddleware)

	s.registerHealthRoutes()
	s.registerSessionRoutes()
	s.registerOpenersRoutes()
	s.registerAnalyticsRoutes()
	s.registerActivityRoutes()
	s.registerRecentEditsRoutes()
	s.registerTrendsRoutes()
	s.registerUsageRoutes()
	s.registerInsightsRoutes()
	s.registerSearchRoutes()
	s.registerSecretsRoutes()
	s.registerMetadataRoutes()
	s.registerSyncRoutes()
	s.registerRemoteSyncRoutes()
	s.registerPushRoutes()
	s.registerConfigRoutes()
	s.registerSettingsRoutes()
	s.registerStarredRoutes()
	s.registerPinRoutes()
	s.registerImportRoutes()
	s.registerAssetRoutes()
	s.registerEmbeddingsRoutes()
}

type routeGroup struct {
	api    huma.API
	prefix string
}

func newRouteGroup(api huma.API, prefix string, tag string) routeGroup {
	group := huma.NewGroup(api, prefix)
	group.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{tag}
	})
	return routeGroup{
		api:    group,
		prefix: prefix,
	}
}

func (g routeGroup) fullPath(path string) string {
	return g.prefix + path
}
