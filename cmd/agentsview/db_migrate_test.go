package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestDBMigrateJSONRequiresYes(t *testing.T) {
	testDataDir(t)

	cmd := newDBMigrateCommand()
	cmd.SetArgs(nil)
	err := cmd.Execute()
	require.EqualError(t, err, "db migrate requires --images")

	cmd = newDBMigrateCommand()
	cmd.SetArgs([]string{"--images", "--format", "json"})
	err = cmd.Execute()
	require.EqualError(t, err, "--format json requires --yes for db migrate --images")
}

func TestDBMigratePreviewClassifies(t *testing.T) {
	dataDir := testDataDir(t)
	seedMigrateCommandArchive(t)

	cmd := newDBMigrateCommand()
	cmd.SetArgs([]string{"--images", "--dry-run", "--format", "json"})
	var preview bytes.Buffer
	cmd.SetOut(&preview)
	require.NoError(t, cmd.Execute())

	var report db.StripImagesReport
	require.NoError(t, json.Unmarshal(preview.Bytes(), &report))
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(1), report.Payloads)
	assert.Positive(t, report.StoredBytes)
	assert.Positive(t, report.DecodedBytes)

	// Archive must be unchanged after a dry-run.
	assertMigrateCommandArchiveStillContainsImage(t)
	// Assets directory must not be created by a dry-run.
	assert.NoDirExists(t, filepath.Join(dataDir, "assets"))
	t.Logf("0 files created during preview: assets dir exists=%v", false)
}

// TestDBMigrateJSONApply covers the --yes --format json apply branch: the JSON
// report decodes with the expected session and payload counts, and the archive
// really carries an asset:// reference afterward.
func TestDBMigrateJSONApply(t *testing.T) {
	dataDir := testDataDir(t)
	seedMigrateCommandArchive(t)

	cmd := newDBMigrateCommand()
	cmd.SetArgs([]string{"--images", "--yes", "--format", "json"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())

	var report db.StripImagesReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(1), report.Payloads)
	assert.Equal(t, int64(len(`data:image/png;base64,AAEC`)), report.StoredBytes)
	assert.Equal(t, int64(3), report.DecodedBytes)
	require.Len(t, report.Projects, 1)
	assert.Equal(t, 1, report.Projects[0].Changed)

	assertMigrateCommandArchiveHasAssetRef(t)
	assert.DirExists(t, filepath.Join(dataDir, "assets"))
}

// TestDBMigrateInteractiveConfirmation covers the interactive branch both ways.
// Declining leaves the archive and the assets directory untouched, which is what
// docs/commands.md promises, and both runs state the backup obligation.
func TestDBMigrateInteractiveConfirmation(t *testing.T) {
	dataDir := testDataDir(t)
	seedMigrateCommandArchive(t)
	assetsDir := filepath.Join(dataDir, "assets")
	const backupLine = "Migrated images require a backup of the assets directory beside the archive."

	cmd := newDBMigrateCommand()
	cmd.SetArgs([]string{"--images"})
	cmd.SetIn(strings.NewReader("n\n"))
	var declined bytes.Buffer
	cmd.SetErr(&declined)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, declined.String(), "Image migration preview.")
	assert.Contains(t, declined.String(), backupLine)
	assert.Contains(t, declined.String(), "Aborted.")
	assert.NotContains(t, declined.String(), "Image migration completed.")
	assertMigrateCommandArchiveStillContainsImage(t)
	assert.NoDirExists(t, assetsDir)

	cmd = newDBMigrateCommand()
	cmd.SetArgs([]string{"--images"})
	cmd.SetIn(strings.NewReader("y\n"))
	var accepted, acceptedErr bytes.Buffer
	cmd.SetOut(&accepted)
	cmd.SetErr(&acceptedErr)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, acceptedErr.String(), backupLine)
	assert.Contains(t, accepted.String(), "Image migration completed.")
	assert.Contains(t, accepted.String(), "Changed: 1")
	assertMigrateCommandArchiveHasAssetRef(t)
	assert.DirExists(t, assetsDir)
}

