package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedconfig"
)

func prepareHostedEmbeddings(ctx context.Context, app config.Config, store *postgres.HostedStore, poll time.Duration) (*pgRawRuntime, error) {
	if !app.Vector.Enabled || app.ArchiveContent.UsageOnly() {
		return nil, nil
	}
	gen := vectorGeneration(app.Vector.Embeddings)
	info := storage.VectorGenerationInfo{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params}
	encode, err := newVectorEncoder(app.Vector.Embeddings, "", embedconfig.RoleDocument, false)
	if err != nil {
		return nil, err
	}
	_, server, err := app.Vector.Embeddings.Server("")
	if err != nil {
		return nil, err
	}
	batch := (vector.BuildOptions{BatchSize: server.BatchSize, ModelContextTokens: app.Vector.Embeddings.ModelContextTokens, MaxBatchTokens: server.MaxBatchTokens}).EncodeBatchOptions()
	drained := int64(-1)
	return newPGRawRuntime(ctx, poll, func(ctx context.Context) {
		var revision int64
		if err := store.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT corpus_revision FROM raw_corpus_state WHERE singleton=1),0)`).Scan(&revision); err != nil {
			if ctx.Err() == nil {
				log.Printf("hosted embeddings: reading corpus revision: %v", err)
			}
			return
		}
		if revision == drained {
			return
		}
		err := postgres.EmbedBatch(ctx, store.DB(), info, encode, app.Vector.Embeddings.MaxInputChars, server.Concurrency, app.Vector.IncludeAutomated, batch...)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("hosted embeddings: pass failed: %v", err)
			}
			return
		}
		drained = revision
	}), nil
}
