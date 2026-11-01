package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedlocal"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
)

var (
	localEmbeddingEndpoint = embedlocal.Endpoint
	installLocalEmbeddings = embedlocal.Install
)

func resolveLocalEmbeddings(ctx context.Context, server config.VectorEmbeddingsServerConfig, approve func(context.Context, int64) (bool, error)) (string, error) {
	if server.Endpoint != "" {
		return server.Endpoint, nil
	}
	opts := embedlocal.Options{SkipOllama: server.Local}
	endpoint, err := localEmbeddingEndpoint(ctx, opts)
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return endpoint, err
	}
	opts.DataDir, err = embedlocal.DefaultDir()
	if err != nil {
		return "", err
	}
	installation, err := installLocalEmbeddings(ctx, embedlocal.InstallOptions{DataDir: opts.DataDir, Confirm: approve})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if approve == nil {
			return "", errors.New("local embeddings: run agentsview embeddings build to approve the first download")
		}
		return "", err
	}
	opts.DataDir, opts.Executable = installation.DataDir, installation.Executable
	return localEmbeddingEndpoint(ctx, opts)
}

func prepareLocalEmbeddings(ctx context.Context, c config.VectorEmbeddingsConfig, name string, in io.Reader, out io.Writer) error {
	_, server, err := c.Server(name)
	if err != nil {
		return err
	}
	_, err = resolveLocalEmbeddings(ctx, server, func(_ context.Context, size int64) (bool, error) {
		return confirm(in, out, fmt.Sprintf("Download approximately %d MB for local semantic search?", (size+999999)/1000000)), nil
	})
	return err
}

func lazyLocalEncoder(c config.VectorEmbeddingsConfig, name string, server config.VectorEmbeddingsServerConfig, role embedconfig.Role, retryRateLimits bool) kitvec.EncodeFunc {
	var mu sync.Mutex
	var cached *kitvec.EncodeFunc
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		mu.Lock()
		if cached == nil {
			endpoint, err := resolveLocalEmbeddings(ctx, server, nil)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			resolved := c
			deployment := server
			deployment.Endpoint = endpoint
			resolved.Servers = map[string]config.VectorEmbeddingsServerConfig{name: deployment}
			encoder, err := newVectorEncoder(resolved, name, role, retryRateLimits)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			cached = &encoder
		}
		current := cached
		mu.Unlock()
		vectors, err := (*current)(ctx, texts)
		if _, ok := errors.AsType[*embedclient.TransportError](err); ok {
			mu.Lock()
			if cached == current {
				cached = nil
			}
			mu.Unlock()
		}
		return vectors, err
	}
}
