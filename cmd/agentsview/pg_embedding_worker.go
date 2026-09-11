package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

type hostedEmbeddingWorkStore interface {
	Reconcile(context.Context, int) (postgres.HostedEmbeddingReconcileResult, error)
	Desired(context.Context) (*postgres.HostedEmbeddingGeneration, error)
	Claim(context.Context, string, int, time.Duration) ([]postgres.HostedEmbeddingLease, error)
	ReadSession(context.Context, postgres.HostedEmbeddingLease) (*postgres.HostedEmbeddingSnapshot, error)
	ReusableVectors(context.Context, *postgres.HostedEmbeddingSnapshot) ([]postgres.HostedEmbeddingVector, error)
	Publish(context.Context, *postgres.HostedEmbeddingSnapshot, []postgres.HostedEmbeddingVector) error
	Heartbeat(context.Context, postgres.HostedEmbeddingLease, time.Duration) (postgres.HostedEmbeddingLease, error)
	Fail(context.Context, postgres.HostedEmbeddingLease, string, bool) error
	Activate(context.Context, int64) (bool, error)
}

type hostedEmbeddingRuntimeOptions struct {
	PollInterval, AttemptTimeout, LeaseDuration, HeartbeatInterval time.Duration
	CoordinationTimeout, FailureTimeout                            time.Duration
	SourceConcurrency                                              int
	Owner                                                          string
}

type hostedEmbeddingRuntime struct {
	mu               sync.Mutex
	started, stopped bool
	ctx              context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	store            hostedEmbeddingWorkStore
	resolver         *hostedEmbeddingResolver
	opts             hostedEmbeddingRuntimeOptions
}

func newHostedEmbeddingRuntime(parent context.Context, store hostedEmbeddingWorkStore, resolver *hostedEmbeddingResolver, opts hostedEmbeddingRuntimeOptions) *hostedEmbeddingRuntime {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = 120 * time.Second
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = time.Minute
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 10 * time.Second
	}
	if opts.CoordinationTimeout <= 0 {
		opts.CoordinationTimeout = 10 * time.Second
	}
	if opts.FailureTimeout <= 0 {
		opts.FailureTimeout = 5 * time.Second
	}
	if opts.SourceConcurrency <= 0 {
		opts.SourceConcurrency = 1
	}
	if opts.Owner == "" {
		opts.Owner = fmt.Sprintf("hosted-embeddings-%d", os.Getpid())
	}
	ctx, cancel := context.WithCancel(parent)
	return &hostedEmbeddingRuntime{ctx: ctx, cancel: cancel, done: make(chan struct{}), store: store, resolver: resolver, opts: opts}
}

func (r *hostedEmbeddingRuntime) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.opts.PollInterval)
		defer ticker.Stop()
		for {
			if r.ctx.Err() != nil {
				return
			}
			if err := r.processBatch(r.ctx); err != nil && r.ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "hosted embedding worker: batch failed")
			}
			select {
			case <-r.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (r *hostedEmbeddingRuntime) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.cancel()
	started := r.started
	r.mu.Unlock()
	if started {
		<-r.done
	}
}

func (r *hostedEmbeddingRuntime) processBatch(ctx context.Context) error {
	coordination, cancel := context.WithTimeout(ctx, r.opts.CoordinationTimeout)
	_, err := r.store.Reconcile(coordination, 64)
	cancel()
	if err != nil {
		return err
	}
	coordination, cancel = context.WithTimeout(ctx, r.opts.CoordinationTimeout)
	desired, err := r.store.Desired(coordination)
	cancel()
	if err != nil {
		return err
	}
	coordination, cancel = context.WithTimeout(ctx, r.opts.CoordinationTimeout)
	leases, err := r.store.Claim(coordination, r.opts.Owner, r.opts.SourceConcurrency, r.opts.LeaseDuration)
	cancel()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for _, lease := range leases {
		wg.Go(func() {
			if err := r.processLease(ctx, lease); err != nil && ctx.Err() == nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		})
	}
	wg.Wait()
	if desired != nil && r.resolver.available(*desired) == nil {
		coordination, cancel = context.WithTimeout(ctx, r.opts.CoordinationTimeout)
		_, activateErr := r.store.Activate(coordination, desired.ID)
		cancel()
		if activateErr != nil && firstErr == nil {
			firstErr = activateErr
		}
	}
	return firstErr
}

