package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
	"go.kenn.io/agentsview/internal/ledger/spoolrun"
)

func withLedgerZones(zones ...config.LedgerZoneConfig) setupOption {
	return func(c *config.Config) {
		c.Ledger.Enabled = true
		c.Ledger.Zones = zones
	}
}

func TestLedgerSpoolIngestRoute(t *testing.T) {
	t.Run("ingests_configured_zone", func(t *testing.T) {
		spool := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(spool, "incoming"), 0o755))
		seg := ledger.NewSegment("hostA", 1, time.Now())
		seg.Append(ledger.Event{
			EventID: ledger.NewEventID(), Zone: "default", Source: "hostA", SourceSeq: 1,
			Timestamp: time.Now().UTC(), EventClass: ledger.ClassHealth, PayloadTier: ledger.TierMetadataOnly,
		})
		require.NoError(t, seg.Seal())
		_, _, err := segfile.NewSpoolWriter(spool).Write(seg)
		require.NoError(t, err)
		te := setup(t, withLedgerZones(config.LedgerZoneConfig{ID: "default", SpoolPath: spool, SpoolAuthority: true}))

		w := te.post(t, "/api/v1/ledger/spool/ingest", `{}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var run spoolrun.LedgerSpoolIngestRun
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &run))
		require.Len(t, run.Zones, 1)
		assert.Equal(t, spoolrun.IngestStateIngested, run.Zones[0].State)
		assert.Equal(t, []string{"hostA-000001.json"}, run.Zones[0].Committed)
		assert.Empty(t, run.Zones[0].Failed)
		status, err := te.db.LedgerStatus(t.Context(), "default")
		require.NoError(t, err)
		assert.Equal(t, 1, status.Segments)
		assert.Equal(t, 1, status.Events)
		_, err = os.Stat(filepath.Join(spool, "processed", "hostA-000001.json"))
		assert.NoError(t, err)
	})

	t.Run("unknown_zone_is_bad_request", func(t *testing.T) {
		te := setup(t, withLedgerZones(config.LedgerZoneConfig{ID: "default"}))
		w := te.post(t, "/api/v1/ledger/spool/ingest", `{"zone":"nope"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "no zones matched")
	})

	// Review Focus 4.
	t.Run("non_localhost_is_forbidden", func(t *testing.T) {
		te := setup(t, withLedgerZones(config.LedgerZoneConfig{ID: "default", SpoolPath: t.TempDir(), SpoolAuthority: true}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/api/v1/ledger/spool/ingest", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.7:5555"
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}
