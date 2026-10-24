package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedconfig"
	kitvec "go.kenn.io/kit/vector"
)

var (
	errHubDataVersion    = errors.New("data version mismatch")
	errHubAutomatedScope = errors.New("automated scope mismatch")
	errHubRecipe         = errors.New("recipe mismatch")
)

func hubProducerBuild(ix *vector.Index, database *db.DB, cfg config.Config) func(context.Context, vector.BuildRequest, func(vector.BuildProgress)) (vector.BuildResult, error) {
	return func(ctx context.Context, req vector.BuildRequest, progress func(vector.BuildProgress)) (result vector.BuildResult, buildErr error) {
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			status := vector.HubPullStatus{Unmatched: -1}
			previous, err := ix.HubPullStatus(ctx)
			buildErr = errors.Join(buildErr, err)
			if previous != nil {
				status.Fingerprint, status.Unmatched = previous.Fingerprint, previous.Unmatched
			}
			if result.Fingerprint != "" {
				missing, err := ix.MissingEmbeddedDocs(ctx, result.Fingerprint, nil)
				buildErr = errors.Join(buildErr, err)
				if err == nil {
					status.Fingerprint, status.Unmatched = result.Fingerprint, missing
				}
			}
			if buildErr != nil {
				status.Reason = "hub pull unavailable"
				for _, reason := range []error{errHubDataVersion, errHubAutomatedScope, errHubRecipe} {
					if errors.Is(buildErr, reason) {
						status.Reason = reason.Error()
						break
					}
				}
			} else if status.Unmatched > 0 {
				status.Reason = "hash mismatch or hub coverage missing"
			}
			buildErr = errors.Join(buildErr, ix.SetHubPullStatus(ctx, status))
		}()
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
			var enc kitvec.EncodeFunc
			var server config.VectorEmbeddingsServerConfig
			if req.LocalProducer {
				name, selected, err := recipe.Server(req.Using)
				if err != nil {
					return vector.BuildResult{}, err
				}
				server = selected
				enc, err = newVectorEncoderForServer(recipe, name, server, embedconfig.RoleDocument, true)
				if err != nil {
					return vector.BuildResult{}, err
				}
			}
			result, err := ix.Pull(ctx, database.LocalEmbeddableSource(), hub, vectorGeneration(recipe), vector.PullOptions{
				IncludeAutomated: req.IncludeAutomated, FullRebuild: req.FullRebuild, Backstop: req.Backstop, RepairInvalid: req.RepairInvalid,
				MapSession: func(id string) string { return remotesync.HubSessionID(cfg.Remote.SpokeHost, id) },
				Progress:   progress,
			})
			if !errors.Is(err, vector.ErrHubGenerationChanged) {
				repairIncomplete := req.RepairInvalid && req.LocalProducer && errors.Is(err, vector.ErrHubRepairIncomplete)
				if !req.LocalProducer || (err != nil && !repairIncomplete) {
					return result, err
				}
				built, err := ix.Build(ctx, database.LocalEmbeddableSource(), enc, vectorGeneration(recipe), vector.BuildOptions{
					RepairInvalid:    req.RepairInvalid,
					IncludeAutomated: req.IncludeAutomated, BatchSize: server.BatchSize,
					ModelContextTokens: recipe.ModelContextTokens, MaxBatchTokens: server.MaxBatchTokens,
					Concurrency: server.Concurrency, Progress: progress,
				})
				if req.RepairInvalid {
					built.Repair.Scanned = result.Repair.Scanned
					built.Repair.ScanComplete = result.Repair.ScanComplete
					built.Repair.Documents = result.Repair.Documents
					built.Repair.Chunks = result.Repair.Chunks
				} else {
					built.Repair = result.Repair
				}
				return built, err
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
		return config.VectorEmbeddingsConfig{}, fmt.Errorf("%w: hub %d, local %d", errHubDataVersion, h.DataVersion, db.CurrentDataVersion())
	}
	if h.IncludeAutomated != cfg.Vector.IncludeAutomated {
		return config.VectorEmbeddingsConfig{}, errHubAutomatedScope
	}
	recipe, ok := decodeReplicaRecipe(storage.VectorGenerationInfo{Fingerprint: h.Fingerprint, Model: h.Model, Dimension: h.Dimension, Params: h.Params})
	if !ok {
		return recipe, fmt.Errorf("%w: unsupported hub recipe", errHubRecipe)
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
		return recipe, fmt.Errorf("%w: explicit local fields conflict with the hub", errHubRecipe)
	}
	recipe.Servers, recipe.DefaultServer = local.Servers, local.DefaultServer
	recipe.ModelContextTokens = local.ModelContextTokens
	return recipe, nil
}
