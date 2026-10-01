// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	kitvec "go.kenn.io/kit/vector"
)

// EncoderConfig configures one role's embeddings encoder on kit's shared
// client. Model, Roles, Deployment, and Transport are the shared embedding
// contract; the rest is agentsview's retry policy.
type EncoderConfig struct {
	Model      embedconfig.Model
	Roles      embedconfig.Roles
	Deployment embedconfig.Deployment
	Transport  embedconfig.Transport
	// APIKey is sent as a Bearer token when non-empty.
	APIKey string
	// OllamaMetalRecovery enables kit's Ollama Metal recovery for an Ollama
	// endpoint ending in /v1.
	OllamaMetalRecovery bool
	// MaxRetries is the maximum total attempts on retryable errors; values
	// <= 1 mean one attempt.
	MaxRetries int
	// RetryRateLimits keeps retrying HTTP 429 responses after MaxRetries is
	// spent, until the request succeeds or ctx is canceled. Long-running
	// document builds enable this; latency-sensitive query encoders do not.
	RetryRateLimits bool
}

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 5 * time.Second
	// retryAfterCap bounds how long a Retry-After header can push a single
	// wait, so a misbehaving endpoint cannot stall a build for arbitrarily
	// long.
	retryAfterCap = 60 * time.Second
)

// NewEncoder returns a kitvec.EncodeFunc that embeds texts for role through
// kit's embedclient, applying the role's configured prefix and suffix. Callers
// batch through kitvec, so the client never splits a call further.
func NewEncoder(cfg EncoderConfig, role embedconfig.Role) (kitvec.EncodeFunc, error) {
	client, err := embedclient.New(embedclient.Options{
		Model:      cfg.Model,
		Roles:      cfg.Roles,
		Deployment: cfg.Deployment,
		Batch:      embedconfig.Batch{Items: math.MaxInt32},
		Transport:  cfg.Transport,
		APIKey:     cfg.APIKey,
		// Ollama recovery re-encodes only the invalid inputs.
		OllamaMetalRecovery: cfg.OllamaMetalRecovery,
		Retry: embedclient.Retry{
			MaxAttempts:    cfg.MaxRetries,
			InitialBackoff: backoffBase,
			MaxBackoff:     backoffMax,
			MaxRetryAfter:  retryAfterCap,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("[vector.embeddings] configure client: %w", err)
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		for attempt := 1; ; attempt++ {
			vectors, err := client.EmbedTexts(ctx, role, texts)
			if err == nil || !cfg.RetryRateLimits {
				return vectors, err
			}
			apiErr, ok := errors.AsType[*embedclient.APIError](err)
			if !ok || apiErr.StatusCode != http.StatusTooManyRequests {
				return nil, err
			}
			// A 429 clears with time and does not mean the request is
			// broken, so a durable build waits it out instead of aborting.
			if err := sleepBackoff(ctx, rateLimitDelay(attempt, apiErr.RetryAfter)); err != nil {
				return nil, err
			}
		}
	}, nil
}

// rateLimitDelay honors a provider's Retry-After, capped at retryAfterCap,
// and otherwise uses capped exponential backoff from attempt.
func rateLimitDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, retryAfterCap)
	}
	delay := backoffBase << min(attempt-1, 16)
	if delay > backoffMax || delay <= 0 {
		delay = backoffMax
	}
	return delay
}

// sleepBackoff waits delay, returning ctx.Err() promptly if ctx is canceled.
func sleepBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
