package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger/spoolrun"
)

func TestDecideLedgerSpoolRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tr       transport
		delegate bool
		errText  string
	}{
		{"writable_daemon_delegates", transport{Mode: transportHTTP}, true, ""},
		{"read_only_daemon_goes_direct", transport{Mode: transportHTTP, ReadOnly: true}, false, ""},
		{"no_daemon_goes_direct", transport{Mode: transportDirect}, false, ""},
		{
			"unresponsive_owner_refuses",
			transport{Mode: transportDirect, DirectReadOnly: true, DirectReason: "local daemon owns the SQLite archive but is not responding"},
			false, "cannot ingest directly: local daemon owns the SQLite archive but is not responding; use the daemon or stop it first",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delegate, err := decideLedgerSpoolRoute(tc.tr)
			if tc.errText != "" {
				require.EqualError(t, err, tc.errText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.delegate, delegate)
		})
	}
}

func TestRequestLedgerSpoolIngestDelegatesZoneToDaemon(t *testing.T) {
	type ingestRequest struct {
		Zone string `json:"zone"`
	}
	type requestRecord struct {
		Method        string
		Path          string
		Authorization string
		Body          ingestRequest
		Err           error
	}
	requests := make(chan requestRecord, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := requestRecord{
			Method:        r.Method,
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
		}
		requestBody, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(requestBody, &record.Body)
		}
		record.Err = err
		requests <- record
		if err != nil {
			http.Error(w, "invalid test request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"zones":[{"zone":"north","state":"ingested","committed":["host-000001.json"],"skipped":[],"failed":[]}]}`))
	}))
	defer server.Close()

	run, err := requestLedgerSpoolIngest(t.Context(), transport{URL: server.URL}, "secret", "north")
	require.NoError(t, err)
	request := <-requests
	require.NoError(t, request.Err)
	assert.Equal(t, http.MethodPost, request.Method)
	assert.Equal(t, "/api/v1/ledger/spool/ingest", request.Path)
	assert.Equal(t, "Bearer secret", request.Authorization)
	assert.Equal(t, ingestRequest{Zone: "north"}, request.Body)
	assert.Equal(t, spoolrun.LedgerSpoolIngestRun{Zones: []spoolrun.ZoneIngest{{
		Zone: "north", State: "ingested", Committed: []string{"host-000001.json"}, Skipped: []string{}, Failed: []spoolrun.Failure{},
	}}}, run)
}

func TestSpoolSourceAndCursorMissingInstallationID(t *testing.T) {
	dataDir := t.TempDir()
	source, cursor := spoolSourceAndCursor(config.Config{DataDir: dataDir}, ledgerSpoolFlags{})
	assert.Empty(t, source, "emit and status must report a missing source")
	assert.Equal(t, filepath.Join(dataDir, "ledger", "spool-cursors"), cursor)
}

func TestLedgerSpoolCommandsAcrossArchives(t *testing.T) {
	spool := t.TempDir()
	producer := testDataDir(t)
	writeLedgerTestConfig(t, producer, fmt.Sprintf("[ledger]\nenabled = true\nsource = %q\n[[ledger.zones]]\nid = %q\nspool_path = %q\n",
		"host-a", "default", spool))
	_, err := runLedger(t, "append", "--class", "health")
	require.NoError(t, err)

	out, err := runLedger(t, "spool", "emit")
	require.NoError(t, err)
	assert.Contains(t, out, "emitted=1")
	_, err = os.Stat(filepath.Join(spool, "incoming", "host-a-000001.json"))
	require.NoError(t, err)
	out, err = runLedger(t, "spool", "status", "--source", "host-a")
	require.NoError(t, err)
	assert.Contains(t, out, "incoming=1 processed=0 cursor[host-a]=1")

	authority := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", authority)
	writeLedgerTestConfig(t, authority, fmt.Sprintf("[ledger]\nenabled = true\nsource = %q\n[[ledger.zones]]\nid = %q\nspool_path = %q\nspool_authority = true\n",
		"host-b", "default", spool))
	out, err = runLedger(t, "spool", "ingest")
	require.NoError(t, err)
	assert.Contains(t, out, "1 committed, 0 skipped, 0 failed")
	out, err = runLedger(t, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "zone default: 1 segment(s), 1 event(s)")
}

func TestLedgerSpoolCommandShape(t *testing.T) {
	cmd := newLedgerSpoolCommand()
	names := []string{}
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
		assert.NotNil(t, sub.Flags().Lookup("zone"), "%s --zone", sub.Name())
	}
	assert.ElementsMatch(t, []string{"emit", "ingest", "status"}, names)
	emit, _, err := cmd.Find([]string{"emit"})
	require.NoError(t, err)
	assert.NotNil(t, emit.Flags().Lookup("source"))
	assert.NotNil(t, emit.Flags().Lookup("cursor-dir"))
}
