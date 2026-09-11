package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	kitvec "go.kenn.io/kit/vector"
)

type hostedResolvedEncoder struct {
	encode kitvec.EncodeFunc
	batch  []kitvec.BatchOption
}

type hostedEmbeddingResolver struct {
	cfg  config.HostedEmbeddingsConfig
	mu   sync.Mutex
	enc  map[string]hostedResolvedEncoder
	gate map[string]chan struct{}
}

func newHostedEmbeddingResolver(cfg config.HostedEmbeddingsConfig) *hostedEmbeddingResolver {
	return &hostedEmbeddingResolver{cfg: cfg, enc: map[string]hostedResolvedEncoder{}, gate: map[string]chan struct{}{}}
}

func (r *hostedEmbeddingResolver) validate(g postgres.HostedEmbeddingGeneration) (config.HostedEmbeddingProfile, string, config.VectorEmbeddingsServerConfig, string, error) {
	profile, err := r.cfg.Profile(g.Recipe.ProfileName)
	if err != nil {
		return profile, "", config.VectorEmbeddingsServerConfig{}, "", err
	}
	want, err := hostedEmbeddingRecipe(g.Recipe.ProfileName, profile)
	if err != nil {
		return profile, "", config.VectorEmbeddingsServerConfig{}, "", err
	}
	if want != g.Recipe {
		return profile, "", config.VectorEmbeddingsServerConfig{}, "", errors.New("configured profile does not match the active hosted embedding recipe")
	}
	name, server, err := profile.Embeddings.Server("")
	if err != nil {
		return profile, "", config.VectorEmbeddingsServerConfig{}, "", err
	}
	key := ""
	if server.APIKeyEnv != "" {
		value, ok := os.LookupEnv(server.APIKeyEnv)
		if !ok || strings.TrimSpace(value) == "" {
			return profile, "", config.VectorEmbeddingsServerConfig{}, "", errors.New("selected API key reference is missing or empty")
		}
		key = value
	}
	return profile, name, server, key, nil
}

func (r *hostedEmbeddingResolver) available(g postgres.HostedEmbeddingGeneration) error {
	_, _, _, _, err := r.validate(g)
	return err
}

func (r *hostedEmbeddingResolver) document(g postgres.HostedEmbeddingGeneration) (hostedResolvedEncoder, error) {
	return r.resolve(g, true)
}

func (r *hostedEmbeddingResolver) query(g postgres.HostedEmbeddingGeneration) (hostedResolvedEncoder, error) {
	return r.resolve(g, false)
}

func (r *hostedEmbeddingResolver) resolve(g postgres.HostedEmbeddingGeneration, document bool) (hostedResolvedEncoder, error) {
	role := "query"
	if document {
		role = "document"
	}
	cacheKey := fmt.Sprintf("%d:%s:%s", g.ID, g.Recipe.Fingerprint, role)
	r.mu.Lock()
	cached, ok := r.enc[cacheKey]
	r.mu.Unlock()
	if ok {
		return cached, nil
	}
	profile, serverName, server, key, err := r.validate(g)
	if err != nil {
		return hostedResolvedEncoder{}, err
	}
	prefix := profile.Embeddings.QueryPrefix
	retryRateLimits := false
	if document {
		prefix = profile.Embeddings.DocumentPrefix
		retryRateLimits = true
	}
	encoder, err := newVectorEncoderWithResolvedKey(profile.Embeddings, serverName, server, key, prefix, retryRateLimits)
	if err != nil {
		return hostedResolvedEncoder{}, err
	}
	r.mu.Lock()
	gateKey := g.Recipe.ProfileName + ":" + serverName
	transportGate := r.gate[gateKey]
	if transportGate == nil {
		transportGate = make(chan struct{}, server.Concurrency)
		r.gate[gateKey] = transportGate
	}
	r.mu.Unlock()
	limited := func(ctx context.Context, texts []string) ([][]float32, error) {
		select {
		case transportGate <- struct{}{}:
			defer func() { <-transportGate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return encoder(ctx, texts)
	}
	batch := []kitvec.BatchOption{kitvec.WithBatchSize(server.BatchSize)}
	if profile.Embeddings.ModelContextTokens > 0 && server.MaxBatchTokens > 0 {
		batch = append(batch, kitvec.WithBatchTokenBudget(server.MaxBatchTokens, profile.Embeddings.ModelContextTokens))
	}
	resolved := hostedResolvedEncoder{encode: limited, batch: batch}
	r.mu.Lock()
	if current, exists := r.enc[cacheKey]; exists {
		resolved = current
	} else {
		r.enc[cacheKey] = resolved
	}
	r.mu.Unlock()
	return resolved, nil
}
