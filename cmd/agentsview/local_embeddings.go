package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

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

func prepareLocalEmbeddings(ctx context.Context, c config.VectorEmbeddingsConfig, name string, in *bufio.Scanner, out io.Writer) error {
	_, server, err := c.Server(name)
	if err != nil {
		return err
	}
	_, err = resolveLocalEmbeddings(ctx, server, func(_ context.Context, size int64) (bool, error) {
		return confirmScanned(in, out, fmt.Sprintf("Download approximately %d MB for local semantic search?", (size+999999)/1000000)), nil
	}, nil)
	return err
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

func setupEmbeddingsBuild(ctx context.Context, out io.Writer, in *bufio.Scanner, cfg config.Config, client *embeddingsDaemonClient, req vector.BuildRequest, opts EmbeddingsBuildOptions) error {
	selected := cfg
	if req.Using != "" {
		selected.Vector.Embeddings.DefaultServer = req.Using
	}
	status, err := embeddingsSetupStatus(ctx, vector.SetupStatus{Action: "turn-on"}, selected)
	if err != nil {
		return err
	}
	_, server, err := cfg.Vector.Embeddings.Server(req.Using)
	if err != nil {
		return err
	}
	confirmed := false
	var approvedBytes int64
	lastPercent := int64(-1)
	_, err = resolveLocalEmbeddings(ctx, server, func(_ context.Context, size int64) (bool, error) {
		message := fmt.Sprintf("Turn on search by meaning? Downloads approximately %d MB once, shared by Kenn apps.", (size+999999)/1000000)
		if req.FullRebuild {
			message += " This rebuild re-embeds every document."
		}
		if !confirmScanned(in, out, message) {
			return false, errors.New("search setup declined")
		}
		confirmed, approvedBytes = true, size
		return true, nil
	}, func(done, total int64) {
		percent := done * 100 / max(total, 1)
		if percent != lastPercent {
			lastPercent = percent
			fmt.Fprintf(out, "Download: %d%% of %d MB\n", percent, (total+999999)/1000000)
		}
	})
	if err != nil {
		return err
	}
	if !confirmed {
		message := fmt.Sprintf("Turn on search by meaning using %s (%s)?", status.Provider, cfg.Vector.Embeddings.Model)
		if req.FullRebuild {
			message += " This rebuild re-embeds every document."
		}
		if !confirmScanned(in, out, message) {
			return errors.New("search setup declined")
		}
	}
	if client != nil {
		if err := client.startSetup(ctx, approvedBytes, req); err != nil {
			if apiErr, ok := errors.AsType[*daemonAPIError](err); ok && apiErr.status == http.StatusConflict {
				return joinDaemonSetup(ctx, out, in, cfg, *client, req, opts, true)
			}
			return err
		}
		if err := pollDaemonSetup(ctx, out, *client); err != nil {
			return err
		}
		return pollDaemonBuildStatus(ctx, out, *client)
	}
	cfg, err = loadDaemonClientConfig(config.ScopeVector)
	if err != nil {
		return err
	}
	if err := cfg.SaveVectorSetup(); err != nil {
		return err
	}
	cfg, err = config.LoadReadOnlyFor(config.ScopeVector)
	if err != nil {
		return err
	}
	return runEmbeddingsBuildDirect(ctx, out, cfg, req)
}

func pollDaemonSetup(ctx context.Context, out io.Writer, client embeddingsDaemonClient) error {
	var downloaded, total int64
	for {
		status, err := client.status(ctx)
		if err != nil {
			return err
		}
		if status.Setup != nil {
			setup := status.Setup
			if setup.Error != "" {
				return errors.New(setup.Error)
			}
			if setup.DownloadedBytes != downloaded || setup.TotalBytes != total {
				downloaded, total = setup.DownloadedBytes, setup.TotalBytes
				fmt.Fprintf(out, "Download: %d/%d bytes\n", downloaded, total)
			}
			if !setup.Busy {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(embeddingsPollInterval):
		}
	}
}

func joinDaemonSetup(ctx context.Context, out io.Writer, in *bufio.Scanner, cfg config.Config, client embeddingsDaemonClient, req vector.BuildRequest, opts EmbeddingsBuildOptions, confirmed bool) error {
	fmt.Fprintln(out, "a build is already running (daemon)")
	if err := pollDaemonSetup(ctx, out, client); err != nil {
		return err
	}
	other := client
	if client.store == "" {
		other.store = vector.RecallIndexSpec().Name
	} else {
		other.store = ""
	}
	for _, current := range []embeddingsDaemonClient{other, client} {
		status, err := current.status(ctx)
		if err != nil {
			return err
		}
		if status.Running {
			if err := pollDaemonBuildStatus(ctx, out, current); err != nil {
				return err
			}
		}
	}
	if opts.FullRebuild && !opts.Yes && !confirmed {
		proceed, err := confirmFullRebuild(ctx, in, out, cfg, req.IncludeAutomated, req.Store)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}
	if err := prepareLocalEmbeddings(ctx, cfg.Vector.Embeddings, req.Using, in, out); err != nil {
		return err
	}
	return buildViaDaemon(ctx, out, client, req)
}
