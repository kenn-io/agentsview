package main

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	kitvec "go.kenn.io/kit/vector"
)

type hostedEmbeddingSearchStore interface {
	Active(context.Context) (*postgres.HostedEmbeddingGeneration, error)
	SearchGeneration(context.Context, postgres.HostedEmbeddingGeneration, []float32, int) ([]db.VectorHit, error)
	ResolveGenerationUnits(context.Context, postgres.HostedEmbeddingGeneration, []db.MessageRef) ([]db.UnitRef, error)
}

type hostedEmbeddingSearcher struct {
	store    hostedEmbeddingSearchStore
	resolver *hostedEmbeddingResolver
}

type boundHostedEmbeddingSearcher struct {
	store      hostedEmbeddingSearchStore
	generation postgres.HostedEmbeddingGeneration
	encoder    hostedResolvedEncoder
}

func newHostedEmbeddingSearcher(store hostedEmbeddingSearchStore, resolver *hostedEmbeddingResolver) *hostedEmbeddingSearcher {
	return &hostedEmbeddingSearcher{store: store, resolver: resolver}
}

func (s *hostedEmbeddingSearcher) SemanticAvailable(ctx context.Context) (bool, error) {
	active, err := s.store.Active(ctx)
	if err != nil || active == nil {
		return false, err
	}
	if err = s.resolver.available(*active); err != nil {
		return false, nil
	}
	return true, nil
}

func (s *hostedEmbeddingSearcher) BindVectorSearch(ctx context.Context) (db.VectorSearcher, error) {
	active, err := s.store.Active(ctx)
	if err != nil {
		return nil, db.NewSemanticUnavailableError("hosted active generation could not be read")
	}
	if active == nil {
		return nil, db.NewSemanticUnavailableError("no complete hosted embedding generation is active")
	}
	resolved, err := s.resolver.query(*active)
	if err != nil {
		return nil, db.NewSemanticUnavailableError("active hosted embedding profile is unavailable")
	}
	return &boundHostedEmbeddingSearcher{store: s.store, generation: *active, encoder: resolved}, nil
}

func (s *hostedEmbeddingSearcher) SemanticSearch(ctx context.Context, query string, limit int) ([]db.VectorHit, error) {
	bound, err := s.BindVectorSearch(ctx)
	if err != nil {
		return nil, err
	}
	return bound.SemanticSearch(ctx, query, limit)
}

func (s *boundHostedEmbeddingSearcher) SemanticSearch(ctx context.Context, query string, limit int) ([]db.VectorHit, error) {
	vectors, err := kitvec.EncodeBatched(ctx, s.encoder.encode, []kitvec.Chunk{{Index: 0, Text: query}}, s.encoder.batch...)
	if err != nil || len(vectors) != 1 {
		return nil, db.ErrSemanticTransient
	}
	return s.store.SearchGeneration(ctx, s.generation, []float32(vectors[0]), limit)
}

func (s *hostedEmbeddingSearcher) ResolveMessageUnits(ctx context.Context, refs []db.MessageRef) ([]db.UnitRef, error) {
	bound, err := s.BindVectorSearch(ctx)
	if err != nil {
		return nil, err
	}
	return bound.ResolveMessageUnits(ctx, refs)
}

func (s *boundHostedEmbeddingSearcher) ResolveMessageUnits(ctx context.Context, refs []db.MessageRef) ([]db.UnitRef, error) {
	return s.store.ResolveGenerationUnits(ctx, s.generation, refs)
}