func TestDBStripAndMigratePartialFailureReportsCommittedWork(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command func() *cobra.Command
		title   string
	}{
		{"strip", newDBStripCommand, "Image strip"},
		{"migrate", newDBMigrateCommand, "Image migration"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testDataDir(t)
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database, err := db.Open(cfg.DBPath)
			require.NoError(t, err)
			for _, id := range []string{"mig-pf-a", "mig-pf-b"} {
				insertSessionForStripTest(t, database, id)
				require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage(id)}))
			}
			// Sessions run in id order, one transaction each. Aborting the second
			// session's revision bump commits the first and stops the run mid-selection.
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				_, err := tx.Exec(`
					CREATE TRIGGER fail_mig_pf_b
					AFTER UPDATE OF transcript_revision ON sessions
					WHEN NEW.id = 'mig-pf-b'
					BEGIN
						SELECT RAISE(ABORT, 'forced failure for mig-pf-b');
					END`)
				return err
			}))
			require.NoError(t, database.Close())

			cmd := tt.command()
			cmd.SetArgs([]string{"--images", "--yes"})
			var output, errOutput bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&errOutput)
			require.Error(t, cmd.Execute())

			assert.Contains(t, errOutput.String(), tt.title+" stopped early.")
			assert.Contains(t, errOutput.String(), "Sessions: 1")
			assert.Contains(t, errOutput.String(), "Changed: 1")
			assert.NotContains(t, errOutput.String(), tt.title+" completed.")
			assert.NotContains(t, errOutput.String(), "Run db compact separately")
			assert.NotContains(t, output.String(), tt.title+" completed.")
			t.Logf("partial report:\n%s", errOutput.String())
			database, err = db.Open(cfg.DBPath)
			require.NoError(t, err)
			defer func() { require.NoError(t, database.Close()) }()
			committed, err := database.GetAllMessages(t.Context(), "mig-pf-a")
			require.NoError(t, err)
			require.Len(t, committed, 1)
			assert.NotContains(t, committed[0].ToolCalls[0].ResultContent, "input_image")
			failed, err := database.GetAllMessages(t.Context(), "mig-pf-b")
			require.NoError(t, err)
			require.Len(t, failed, 1)
			assert.Contains(t, failed[0].ToolCalls[0].ResultContent, "input_image")
		})
	}
}

