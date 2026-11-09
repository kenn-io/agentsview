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
	"go.kenn.io/agentsview/internal/vector"
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

func prepareLocalEmbeddings(ctx context.Context, c config.VectorEmbeddingsConfig, name string, in *bufio.Scanner, out io.Writer, setup, fullRebuild bool, progress func(int64, int64)) (bool, error) {
	_, server, err := c.Server(name)
	if err != nil {
		return false, err
	}
	asked := false
	_, err = resolveLocalEmbeddings(ctx, server, func(_ context.Context, size int64) (bool, error) {
		asked = true
		message := fmt.Sprintf("Download approximately %d MB for local semantic search?", (size+999999)/1000000)
		if setup {
			message = fmt.Sprintf("Turn on search by meaning? Downloads approximately %d MB once, shared by Kenn apps.", (size+999999)/1000000)
		}
		if setup && fullRebuild {
			message += " This rebuild re-embeds every document."
		}
		return confirmScanned(in, out, message), nil
	}, progress)
	return asked, err
}

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

func setupEmbeddingsBuild(ctx context.Context, out io.Writer, in *bufio.Scanner, cfg config.Config, client embeddingsDaemonClient, req vector.BuildRequest) error {
	selected := cfg
	if req.Using != "" {
		selected.Vector.Embeddings.DefaultServer = req.Using
	}
	status, err := embeddingsSetupStatus(ctx, vector.SetupStatus{Action: "turn-on"}, selected)
	if err != nil {
		return err
	}
	var progress buildProgressPrinter
	asked, err := prepareLocalEmbeddings(ctx, cfg.Vector.Embeddings, req.Using, in, out, true, req.FullRebuild, func(done, total int64) {
		progress.printDownload(out, done, total)
	})
	if err != nil {
		return err
	}
	if !asked {
		message := fmt.Sprintf("Turn on search by meaning using %s (%s)?", status.Provider, cfg.Vector.Embeddings.Model)
		if req.FullRebuild {
			message += " This rebuild re-embeds every document."
		}
		if !confirmScanned(in, out, message) {
			return errors.New("search setup declined")
		}
	}
	if err := client.startSetup(ctx, req); err != nil {
		return err
	}
	return pollDaemonBuildStatus(ctx, out, client)
}
