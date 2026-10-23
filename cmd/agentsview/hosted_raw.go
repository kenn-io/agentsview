package main

import (
	"context"
	"sort"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func unsupportedRawSyncProviders(cfg config.Config) []string {
	var providers []string
	for _, factory := range cfg.LocalProviderFactories() {
		if factory.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			providers = append(providers, string(factory.Definition().Type))
		}
	}
	sort.Strings(providers)
	return providers
}

func openServeDBWith(ctx context.Context, cfg config.Config, openArchive func(context.Context, config.Config) (*db.DB, error)) (*db.DB, *writeOwnerLock, error) {
	if cfg.HostedRawMode() {
		database, err := openReadOnlyDB(ctx, cfg)
		return database, nil, err
	}
	return openWriteDBWith(ctx, cfg, openArchive)
}
