// Package vector wires agentsview into kit's vector package for semantic
// search: SQLite-backed vector storage and OpenAI-compatible embeddings.
package vector

import (
	"context"
	"fmt"
	"math"
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
	retry := embedclient.Retry{
		MaxAttempts:     cfg.MaxRetries,
		InitialBackoff:  backoffBase,
		MaxBackoff:      backoffMax,
		MaxRetryAfter:   retryAfterCap,
		RetryRateLimits: cfg.RetryRateLimits,
	}
	client, err := embedclient.New(embedclient.Options{
		Model:      cfg.Model,
		Roles:      cfg.Roles,
		Deployment: cfg.Deployment,
		Batch:      embedconfig.Batch{Items: math.MaxInt32},
		Transport:  cfg.Transport,
		APIKey:     cfg.APIKey,
		// Ollama recovery re-encodes only the invalid inputs.
		OllamaMetalRecovery: cfg.OllamaMetalRecovery,
		Retry:               retry,
	})
	if err != nil {
		return nil, fmt.Errorf("[vector.embeddings] configure client: %w", err)
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		return client.EmbedTexts(ctx, role, texts)
	}, nil
}