// The archive reference and digest must identify the bytes in the asset store.
func TestDBMigrateReferenceMatchesAssetsPut(t *testing.T) {
	dataDir := testDataDir(t)
	seedMigrateCommandArchive(t)

	cmd := newDBMigrateCommand()
	cmd.SetArgs([]string{"--images", "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())

	block := migrateCommandImageBlock(t)
	var ref, sha256Hex string
	require.NoError(t, json.Unmarshal(block["image_ref"], &ref))
	require.NoError(t, json.Unmarshal(block["sha256"], &sha256Hex))

	assetsDir := filepath.Join(dataDir, "assets")
	entries, err := os.ReadDir(assetsDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	stored, err := os.ReadFile(filepath.Join(assetsDir, entries[0].Name()))
	require.NoError(t, err)
	sum := sha256.Sum256(stored)
	assert.Equal(t, fmt.Sprintf("%x", sum[:]), sha256Hex,
		"sha256 must be the digest of the bytes assets.Put stored")
	assert.Equal(t, "asset://"+entries[0].Name(), ref)
	assert.Equal(t, sha256Hex+".png", entries[0].Name())
}

// TestDBMigrateReachesTrashedAndOrphanRows verifies that the command migrates
// sessions regardless of deletion state, and that a true orphan tool_result_events
// row (no matching tool_calls row) is also migrated and counted.
func TestDBMigrateReachesTrashedAndOrphanRows(t *testing.T) {
	dataDir := testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)

	// Session A: active.
	insertSessionForStripTest(t, database, "mig-active")
	require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage("mig-active")}))

	// Session B: trashed (soft-deleted).
	insertSessionForStripTest(t, database, "mig-trashed")
	require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage("mig-trashed")}))
	require.NoError(t, database.SoftDeleteSession("mig-trashed"))

	// Session C: a true orphan tool_result_events row with no matching tool_calls row.
	// This exercises the UNION ALL branch that reads tool_result_events directly.
	insertSessionForStripTest(t, database, "mig-orphan")
	orphanContent := `[{"type":"input_image","image_url":"data:image/gif;base64,AAEC"}]`
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index,
				 tool_use_id, agent_id, subagent_session_id,
				 source, status, content, content_length, timestamp, event_index)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"mig-orphan", 9, 0, "toolu-orphan-cli", "agent-cli", "",
			"subagent_notification", "completed", orphanContent, len(orphanContent),
			"2026-01-01T00:00:00Z", 0,
		)
		return err
	}))

	require.NoError(t, database.Close())

	cmd := newDBMigrateCommand()
	cmd.SetArgs([]string{"--images", "--yes"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, output.String(), "Image migration completed.")
	// 3 sessions changed (active, trashed, orphan).
	assert.Contains(t, output.String(), "Changed: 3")
	assert.Contains(t, output.String(), "Image payloads: 3")
	assert.DirExists(t, filepath.Join(dataDir, "assets"))
	// active and trashed share bytes (PNG); orphan is a GIF — two files.
	entries, err := os.ReadDir(filepath.Join(dataDir, "assets"))
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	database2, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database2.Close()) }()
	for _, id := range []string{"mig-active", "mig-trashed"} {
		messages, err := database2.GetAllMessages(context.Background(), id)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
		assert.Contains(t, messages[0].ToolCalls[0].ResultContent, "image_ref")
	}

	// True orphan event row is migrated.
	var orphanStored string
	require.NoError(t, database2.Reader().QueryRow(
		"SELECT content FROM tool_result_events WHERE session_id = ?", "mig-orphan",
	).Scan(&orphanStored))
	assert.Contains(t, orphanStored, "image_ref")
	assert.Contains(t, orphanStored, "asset://")
	assert.NotContains(t, orphanStored, "input_image")

	// Count trashed-session event rows that were migrated.
	var trashedMigrated int
	require.NoError(t, database2.Reader().QueryRow(`
		SELECT COUNT(*) FROM tool_result_events e
		JOIN sessions s ON s.id = e.session_id
		WHERE s.deleted_at IS NOT NULL AND e.content LIKE '%image_ref%'`).Scan(&trashedMigrated))
	assert.Equal(t, 1, trashedMigrated)
}

func seedMigrateCommandArchive(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	insertSessionForStripTest(t, database, "mig-cmd")
	require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage("mig-cmd")}))
}

func assertMigrateCommandArchiveStillContainsImage(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	messages, err := database.GetAllMessages(context.Background(), "mig-cmd")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
}

// assertMigrateCommandArchiveHasAssetRef verifies the archive content was
// rewritten to use an asset:// reference.
func assertMigrateCommandArchiveHasAssetRef(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	messages, err := database.GetAllMessages(context.Background(), "mig-cmd")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	content := messages[0].ToolCalls[0].ResultContent
	assert.NotContains(t, content, "input_image")
	assert.Contains(t, content, "image_ref")
	assert.Contains(t, content, "asset://")
}

// migrateCommandImageBlock returns the migrated image block of the seeded
// mig-cmd session.
func migrateCommandImageBlock(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	messages, err := database.GetAllMessages(context.Background(), "mig-cmd")
	require.NoError(t, err)
	require.Len(t, messages, 1)

	var blocks []json.RawMessage
	require.NoError(t, json.Unmarshal(
		[]byte(messages[0].ToolCalls[0].ResultContent), &blocks,
	))
	for _, raw := range blocks {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		if _, ok := fields["image_ref"]; ok {
			return fields
		}
	}
	t.Fatal("session mig-cmd carries no migrated image block")
	return nil
}
