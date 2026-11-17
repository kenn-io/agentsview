package main

import (
	"context"
	"log"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

type hostedEmbeddingSchedule struct {
	drained, failed int64
	retryAt         time.Time
	delay           *backoff.ExponentialBackOff
}

func newHostedEmbeddingSchedule() *hostedEmbeddingSchedule {
	delay := backoff.NewExponentialBackOff()
	delay.InitialInterval = 30 * time.Second
	delay.MaxInterval = 5 * time.Minute
	delay.Reset()
	return &hostedEmbeddingSchedule{drained: -1, failed: -1, delay: delay}
}

func (s *hostedEmbeddingSchedule) ready(revision int64, now time.Time) bool {
	if revision == s.drained || revision == s.failed && now.Before(s.retryAt) {
		return false
	}
	if revision != s.failed {
		s.delay.Reset()
	}
	return true
}

func (s *hostedEmbeddingSchedule) finish(revision int64, now time.Time, err error) {
	if err != nil {
		s.failed = revision
		s.retryAt = now.Add(s.delay.NextBackOff())
		return
	}
	s.drained = revision
	s.failed = -1
	s.delay.Reset()
}

func prepareHostedEmbeddings(ctx context.Context, app config.Config, store *postgres.HostedStore, poll time.Duration) (*pgRawRuntime, error) {
	if !app.Vector.Enabled || app.ArchiveContent.UsageOnly() {
		return nil, nil
	}
	gen := vectorGeneration(app.Vector.Embeddings)
	info := storage.VectorGenerationInfo{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params}
	encode, err := newVectorDocumentEncoder(app.Vector.Embeddings, "")
	if err != nil {
		return nil, err
	}
	_, server, err := app.Vector.Embeddings.Server("")
	if err != nil {
		return nil, err
	}
	batch := (vector.BuildOptions{BatchSize: server.BatchSize, ModelContextTokens: app.Vector.Embeddings.ModelContextTokens, MaxBatchTokens: server.MaxBatchTokens}).EncodeBatchOptions()
	schedule := newHostedEmbeddingSchedule()
	return newPGRawRuntime(ctx, poll, func(ctx context.Context) {
		var revision int64
		if err := store.DB().QueryRowContext(ctx, `SELECT COALESCE((SELECT corpus_revision FROM raw_corpus_state WHERE singleton=1),0)`).Scan(&revision); err != nil {
			if ctx.Err() == nil {
				log.Printf("hosted embeddings: reading corpus revision: %v", err)
			}
			return
		}
		if !schedule.ready(revision, time.Now()) {
			return
		}
		started := time.Now()
		err := postgres.EmbedBatch(ctx, store.DB(), info, encode, app.Vector.Embeddings.MaxInputChars, server.Concurrency, app.Vector.IncludeAutomated, batch...)
		schedule.finish(revision, time.Now(), err)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("hosted embeddings: pass failed elapsed=%s: %v", time.Since(started), err)
			}
			return
		}
	}), nil
}
