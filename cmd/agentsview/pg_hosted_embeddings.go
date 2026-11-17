package main

import (
	"context"
	"errors"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

func prepareHostedEmbeddings(ctx context.Context, app config.Config, pg config.PGConfig, store *postgres.HostedStore, poll time.Duration) (*pgRawRuntime, error) {
	if !app.Vector.Enabled || app.ArchiveContent.UsageOnly() {
		return nil, nil
	}
	gen := vectorGeneration(app.Vector.Embeddings)
	info := storage.VectorGenerationInfo{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params}
	if err := postgres.CheckHostedEmbeddingPrivileges(ctx, store.DB(), pg.Schema, info.Fingerprint); err != nil {
		return nil, err
	}
	encode, err := newVectorDocumentEncoder(app.Vector.Embeddings, "")
	if err != nil {
		return nil, err
	}
	_, server, err := app.Vector.Embeddings.Server("")
	if err != nil {
		return nil, err
	}
	batch := (vector.BuildOptions{BatchSize: server.BatchSize, ModelContextTokens: app.Vector.Embeddings.ModelContextTokens, MaxBatchTokens: server.MaxBatchTokens}).EncodeBatchOptions()
	if err := wireReplicaVectorSearch(ctx, app, pgReplica{}, store, "hosted pg serve"); err != nil {
		return nil, err
	}
	lastDrained := int64(-1)
	return newPGRawRuntime(ctx, poll, func(ctx context.Context) {
		var revision int64
		if err := store.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT corpus_revision FROM raw_corpus_state WHERE singleton=1),0)`).Scan(&revision); err != nil {
			if ctx.Err() == nil {
				log.Printf("hosted embeddings: reading corpus revision: %v", err)
			}
			return
		}
		if revision == lastDrained {
			return
		}
		started := time.Now()
		if err := postgres.EmbedBatch(ctx, store.DB(), info, encode, app.Vector.Embeddings.MaxInputChars, app.Vector.IncludeAutomated, batch...); err != nil {
			if ctx.Err() == nil {
				var batchError *postgres.HostedEmbeddingError
				if errors.As(err, &batchError) {
					log.Printf("hosted embeddings: processed=%d failed=%d elapsed=%s", batchError.Processed, batchError.Failed, time.Since(started))
				} else {
					log.Printf("hosted embeddings: pass failed elapsed=%s", time.Since(started))
				}
			}
			return
		}
		lastDrained = revision
	}), nil
}
