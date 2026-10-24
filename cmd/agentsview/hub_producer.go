package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

func hubProducerBuild(ix *vector.Index, database *db.DB, cfg config.Config) func(context.Context, vector.BuildRequest, func(vector.BuildProgress)) (vector.BuildResult, error) {
	return func(ctx context.Context, req vector.BuildRequest, progress func(vector.BuildProgress)) (vector.BuildResult, error) {
		hub, err := newProducerHub(cfg)
		if err != nil {
			return vector.BuildResult{}, err
		}
		for range 3 {
			h, err := hub.HubVectorGeneration(ctx)
			if errors.Is(err, vector.ErrHubGenerationChanged) {
				continue
			}
			if err != nil {
				return vector.BuildResult{}, err
			}
			cfg.Vector.IncludeAutomated = req.IncludeAutomated
			recipe, err := decodeHubRecipe(cfg, h)
			if err != nil {
				return vector.BuildResult{}, err
			}
			result, err := ix.Pull(ctx, database.LocalEmbeddableSource(), hub, vectorGeneration(recipe), vector.PullOptions{
				IncludeAutomated: req.IncludeAutomated, FullRebuild: req.FullRebuild, RepairInvalid: req.RepairInvalid,
				MapSession: func(id string) string { return remotesync.HubSessionID(cfg.Remote.SpokeHost, id) },
				Progress:   progress,
			})
			if !errors.Is(err, vector.ErrHubGenerationChanged) {
				return result, err
			}
		}
		return vector.BuildResult{}, vector.ErrHubGenerationChanged
	}
}

// newProducerHub preserves the remote credential policy and native request signing.
func newProducerHub(cfg config.Config) (*httpHub, error) {
	client, err := remotecred.Client(cfg.Remote.URL, cfg.Remote.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(os.Getenv("AGENTSVIEW_SERVER_TOKEN"))
	if cfg.Remote.TokenFile != "" {
		token, err = remotecred.ReadToken(cfg.Remote.TokenFile)
		if err != nil {
			return nil, err
		}
	}
	return &httpHub{URL: cfg.Remote.URL, Token: token, Client: client}, nil
}

func producerRecipe(ctx context.Context, cfg config.Config) (config.VectorEmbeddingsConfig, error) {
	hub, err := newProducerHub(cfg)
	if err != nil {
		return config.VectorEmbeddingsConfig{}, err
	}
	h, err := hub.HubVectorGeneration(ctx)
	if err != nil {
		return config.VectorEmbeddingsConfig{}, err
	}
	return decodeHubRecipe(cfg, h)
}

// decodeHubRecipe adopts only a strictly reproducible hub recipe. Defaults do
// not conflict; fields the user explicitly configured must describe that space.
func decodeHubRecipe(cfg config.Config, h vector.HubGeneration) (config.VectorEmbeddingsConfig, error) {
	if h.Unavailable != "" {
		return config.VectorEmbeddingsConfig{}, errors.New("hub generation unavailable")
	}
	if h.DataVersion != db.CurrentDataVersion() {
		return config.VectorEmbeddingsConfig{}, fmt.Errorf("data version mismatch: hub %d, local %d", h.DataVersion, db.CurrentDataVersion())
	}
	if h.IncludeAutomated != cfg.Vector.IncludeAutomated {
		return config.VectorEmbeddingsConfig{}, errors.New("automated scope mismatch")
	}
	recipe, ok := decodeReplicaRecipe(storage.VectorGenerationInfo{Fingerprint: h.Fingerprint, Model: h.Model, Dimension: h.Dimension, Params: h.Params})
	if !ok {
		return recipe, errors.New("recipe mismatch: unsupported hub recipe")
	}
	local := cfg.Vector.Embeddings
	check := recipe
	for key := range local.RecipeKeys {
		switch key {
		case "model":
			check.Model = local.Model
		case "dimension":
			check.Dimension = local.Dimension
		case "request_dimensions":
			check.RequestDimensions = local.RequestDimensions
		case "max_input_chars":
			check.MaxInputChars = local.MaxInputChars
		case "query_prefix":
			check.QueryPrefix = local.QueryPrefix
		case "document_prefix":
			check.DocumentPrefix = local.DocumentPrefix
		case "input_suffix":
			check.InputSuffix = local.InputSuffix
		}
	}
	if vectorGeneration(check).Fingerprint() != h.Fingerprint {
		return recipe, errors.New("recipe mismatch: explicit local fields conflict with the hub")
	}
	recipe.Servers, recipe.DefaultServer = local.Servers, local.DefaultServer
	recipe.ModelContextTokens = local.ModelContextTokens
	return recipe, nil
}
