package main

import (
	"bufio"
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
	localEmbeddingEndpoint   = embedlocal.Endpoint
	installLocalEmbeddings   = embedlocal.Install
	errLocalApprovalRequired = errors.New("local embedding download requires approval")
)

func resolveLocalEmbeddings(ctx context.Context, server config.VectorEmbeddingsServerConfig, approve func(context.Context, int64) (bool, error), progress func(int64, int64)) (string, error) {
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
	if approve == nil {
		approve = func(context.Context, int64) (bool, error) { return false, errLocalApprovalRequired }
	}
	installOpts := embedlocal.InstallOptions{DataDir: opts.DataDir, Confirm: approve, Progress: progress}
	installation, err := installLocalEmbeddings(ctx, installOpts)
	if err != nil {
		if errors.Is(err, errLocalApprovalRequired) {
			return "", errors.New("local embeddings: run agentsview index embeddings build to approve the first download")
		}
		return "", err
	}
	opts.DataDir, opts.Executable = installation.DataDir, installation.Executable
	return localEmbeddingEndpoint(ctx, opts)
}

// prepareLocalEmbeddings asks before downloading the local model. When the
// daemon reports setup it can finish, it skips the download so the build's
// setup fallback asks once and the daemon downloads the model itself.
func prepareLocalEmbeddings(ctx context.Context, cfg config.Config, name string, in *bufio.Scanner, out io.Writer) error {
	_, server, err := cfg.Vector.Embeddings.Server(name)
	if err != nil {
		return err
	}
	setupPending := false
	_, err = resolveLocalEmbeddings(ctx, server, func(ctx context.Context, size int64) (bool, error) {
		_, setupPending = daemonSetupClient(ctx, cfg)
		return !setupPending && confirmScanned(in, out, fmt.Sprintf("Download approximately %d MB for local semantic search?", downloadMegabytes(size))), nil
	}, nil)
	if setupPending {
		return nil
	}
	return err
}

// daemonSetupClient returns the local daemon's client and whether it asks to
// turn search on. An unavailable daemon or status counts as no pending setup.
func daemonSetupClient(ctx context.Context, cfg config.Config) (embeddingsDaemonClient, bool) {
	if !IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		return embeddingsDaemonClient{}, false
	}
	client, err := resolveEmbeddingsDaemonClient(cfg)
	if err != nil {
		return client, false
	}
	status, err := client.status(ctx)
	return client, err == nil && status.Setup != nil && status.Setup.Action == "turn-on"
}

func downloadMegabytes(size int64) int64 { return (size + 999999) / 1000000 }

func lazyLocalEncoder(c config.VectorEmbeddingsConfig, name string, server config.VectorEmbeddingsServerConfig, role embedconfig.Role, retryRateLimits bool) kitvec.EncodeFunc {
	var mu sync.Mutex
	var cached *kitvec.EncodeFunc
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		mu.Lock()
		current := cached
		mu.Unlock()
		if current == nil {
			endpoint, err := resolveLocalEmbeddings(ctx, server, nil, nil)
			if err != nil {
				return nil, err
			}
			resolved := c
			deployment := server
			deployment.Endpoint = endpoint
			resolved.Servers = map[string]config.VectorEmbeddingsServerConfig{name: deployment}
			encoder, err := newVectorEncoder(resolved, name, role, retryRateLimits)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			if cached == nil {
				cached = &encoder
			}
			current = cached
			mu.Unlock()
		}
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
