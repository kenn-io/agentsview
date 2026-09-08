package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func commandImageMessage(sessionID string) db.Message {
	content := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	return db.Message{
		SessionID: sessionID,
		Ordinal:   0,
		Role:      "assistant",
		ToolCalls: []db.ToolCall{{
			ToolUseID:     "call",
			ResultContent: content,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "call", Source: "tool", Status: "completed", Content: content,
			}},
		}},
	}
}

func TestDBStripCommandControls(t *testing.T) {
	dataDir := testDataDir(t)
	cmd := newDBStripCommand()
	cmd.SetArgs(nil)
	err := cmd.Execute()
	require.EqualError(t, err, "db strip requires --images")

	cmd = newDBStripCommand()
	cmd.SetArgs([]string{"--images", "--format", "json"})
	err = cmd.Execute()
	require.EqualError(t, err, "--format json requires --yes for db strip --images")

	seedCommandArchive(t)
	cmd = newDBStripCommand()
	cmd.SetArgs([]string{"--images", "--dry-run", "--format", "json"})
	var dryRun bytes.Buffer
	cmd.SetOut(&dryRun)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, dryRun.String(), `"changed":1`)
	assertCommandArchiveStillContainsImage(t)
	assert.NoFileExists(t, filepath.Join(dataDir, "config.toml"))

	cmd = newDBStripCommand()
	cmd.SetArgs([]string{"--images"})
	cmd.SetIn(strings.NewReader("n\n"))
	var declined bytes.Buffer
	cmd.SetErr(&declined)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, declined.String(), "Aborted.")
	assertCommandArchiveStillContainsImage(t)
	assert.NoFileExists(t, filepath.Join(dataDir, "config.toml"))

	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte("tool_result_images = \"drop\"\n"), 0o600,
	))
	loaded, err := config.LoadReadOnly()
	require.NoError(t, err)
	assert.Equal(t, config.ToolResultImagesDrop, loaded.ToolResultImages)

	cmd = newDBStripCommand()
	cmd.SetArgs([]string{"--images", "--yes"})
	var applied bytes.Buffer
	cmd.SetOut(&applied)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, applied.String(), "Image strip completed.")
	assert.Contains(t, applied.String(), "Changed: 1")
	assert.Contains(t, applied.String(), "project: 1 sessions, 1 changed, 1 payloads, 26 B stored, 3 B decoded")
	assertCommandArchiveHasNoImage(t)
}

func TestDBStripCommandJSONApply(t *testing.T) {
	dataDir := testDataDir(t)
	seedCommandArchive(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"),
		[]byte("tool_result_images = \"drop\"\n"), 0o600,
	))

	cmd := newDBStripCommand()
	cmd.SetArgs([]string{"--images", "--format", "json", "--yes"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())

	var report db.StripImagesReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	assert.Equal(t, 1, report.Sessions)
	assert.Equal(t, 1, report.Changed)
	assert.Equal(t, int64(26), report.StoredBytes)
	assert.Equal(t, int64(3), report.DecodedBytes)
	require.Len(t, report.Projects, 1)
	assert.Equal(t, 1, report.Projects[0].Changed)
	assertCommandArchiveHasNoImage(t)
}

func TestDBStripLeavesSourceFiles(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	path := t.TempDir() + "\\provider.jsonl"
	sourcePath := path
	insertSessionForStripTest(t, database, "source", func(s *db.Session) {
		s.FilePath = &sourcePath
	})
	require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage("source")}))
	database.SetToolResultImages(config.ToolResultImagesKeep)

	before := "provider transcript remains byte-for-byte unchanged"
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	report, err := database.StripToolImages(context.Background(), db.StripImagesFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Changed)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, string(after))
}

func TestStripThenCompactAccounting(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	insertSessionForStripTest(t, database, "accounting")
	require.NoError(t, database.InsertMessages([]db.Message{
		largeCommandImageMessage("accounting"),
	}))
	report, err := database.StripToolImages(context.Background(), db.StripImagesFilter{})
	require.NoError(t, err)
	var output strings.Builder
	require.NoError(t, writeDBStripReport(&output, report, false, false))
	assert.Contains(t, output.String(), "Stored content bytes:")
	assert.Contains(t, output.String(), "Decoded image bytes:")
	assert.NotContains(t, output.String(), "Reclaimed:")
	t.Logf("strip report:\n%s", output.String())

	before, err := database.EstimateCompact(context.Background())
	require.NoError(t, err)
	assert.Positive(t, before.FreeListBytes)

	result, err := database.Compact(context.Background(), db.CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, before.DatabaseBytes, result.Before.DatabaseBytes)
	assert.Equal(t, before.TotalBytes, result.Before.TotalBytes)
	assert.Positive(t, result.ReclaimedBytes)
	assert.Greater(t, result.Before.TotalBytes, result.After.TotalBytes)
	assert.Zero(t, result.After.FreeListCount)
	assert.Equal(t,
		result.Before.TotalBytes-result.After.TotalBytes,
		result.ReclaimedBytes,
	)
	compactStat, err := os.Stat(database.Path())
	require.NoError(t, err)
	assert.Equal(t, result.After.DatabaseBytes, compactStat.Size())

	var compactOutput strings.Builder
	require.NoError(t, writeDBCompactResult(&compactOutput, result, false))
	assert.Contains(t, compactOutput.String(), "Before:")
	assert.Contains(t, compactOutput.String(), "After:")
	assert.Contains(t, compactOutput.String(), "Reclaimed:")
	t.Logf("compact report:\n%sfile size: %d B", compactOutput.String(), compactStat.Size())
}

func largeCommandImageMessage(sessionID string) db.Message {
	content := `[{
  "type":"input_image",
  "image_url":"data:image/png;base64,` + strings.Repeat("AAEC", 1<<18) + `"
}]`
	message := commandImageMessage(sessionID)
	message.ToolCalls[0].ResultContent = content
	message.ToolCalls[0].ResultEvents[0].Content = content
	return message
}

func seedCommandArchive(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	insertSessionForStripTest(t, database, "command")
	require.NoError(t, database.InsertMessages([]db.Message{commandImageMessage("command")}))
}

func assertCommandArchiveStillContainsImage(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	messages, err := database.GetAllMessages(context.Background(), "command")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
}

func assertCommandArchiveHasNoImage(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	messages, err := database.GetAllMessages(context.Background(), "command")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.NotContains(t, messages[0].ToolCalls[0].ResultContent, "input_image")
}

func insertSessionForStripTest(
	t *testing.T, database *db.DB, id string, opts ...func(*db.Session),
) {
	t.Helper()
	session := db.Session{
		ID: id, Project: "project", Machine: "local", Agent: "codex", MessageCount: 1,
	}
	for _, opt := range opts {
		opt(&session)
	}
	require.NoError(t, database.UpsertSession(session))
}
