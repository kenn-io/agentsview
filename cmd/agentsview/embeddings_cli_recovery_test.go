package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedlocal"
)

func TestLiveEmbeddingsSetupCLIRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := testDataDir(t)
	writeTestConfig(t, dir, "[vector]\nenabled=true\n")
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	stub := newEmbeddingsStubServer(t, 768)
	defer stub.Close()
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
	})
	var installed atomic.Bool
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		if installed.Load() {
			return embedlocal.Status{}, nil
		}
		return embedlocal.Status{DownloadBytes: 295000000}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		if installed.Load() {
			return stub.URL + "/v1", nil
		}
		return "", embedlocal.ErrNotFound
	}
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		ok, err := opts.Confirm(ctx, 295000000)
		if err != nil {
			return embedlocal.Installation{}, err
		}
		if !ok {
			return embedlocal.Installation{}, errors.New("download was not approved")
		}
		installed.Store(true)
		return embedlocal.Installation{Executable: "kennembed", DataDir: opts.DataDir}, nil
	}
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	writeDaemonRuntimeForTest(t, dir, cfg.Host, cfg.Port, "test", false)
	embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"turn-on"}`), http.StatusAccepted)
	<-owner.done
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "download was not approved", status.Error)
	command := newEmbeddingsBuildCommand()
	command.SetContext(t.Context())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetIn(strings.NewReader("y\n"))
	command.SetArgs([]string{"--yes"})
	require.NoError(t, command.Execute())
	var build vector.BuildStatus
	raw := embeddingRequest(t, ts.Client(), ts.URL, http.MethodGet, "/api/v1/embeddings/status", "", nil, http.StatusOK)
	require.NoError(t, json.Unmarshal(raw, &build))
	require.NotNil(t, build.LastResult)
	require.True(t, build.LastResult.Activated)
	assert.Empty(t, build.Setup.Error, "a successful CLI recovery must clear the stale setup failure")
	assert.Empty(t, build.Setup.Action, "a built index must stop offering Turn on")
}
