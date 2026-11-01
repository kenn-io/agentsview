package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"

	"go.kenn.io/agentsview/internal/config"
)

func TestLocalEmbeddingsBuildConsent(t *testing.T) {
	oldEndpoint, oldInstall := localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() { localEmbeddingEndpoint, installLocalEmbeddings = oldEndpoint, oldInstall })
	for _, daemon := range []bool{false, true} {
		for _, state := range []string{"declined", "approved", "running", "full rebuild"} {
			t.Run(fmt.Sprintf("daemon=%t/%s", daemon, state), func(t *testing.T) {
				approve := state != "declined"
				dir := testDataDir(t)
				writeTestConfig(t, dir, "[vector]\nenabled = true\n")
				seedEmbeddableArchive(t, dir)
				stub := newEmbeddingsStubServer(t, 768)
				t.Cleanup(stub.Close)
				installed, dispatched, starting := state == "running", false, false
				localEmbeddingEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
					assert.False(t, opts.SkipOllama)
					if !installed {
						return "", embedlocal.ErrNotFound
					}
					if starting {
						assert.Equal(t, "kennembed", opts.Executable)
						assert.NotEmpty(t, opts.DataDir)
						starting = false
					}
					return stub.URL + "/v1", nil
				}
				installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					require.False(t, installed, "a running server needs no installation")
					require.NotNil(t, opts.Confirm)
					ok, err := opts.Confirm(ctx, 290_000_000)
					require.NoError(t, err)
					if !ok {
						return embedlocal.Installation{}, errors.New("declined")
					}
					installed, starting = true, true
					return embedlocal.Installation{DataDir: opts.DataDir, Executable: "kennembed"}, nil
				}
				if daemon {
					startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
						"POST /api/v1/embeddings/build": func(w http.ResponseWriter, _ *http.Request) {
							assert.True(t, installed)
							dispatched = true
							w.WriteHeader(http.StatusAccepted)
						},
						"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"running":false}`) },
					})
				}
				var out bytes.Buffer
				answer := "n\n"
				if approve {
					answer = "y\n"
				}
				opts := EmbeddingsBuildOptions{Yes: true}
				if state == "full rebuild" {
					opts = EmbeddingsBuildOptions{FullRebuild: true}
					answer = "y\ny\n"
				}
				err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader(answer), opts)
				if state == "running" {
					assert.NotContains(t, out.String(), "Download")
				} else {
					assert.Contains(t, out.String(), "Download approximately 290 MB")
				}
				if approve {
					require.NoError(t, err)
					if !daemon {
						assert.Contains(t, out.String(), "Embedded 2 documents")
					}
				} else {
					require.EqualError(t, err, "declined")
					assert.NoFileExists(t, filepath.Join(dir, "vectors.db"))
				}
				assert.Equal(t, approve && daemon, dispatched)
				assert.Equal(t, approve, installed)
			})
		}
	}
}

func TestLocalEmbeddingsLazyDiscoveryAndRefresh(t *testing.T) {
	oldEndpoint, oldInstall := localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() { localEmbeddingEndpoint, installLocalEmbeddings = oldEndpoint, oldInstall })
	first := newEmbeddingsStubServer(t, 4)
	second := newEmbeddingsStubServer(t, 4)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	c := enabledVectorConfig(t).Vector.Embeddings
	c.Model = "nomic-embed-text"
	c.Servers = map[string]config.VectorEmbeddingsServerConfig{"shared": {Local: true, Timeout: "5s", MaxRetries: 1}}
	discoveries := 0
	localEmbeddingEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		assert.True(t, opts.SkipOllama)
		discoveries++
		if discoveries == 1 {
			return first.URL + "/v1", nil
		}
		return second.URL + "/v1", nil
	}
	installLocalEmbeddings = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		require.FailNow(t, "a running server needs no installation")
		return embedlocal.Installation{}, nil
	}
	encode, err := newVectorQueryEncoder(c, "")
	require.NoError(t, err)
	assert.Zero(t, discoveries)
	_, err = encode(t.Context(), []string{"hello"})
	require.NoError(t, err)
	first.Close()
	_, err = encode(t.Context(), []string{"hello"})
	require.Error(t, err)
	vectors, err := encode(t.Context(), []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 1, 1, 1}}, vectors)
	assert.Equal(t, 2, discoveries)

	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		require.NotNil(t, opts.Confirm)
		_, err := opts.Confirm(ctx, 290_000_000)
		return embedlocal.Installation{}, fmt.Errorf("install: %w", err)
	}
	encoders, err := vectorDocumentEncoderSet(c)
	require.NoError(t, err)
	_, err = encoders.ByName["shared"].Encode(t.Context(), []string{"hello"})
	require.EqualError(t, err, "local embeddings: run agentsview embeddings build to approve the first download")
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, os.ErrPermission, errors.New("install lock failed")} {
		installLocalEmbeddings = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
			return embedlocal.Installation{}, fmt.Errorf("install interrupted: %w", cause)
		}
		_, err = encoders.ByName["shared"].Encode(t.Context(), []string{"hello"})
		require.ErrorIs(t, err, cause)
	}
}

func TestLocalEmbeddingsDiscoveryDoesNotBlockCancellation(t *testing.T) {
	oldEndpoint := localEmbeddingEndpoint
	t.Cleanup(func() { localEmbeddingEndpoint = oldEndpoint })
	entered := make(chan struct{})
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	localEmbeddingEndpoint = func(ctx context.Context, _ embedlocal.Options) (string, error) {
		if ctx == firstCtx {
			close(entered)
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	c := enabledVectorConfig(t).Vector.Embeddings
	c.Servers = map[string]config.VectorEmbeddingsServerConfig{"shared": {Local: true}}
	encode, err := newVectorQueryEncoder(c, "")
	require.NoError(t, err)
	firstDone := make(chan error, 1)
	go func() {
		_, err := encode(firstCtx, []string{"first"})
		firstDone <- err
	}()
	defer func() {
		cancelFirst()
		<-firstDone
	}()
	<-entered
	secondCtx, cancelSecond := context.WithCancel(t.Context())
	cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		_, err := encode(secondCtx, []string{"second"})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		cancelFirst()
		<-secondDone
		require.FailNow(t, "canceled caller waited for another discovery")
	}
}

func TestLocalEmbeddingsAutomaticNomic(t *testing.T) {
	oldEndpoint := localEmbeddingEndpoint
	t.Cleanup(func() { localEmbeddingEndpoint = oldEndpoint })
	stub := newEmbeddingsStubServer(t, 4)
	t.Cleanup(stub.Close)
	localEmbeddingEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		assert.False(t, opts.SkipOllama)
		return stub.URL + "/v1", nil
	}
	c := enabledVectorConfig(t).Vector.Embeddings
	c.Model, c.DefaultServer, c.Servers = "nomic-embed-text", "", nil
	encoders, err := vectorDocumentEncoderSet(c)
	require.NoError(t, err)
	require.Equal(t, "automatic", encoders.Default)
	encoder, ok := encoders.ByName["automatic"]
	require.True(t, ok)
	vectors, err := encoder.Encode(t.Context(), []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 1, 1, 1}}, vectors)
	assert.Equal(t, 32, encoder.Settings.BatchSize)
	assert.Equal(t, 4, encoder.Settings.Concurrency)
}
