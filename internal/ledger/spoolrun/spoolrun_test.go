package spoolrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
)

func TestZonesFromConfig(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	dataDir := t.TempDir()
	cfg := config.LedgerConfig{Enabled: true, Zones: []config.LedgerZoneConfig{
		{ID: "default", SpoolPath: "~/spool/default", SpoolAuthority: true},
		{ID: "local-only"},
	}}

	t.Run("expands_home_and_keeps_order", func(t *testing.T) {
		zones, err := ZonesFromConfig(cfg, "", dataDir)
		require.NoError(t, err)
		assert.Equal(t, []Zone{
			{ID: "default", SpoolRoot: filepath.Join(home, "spool", "default"), Authority: true},
			{ID: "local-only"},
		}, zones)
	})

	t.Run("filter_by_zone", func(t *testing.T) {
		zones, err := ZonesFromConfig(cfg, "local-only", dataDir)
		require.NoError(t, err)
		assert.Equal(t, []Zone{{ID: "local-only"}}, zones)
	})

	t.Run("implicit_default_zone", func(t *testing.T) {
		zones, err := ZonesFromConfig(config.LedgerConfig{Enabled: true}, "", dataDir)
		require.NoError(t, err)
		assert.Equal(t, []Zone{{ID: "default"}}, zones)
	})

	t.Run("unmatched_zone_is_jilog_error", func(t *testing.T) {
		_, err := ZonesFromConfig(cfg, "nope", dataDir)
		require.Error(t, err)
		assert.Equal(t, `no zones matched (--zone Some("nope"), configured: ["default", "local-only"])`, err.Error())
	})

	t.Run("ledger_disabled_is_refused", func(t *testing.T) {
		_, err := ZonesFromConfig(config.LedgerConfig{Zones: cfg.Zones}, "", dataDir)
		require.EqualError(t, err, "[ledger] enabled = false; enable the ledger before using ledger spool")
	})

	// Review Focus 3.
	t.Run("data_dir_inside_spool_path_is_refused", func(t *testing.T) {
		spool := t.TempDir()
		inside := filepath.Join(spool, "agentsview-data")
		_, err := ZonesFromConfig(config.LedgerConfig{Enabled: true, Zones: []config.LedgerZoneConfig{
			{ID: "default", SpoolPath: spool},
		}}, "", inside)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contains the agentsview data directory")
	})
}

func TestIngestZonesAndRender(t *testing.T) {
	t.Run("ingest_without_fleet_store_is_a_loud_error", func(t *testing.T) {
		run, err := IngestZones(t.Context(), []Zone{{ID: "test-zone", SpoolRoot: t.TempDir()}}, newRecordingAppender())
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		err = run.Render(&out, &errOut)
		require.ErrorIs(t, err, ErrNotAuthority)
		assert.Contains(t, err.Error(), "not configured as the spool authority")
		assert.Equal(t, "spool ingest [test-zone]: spool_authority not set — skipping (producer host?)\n", out.String())
	})

	t.Run("disabled_zone_line", func(t *testing.T) {
		run, err := IngestZones(t.Context(), []Zone{{ID: "z"}}, newRecordingAppender())
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		require.ErrorIs(t, run.Render(&out, &errOut), ErrNotAuthority)
		assert.Equal(t, "spool ingest [z]: spool disabled (no spool_path) — not an ingest target\n", out.String())
	})

	t.Run("authority_prints_summary_and_fails_on_failures", func(t *testing.T) {
		spool := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(spool, "incoming"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(spool, "incoming", "hostA-000001.json"), []byte("not json"), 0o644))
		run, err := IngestZones(t.Context(), []Zone{{ID: "z", SpoolRoot: spool, Authority: true}}, newRecordingAppender())
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		err = run.Render(&out, &errOut)
		require.ErrorIs(t, err, ErrIngestFailures)
		assert.Contains(t, out.String(), "spool ingest [z]: Spool ingest: 0 committed, 0 skipped, 1 failed (1 total)\n\nFailures:\n  hostA-000001.json -- ledger error: serialization error: ")
		assert.Equal(t, "spool ingest finished with failed segments (left in incoming/)", err.Error())
	})
}

func TestEmitAndStatusZones(t *testing.T) {
	t.Run("emit_skips_spool_disabled_zone", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := EmitZones(t.Context(), &out, &errOut, []Zone{{ID: "test-zone"}}, newRecordingAppender(), "hostA", t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, "spool emit [test-zone]: spool disabled — skipped\n", out.String())
	})

	t.Run("emit_rejects_invalid_source_name", func(t *testing.T) {
		for _, bad := range []string{"../evil", "a/b", ".hidden"} {
			var out, errOut bytes.Buffer
			err := EmitZones(t.Context(), &out, &errOut, []Zone{{ID: "z", SpoolRoot: t.TempDir()}}, newRecordingAppender(), bad, t.TempDir())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid source name", "source %q", bad)
		}
		var out, errOut bytes.Buffer
		err := EmitZones(t.Context(), &out, &errOut, []Zone{{ID: "z", SpoolRoot: t.TempDir()}}, newRecordingAppender(), "", t.TempDir())
		require.EqualError(t, err, "cannot determine this host's ledger source (installation ID missing); refusing to emit — pass --source explicitly")
	})

	t.Run("status_lines_and_unhealthy_error", func(t *testing.T) {
		cursors := t.TempDir()
		cpath := segfile.CursorPath(cursors, "bad", "hostA")
		require.NoError(t, os.MkdirAll(filepath.Dir(cpath), 0o755))
		require.NoError(t, os.WriteFile(cpath, []byte("{"), 0o644))
		var out bytes.Buffer
		err := StatusZones(&out, []Zone{
			{ID: "off"},
			{ID: "good", SpoolRoot: t.TempDir(), Authority: true},
			{ID: "bad", SpoolRoot: t.TempDir()},
		}, cursors, "hostA")
		require.EqualError(t, err,
			"spool status found problems (unreadable dirs/entries, or corrupt cursor) in zone(s): bad")
		assert.Equal(t,
			"spool status [off]: spool disabled\n"+
				"spool status [good]: incoming=0 processed=0 cursor[hostA]=0 fleet_store=(this archive)\n"+
				"spool status [bad]: incoming=0 processed=0 cursor[hostA]=unreadable/corrupt (emit will restart from 0) fleet_store=(none — producer host)\n",
			out.String())
	})
}

// recordingAppender is a minimal Lister+Appender for orchestration tests.
type recordingAppender struct{ segs []ledger.Segment }

func newRecordingAppender() *recordingAppender { return &recordingAppender{} }
func (r *recordingAppender) ListLedgerSegments(
	context.Context, string, string, uint64, int,
) ([]ledger.Segment, error) {
	return nil, nil
}

func (r *recordingAppender) AppendLedgerSegment(
	_ context.Context, _ string, seg ledger.Segment, _ string,
) (ledger.PublishOutcome, error) {
	r.segs = append(r.segs, seg)
	return ledger.Published, nil
}

func (r *recordingAppender) LedgerSegmentSeqs(context.Context, string, string) ([]uint64, error) {
	return nil, nil
}

func (r *recordingAppender) LedgerStatus(_ context.Context, zone string) (ledger.ZoneStatus, error) {
	return ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}, nil
}
