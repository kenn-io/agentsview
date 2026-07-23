package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/docbank"
)

func TestSyncGCCommandIsVaultMaintenanceNotFolderDeletion(t *testing.T) {
	cmd := newSyncGCCommand()
	assert.Equal(t, "gc", cmd.Use)
	assert.Error(t, cmd.Args(cmd, []string{"shared-folder"}),
		"maintenance must not accept an external folder path")
	assert.NotNil(t, cmd.Flags().Lookup("grace"))
	assert.NotNil(t, cmd.Flags().Lookup("quarantine-grace"))
	assert.NotNil(t, cmd.Flags().Lookup("max-objects"))
	maxBytes := cmd.Flags().Lookup("max-bytes")
	require.NotNil(t, maxBytes)
	assert.Contains(t, maxBytes.Usage, "garbage collection")
	assert.Contains(t, maxBytes.Usage, "repacking")
	assert.NotNil(t, cmd.Flags().Lookup("trash-cursor"))
	assert.NotNil(t, cmd.Flags().Lookup("gc-cursor"))
	assert.NotNil(t, cmd.Flags().Lookup("repack-cursor"))
}

func TestRunSyncGCDaemonUsesAuthenticatedLoopbackRoute(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/artifacts/maintenance", r.URL.Path)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"logical":{"origins":2,"deleted":3},"physical":{"supported":true,"result":{}}}`))
	}))
	defer server.Close()
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)

	err := runSyncGCDaemon(t.Context(), cmd, daemonRuntimeFromTestURL(t, server.URL),
		"secret", SyncGCConfig{
			Grace:           time.Hour + 1500*time.Nanosecond,
			QuarantineGrace: 2*time.Hour + 2500*time.Nanosecond,
			MaxObjects:      7, MaxBytes: 8, DryRun: true,
			TrashCursor: "trash-next", GCCursor: "gc-next", RepackCursor: "repack-next",
		})
	require.NoError(t, err)
	assert.Equal(t, "1h0m0.0000015s", requestBody["grace"])
	assert.Equal(t, "2h0m0.0000025s", requestBody["quarantine_grace"])
	assert.NotContains(t, requestBody, "grace_seconds")
	assert.NotContains(t, requestBody, "quarantine_grace_seconds")
	assert.Equal(t, float64(7), requestBody["max_objects"])
	assert.Equal(t, float64(8), requestBody["max_bytes"])
	assert.Equal(t, true, requestBody["dry_run"])
	assert.Equal(t, "trash-next", requestBody["trash_cursor"])
	assert.Equal(t, "gc-next", requestBody["gc_cursor"])
	assert.Equal(t, "repack-next", requestBody["repack_cursor"])
	assert.Contains(t, output.String(), "scanned 2 origin(s)")
}

func TestPrintArtifactMaintenanceSummaryReportsResumableStages(t *testing.T) {
	response := syncGCMaintenanceResponse{}
	response.Physical.Supported = true
	response.Physical.Result.EmptyTrash = artifact.MaintenanceResult{
		More: true, NextCursor: "trash'next",
	}
	response.Physical.Result.GarbageCollect = artifact.MaintenanceResult{
		More: true, NextCursor: "gc-next",
	}
	response.Physical.Result.Repack = artifact.MaintenanceResult{
		More: true, NextCursor: "repack-next",
	}
	var output bytes.Buffer

	printArtifactMaintenanceSummary(&output, response, SyncGCConfig{})

	assert.NotContains(t, output.String(), "physical maintenance complete")
	assert.Contains(t, output.String(), "--trash-cursor 'trash'\"'\"'next'")
	assert.Contains(t, output.String(), "--gc-cursor 'gc-next'")
	assert.Contains(t, output.String(), "--repack-cursor 'repack-next'")
}

func TestRunSyncGCDaemonResumeCommandPreservesEffectivePolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "logical":{},
            "physical":{"supported":true,"result":{
                "EmptyTrash":{"More":true,"NextCursor":"trash'next"},
                "GarbageCollect":{"More":true,"NextCursor":"gc-next"},
                "Repack":{"More":true,"NextCursor":"repack-next"}
            }}
        }`))
	}))
	defer server.Close()
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cfg := SyncGCConfig{
		Grace:           time.Hour + 1500*time.Nanosecond,
		QuarantineGrace: 2*time.Hour + 2500*time.Nanosecond,
		MaxObjects:      7, MaxBytes: 8,
	}

	err := runSyncGCDaemon(t.Context(), cmd, daemonRuntimeFromTestURL(t, server.URL), "", cfg)

	require.NoError(t, err)
	assert.Contains(t, output.String(),
		"  agentsview sync gc --grace '1h0m0.0000015s'"+
			" --quarantine-grace '2h0m0.0000025s'"+
			" --max-objects '7' --max-bytes '8'"+
			" --trash-cursor 'trash'\"'\"'next'"+
			" --gc-cursor 'gc-next' --repack-cursor 'repack-next'\n")
}

func TestRunSyncGCWithOwnershipNeverFallsBackFromDaemonOwner(t *testing.T) {
	cases := []struct {
		name    string
		runtime *DaemonRuntime
		active  bool
	}{
		{name: "read-only owner", runtime: &DaemonRuntime{ReadOnly: true}},
		{name: "unreachable writable owner", active: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			deps := syncGCDependencies{
				findDaemon:        func(string, ...string) *DaemonRuntime { return tc.runtime },
				localDaemonActive: func(string, ...string) bool { return tc.active },
				openRepository: func(context.Context, string) (*artifact.Repository, error) {
					opened = true
					return nil, nil
				},
			}
			cmd := &cobra.Command{}
			err := runSyncGCWith(cmd, config.Config{DataDir: t.TempDir()}, SyncGCConfig{}, deps)
			require.Error(t, err)
			assert.False(t, opened, "daemon ownership must prohibit direct vault fallback")
		})
	}
}

func TestRunSyncGCDirectUsesRepositoryLogicalAndPhysicalMaintenance(t *testing.T) {
	dataDir := t.TempDir()
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	err := runSyncGCWith(cmd, config.Config{DataDir: dataDir}, SyncGCConfig{
		Grace: time.Hour, QuarantineGrace: time.Hour,
		MaxObjects: 8, MaxBytes: 1 << 20,
	}, syncGCDependencies{
		findDaemon:        func(string, ...string) *DaemonRuntime { return nil },
		localDaemonActive: func(string, ...string) bool { return false },
		openRepository:    artifact.OpenRepository,
	})
	require.NoError(t, err)
	assert.Contains(t, output.String(), "scanned 0 origin(s)")
	assert.Contains(t, output.String(), "physical maintenance complete")
}

func TestRunSyncGCDirectRejectsOversizedBudgetBeforeLogicalRetention(t *testing.T) {
	_, err := runSyncGCDirect(t.Context(), nil, SyncGCConfig{
		MaxObjects: docbank.MaxMaintenanceObjects + 1,
	})

	assert.ErrorIs(t, err, artifact.ErrArtifactInvalid)
}

func TestRunSyncGCDirectPreservesExplicitZeroBudgets(t *testing.T) {
	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	response, err := runSyncGCDirect(t.Context(), repository, SyncGCConfig{})

	require.NoError(t, err)
	assert.True(t, response.Physical.Supported)
}
