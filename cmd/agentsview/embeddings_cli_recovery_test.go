package main

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/kit/embedlocal"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLiveEmbeddingsSetupCLIRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	dir := testDataDir(t)
	writeTestConfig(t, dir, "[vector]\nenabled=true\n")
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
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
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	installLocalEmbeddings = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		return embedlocal.Installation{}, errors.New("download interrupted")
	}
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	require.NoError(t, owner.Setup(0))
	<-owner.done
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "download interrupted", status.Error)
	installed.Store(true)
	status, err = owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.Action)
	assert.Empty(t, status.Error)
}
