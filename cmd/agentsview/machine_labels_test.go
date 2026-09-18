package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestMachineLabelCatalogDiscardsPartialResult(t *testing.T) {
	var stderr bytes.Buffer
	wantErr := errors.New("catalog unavailable")

	got := machineLabelCatalog(
		t.Context(), &stderr,
		func(context.Context) (service.MachineLabelCatalog, error) {
			return service.MachineLabelCatalog{"partial-key": "Partial Label"}, wantErr
		},
	)

	assert.Empty(t, got)
	assert.Equal(t, "warning: machine labels unavailable: catalog unavailable\n",
		stderr.String())
}

func TestMachineLabelCatalogNilSuccessReturnsEmpty(t *testing.T) {
	var stderr bytes.Buffer

	got := machineLabelCatalog(
		t.Context(), &stderr,
		func(context.Context) (service.MachineLabelCatalog, error) { return nil, nil },
	)

	assert.NotNil(t, got)
	assert.Empty(t, got)
	assert.Empty(t, stderr.String())
}

func TestSessionListJSONDegradesWhenMachineCatalogUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_ = newAgentDataDir(t)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.URL.Path == "/api/v1/machines" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"catalog unavailable"}`)
			return
		}
		writeJSONResponse(w, `{"sessions":[{"id":"remote-session","machine":"machine-key"}],"total":1}`)
	}))
	t.Cleanup(server.Close)
	root := newRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"session", "list", "--server", server.URL, "--format", "json",
	})

	_, err := root.ExecuteC()
	require.NoError(err)
	var document sessionListDocument
	require.NoError(json.Unmarshal(stdout.Bytes(), &document))
	require.Len(document.Sessions, 1)
	assert.Equal("remote-session", document.Sessions[0].ID)
	assert.Empty(document.MachineLabels)
	assert.Contains(stderr.String(), "HTTP 500")
}

func TestMachineLabelCatalogHTTPNullBodyReturnsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = io.WriteString(w,
			`{"machines":[],"machine_labels":null,"machine_aliases":null}`,
		)
	}))
	t.Cleanup(server.Close)
	var stderr bytes.Buffer

	labels := machineLabelCatalog(
		t.Context(), &stderr,
		func(ctx context.Context) (service.MachineLabelCatalog, error) {
			return service.MachineLabels(
				ctx, servicehttp.NewHTTPBackend(server.URL, "", true, ""),
			)
		},
	)

	assert.Empty(t, labels)
	assert.Empty(t, stderr.String())
}

func TestSessionListHumanSkipsMachineCatalog(t *testing.T) {
	_ = newAgentDataDir(t)
	var machineRequests int
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.URL.Path == "/api/v1/machines" {
			machineRequests++
			return
		}
		writeJSONResponse(w, `{"sessions":[],"total":0}`)
	}))
	t.Cleanup(server.Close)

	out, err := executeCommand(
		newRootCommand(), "session", "list", "--server", server.URL,
	)

	require.NoError(t, err)
	assert.Zero(t, machineRequests)
	assert.NotEmpty(t, out)
}

func TestSessionListJSONIncludesMachineLabelCatalog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := newAgentDataDir(t)
	const machineKey = "machine-key"
	seedSessionsWithOpts(t, dataDir, sessionSeed{
		id:      "session-with-machine-label",
		project: "machine-label-project",
		mut: func(s *db.Session) {
			s.Machine = machineKey
		},
	})
	database, err := db.Open(sessionsDBPath(dataDir))
	require.NoError(err)
	require.NoError(database.SetSyncState(
		db.MachineLabelKeyPrefix+machineKey, "Build Host",
	))
	require.NoError(database.SetSyncState(
		db.MachineLabelKeyPrefix+"unrelated-machine", "Other Host",
	))
	require.NoError(database.Close())

	out, err := executeCommand(
		newRootCommand(), "session", "list", "--format", "json",
	)

	require.NoError(err)
	var document struct {
		Sessions      []db.Session      `json:"sessions"`
		MachineLabels map[string]string `json:"machine_labels"`
	}
	require.NoError(json.Unmarshal([]byte(out), &document))
	require.Len(document.Sessions, 1)
	assert.Equal(machineKey, document.Sessions[0].Machine)
	assert.Equal("Build Host", document.MachineLabels[machineKey])
	assert.NotContains(document.MachineLabels, "unrelated-machine")
}

func TestRunUsageDailyBreakdownJSONMachineLabelsFromDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := newAgentDataDir(t)
	const machineKey = "machine-key"
	ts := sessionUsageRuntimeServerWithMachines(t,
		`{"machines":["machine-key","unrelated-machine"],"machine_labels":{"machine-key":"Build Host","unrelated-machine":"Other Host"},"machine_aliases":{}}`,
		func(w http.ResponseWriter, r *http.Request) {
			writeUsageStreamResponse(t, w, r, `{
				"schema_version":6,
				"projects":{},
				"daily":[{"date":"2026-06-01","machineBreakdowns":[{"machineName":"machine-key"}]}],
				"totals":{}
			}`)
		},
	)
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON:      true,
			Breakdown: true,
			NoSync:    true,
			Since:     "2026-06-01",
			Until:     "2026-06-01",
			Timezone:  "UTC",
		})
	})

	var document usageDailyDocument
	require.NoError(json.Unmarshal([]byte(out), &document))
	require.Len(document.Daily, 1)
	require.Len(document.Daily[0].MachineBreakdowns, 1)
	assert.Equal(machineKey, document.Daily[0].MachineBreakdowns[0].MachineName)
	assert.Equal("Build Host", document.MachineLabels[machineKey])
	assert.NotContains(document.MachineLabels, "unrelated-machine")
}

func TestRunUsageDailyBreakdownJSONEmitsEmptyMachineLabels(t *testing.T) {
	dataDir := newAgentDataDir(t)
	ts := sessionUsageRuntimeServerWithMachines(t,
		`{"machines":[],"machine_labels":{},"machine_aliases":{}}`,
		func(w http.ResponseWriter, r *http.Request) {
			writeUsageStreamResponse(t, w, r, `{
				"schema_version":6,
				"projects":{},
				"daily":[{"date":"2026-06-01","machineBreakdowns":[]}],
				"totals":{}
			}`)
		},
	)
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out := captureStdout(t, func() {
		runUsageDaily(UsageDailyConfig{
			JSON: true, Breakdown: true, NoSync: true,
			Since: "2026-06-01", Until: "2026-06-01", Timezone: "UTC",
		})
	})

	assert.Contains(t, out, `"machine_labels": {}`)
}

func TestRunUsageDailySkipsMachineLabelsWithoutJSONBreakdown(t *testing.T) {
	tests := []struct {
		name string
		cfg  UsageDailyConfig
	}{
		{name: "json without breakdown", cfg: UsageDailyConfig{JSON: true}},
		{name: "human table", cfg: UsageDailyConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := newAgentDataDir(t)
			machineRequests := 0
			ts := sessionUsageRuntimeServerWithMachines(t,
				`{"machines":[],"machine_labels":{},"machine_aliases":{}}`,
				func(w http.ResponseWriter, r *http.Request) {
					writeUsageStreamResponse(t, w, r, sampleDailyUsageJSON)
				},
				func() { machineRequests++ },
			)
			registerSyncRouteTestRuntime(t, dataDir, ts.URL)
			cfg := tt.cfg
			cfg.NoSync = true
			cfg.Timezone = "UTC"

			captureStdout(t, func() { runUsageDaily(cfg) })

			assert.Zero(t, machineRequests)
		})
	}
}