func (r *hostedEmbeddingRuntime) processLease(parent context.Context, lease postgres.HostedEmbeddingLease) error {
	ctx, cancel := context.WithTimeout(parent, r.opts.AttemptTimeout)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := r.store.Heartbeat(ctx, lease, r.opts.LeaseDuration); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	finishHeartbeat := func() { cancel(); <-heartbeatDone }

	snapshot, err := r.store.ReadSession(ctx, lease)
	if err != nil {
		finishHeartbeat()
		return r.recordFailure(parent, lease, err, "source_read")
	}
	resolved, err := r.resolver.document(snapshot.Generation)
	if err != nil {
		finishHeartbeat()
		return r.recordFailure(parent, lease, err, "encoder_unavailable")
	}
	reused, err := r.store.ReusableVectors(ctx, snapshot)
	if err != nil {
		finishHeartbeat()
		return r.recordFailure(parent, lease, err, "source_read")
	}
	vectors, err := encodeHostedEmbeddingSnapshot(ctx, snapshot, reused, resolved)
	if err != nil {
		finishHeartbeat()
		return r.recordFailure(parent, lease, err, "")
	}
	if err = r.store.Publish(ctx, snapshot, vectors); err != nil {
		finishHeartbeat()
		if errors.Is(err, postgres.ErrHostedEmbeddingLeaseLost) || errors.Is(err, postgres.ErrHostedEmbeddingStale) {
			return nil
		}
		return r.recordFailure(parent, lease, err, "invalid_results")
	}
	finishHeartbeat()
	return nil
}

func encodeHostedEmbeddingSnapshot(ctx context.Context, snapshot *postgres.HostedEmbeddingSnapshot, reused []postgres.HostedEmbeddingVector, resolved hostedResolvedEncoder) ([]postgres.HostedEmbeddingVector, error) {
	type key struct {
		doc   string
		index int
	}
	values := make(map[key]postgres.HostedEmbeddingVector, len(reused))
	for _, value := range reused {
		values[key{value.DocumentKey, value.ChunkIndex}] = value
	}
	var chunks []kitvec.Chunk
	var keys []key
	for _, document := range snapshot.Documents {
		for _, chunk := range document.Chunks {
			k := key{document.Key, chunk.Index}
			if _, ok := values[k]; ok {
				continue
			}
			keys = append(keys, k)
			chunks = append(chunks, kitvec.Chunk{Index: chunk.Index, Text: chunk.Text})
		}
	}
	if len(chunks) > 0 {
		encoded, err := kitvec.EncodeBatched(ctx, resolved.encode, chunks, resolved.batch...)
		if err != nil {
			return nil, err
		}
		if len(encoded) != len(keys) {
			return nil, postgres.ErrHostedEmbeddingInvalidResults
		}
		for i, v := range encoded {
			k := keys[i]
			values[k] = postgres.HostedEmbeddingVector{DocumentKey: k.doc, ChunkIndex: k.index, Values: []float32(v)}
		}
	}
	out := make([]postgres.HostedEmbeddingVector, 0, len(values))
	for _, document := range snapshot.Documents {
		for _, chunk := range document.Chunks {
			value, ok := values[key{document.Key, chunk.Index}]
			if !ok {
				return nil, postgres.ErrHostedEmbeddingInvalidResults
			}
			out = append(out, value)
		}
	}
	return out, nil
}

func (r *hostedEmbeddingRuntime) recordFailure(ctx context.Context, lease postgres.HostedEmbeddingLease, err error, fallback string) error {
	// Unclassified store failures may recover without a new source revision.
	// Resolver/profile failures stay permanent; typed failures keep their policy.
	code, retryable := hostedEmbeddingFailure(err, fallback == "source_read" || fallback == "invalid_results")
	if fallback != "" && code == "encoder_unavailable" {
		code = fallback
	}
	failureCtx, cancel := context.WithTimeout(ctx, r.opts.FailureTimeout)
	defer cancel()
	if failErr := r.store.Fail(failureCtx, lease, code, retryable); failErr != nil {
		return failErr
	}
	return nil
}

func hostedEmbeddingFailure(err error, unknownRetryable bool) (string, bool) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "encoder_timeout", true
	}
	if errors.Is(err, postgres.ErrHostedEmbeddingWorkLimit) {
		return "work_limit", false
	}
	if errors.Is(err, postgres.ErrHostedEmbeddingInvalidResults) {
		return "invalid_results", false
	}
	if status, ok := errors.AsType[*vector.HTTPStatusError](err); ok {
		if status.Status == 429 {
			return "encoder_rate_limit", true
		}
		if status.Permanent() {
			return "invalid_results", false
		}
		return "encoder_unavailable", status.Status >= 500
	}
	if _, ok := errors.AsType[*vector.InvalidEmbeddingError](err); ok {
		return "invalid_results", false
	}
	if retryable, ok := vector.FailureRetryable(err); ok {
		return "encoder_unavailable", retryable
	}
	return "encoder_unavailable", unknownRetryable
}
