package server

import "github.com/danielgtaylor/huma/v2"

func (s *Server) registerTypedAPIRoutes() {
	s.api.UseMiddleware(humaRequestInfoMiddleware)

	s.registerHealthRoutes()
	s.registerSessionRoutes()
	s.registerOpenersRoutes()
	s.registerAnalyticsRoutes()
	s.registerActivityRoutes()
	s.registerDataRoutes()
	s.registerRecentEditsRoutes()
	s.registerTrendsRoutes()
	s.registerUsageRoutes()
	s.registerInsightsRoutes()
	s.registerSearchRoutes()
	s.registerRecallRoutes()
	s.describeTransferRoutes()
	s.describeStartupProbe()
	s.registerSecretsRoutes()
	s.registerMetadataRoutes()
	s.registerSyncRoutes()
	s.registerRemoteSyncRoutes()
	s.registerRawSyncRoutes()
	s.registerPushRoutes()
	s.registerConfigRoutes()
	s.registerSettingsRoutes()
	s.registerStarredRoutes()
	s.registerPinRoutes()
	s.registerImportRoutes()
	s.registerAssetRoutes()
	s.registerEmbeddingsRoutes()
}

func configureRouteGroup(group *huma.Group, tag string) {
	group.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{tag}
		op.OperationID = operationID(op.Method, op.Path)
	})
}
