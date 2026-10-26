package main

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// hubSearcherAdapter resolves the active saved recipe on every query, so a
// pending pull or changed local config cannot select another embedding space.
type hubSearcherAdapter struct {
	searcherAdapter
	local config.VectorEmbeddingsConfig
}

func localSemanticUnavailable(reason string) error {
	return fmt.Errorf("%w: semantic search unavailable locally: %s", db.ErrSemanticUnavailable, reason)
}

func (a hubSearcherAdapter) active(ctx context.Context) (searcherAdapter, error) {
	if a.ix == nil {
		return searcherAdapter{}, localSemanticUnavailable("no active hub generation")
	}
	g, ok, err := a.ix.ActiveHubRecipe(ctx)
	if err != nil || !ok {
		return searcherAdapter{}, localSemanticUnavailable("no supported active hub recipe")
	}
	pending, err := a.ix.ActiveFullRebuildPending(ctx, g.Fingerprint())
	if err != nil || pending {
		return searcherAdapter{}, localSemanticUnavailable("hub rebuild is incomplete")
	}
	recipe, ok := decodeReplicaRecipe(storage.VectorGenerationInfo{Fingerprint: g.Fingerprint(), Model: g.Model, Dimension: g.Dimensions, Params: g.Params})
	if !ok {
		return searcherAdapter{}, localSemanticUnavailable("unsupported active hub recipe")
	}
	recipe.Servers, recipe.DefaultServer = a.local.Servers, a.local.DefaultServer
	recipe.ModelContextTokens = a.local.ModelContextTokens
	enc, err := newVectorQueryEncoder(recipe, "")
	if err != nil {
		return searcherAdapter{}, localSemanticUnavailable("configure a local query encoder")
	}
	adapter := newSearcherAdapter(a.ix, enc, vectorSpace(recipe, g))
	adapter.fingerprint = g.Fingerprint()
	return adapter, nil
}

func (a hubSearcherAdapter) SemanticReadiness(ctx context.Context) (db.SemanticReadiness, error) {
	adapter, err := a.active(ctx)
	if err != nil {
		return db.SemanticReadiness{State: "unavailable", Reason: "semantic_search_unavailable_locally"}, ctx.Err()
	}
	return adapter.SemanticReadiness(ctx)
}

func (a hubSearcherAdapter) PrepareSemanticSearch(ctx context.Context, query string) (db.VectorSearchPage, int, error) {
	adapter, err := a.active(ctx)
	if err != nil {
		return nil, 0, err
	}
	return adapter.PrepareSemanticSearch(ctx, query)
}
