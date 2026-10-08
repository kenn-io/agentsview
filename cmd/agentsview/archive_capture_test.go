package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestArchiveCapturePortableSource(t *testing.T) {
	data := testDataDir(t)
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	const installation = "019eb791cf7d75c184399ed74c122e04"
	const session = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(installation))
	dbtest.WriteTestFile(t, filepath.Join(data, "assets", "kept.bin"), []byte("asset bytes"))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: session, Agent: "claude", Project: "project-a", Machine: installation}))
	_, err = database.StarSession(t.Context(), session)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(data, "sessions.db-wal"))
	root := t.TempDir()
	transcript := filepath.Join("projects", "project-a", session+".jsonl")
	body := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "retained question", session).String()
	dbtest.WriteTestFile(t, filepath.Join(root, transcript), []byte(body))
	dbtest.WriteTestFile(t, filepath.Join(root, "file-history", "version"), []byte("original text"))
	dbtest.WriteTestFile(t, filepath.Join(root, ".credentials.json"), []byte(`{"token":"synthetic-secret"}`))
	require.NoError(t, os.Symlink(filepath.Join(root, transcript), filepath.Join(root, "linked.jsonl")))
	target := filepath.Join(t.TempDir(), "capture")
	command := newRootCommand()
	var out, diagnostics bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostics)
	command.SetArgs([]string{"archive", "capture", target, "--root", "claude=" + filepath.Join(root, "projects"), "--writers-stopped"})
	require.NoError(t, command.ExecuteContext(t.Context()), diagnostics.String())
	var descriptor struct {
		Version        int                   `json:"version"`
		CaptureID      string                `json:"capture_id"`
		Source         rawarchive.ImportSpec `json:"source"`
		WritersStopped bool                  `json:"writers_stopped"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &descriptor))
	assert.Equal(t, 1, descriptor.Version)
	assert.Len(t, descriptor.CaptureID, 64)
	assert.Equal(t, installation, descriptor.Source.DeviceID)
	assert.True(t, descriptor.WritersStopped)
	spec, err := rawarchive.LoadImportSpec(t.Context(), filepath.Join(target, "capture.json"))
	require.NoError(t, err)
	var providerPath, applicationPath string
	for _, r := range spec.Roots {
		if r.Provider == "claude" {
			providerPath = r.Path
		}
		if r.ID == "application" {
			applicationPath = r.Path
		}
	}
	require.NotEmpty(t, providerPath)
	b, err := os.ReadFile(filepath.Join(providerPath, transcript))
	require.NoError(t, err)
	assert.Equal(t, body, string(b))
	assert.FileExists(t, filepath.Join(providerPath, "file-history", "version"))
	assert.NoFileExists(t, filepath.Join(providerPath, ".credentials.json"))
	assert.NoFileExists(t, filepath.Join(providerPath, "linked.jsonl"))
	require.NotEmpty(t, applicationPath)
	snapshot, err := sql.Open("sqlite3", "file:"+filepath.Join(applicationPath, "sessions.db")+"?mode=ro&immutable=1")
	require.NoError(t, err)
	defer snapshot.Close()
	var stars int
	require.NoError(t, snapshot.QueryRowContext(t.Context(), "SELECT count(*) FROM starred_sessions").Scan(&stars))
	assert.Equal(t, 1, stars, "capture includes committed WAL state")
	require.NoError(t, snapshot.Close())
	assert.FileExists(t, filepath.Join(applicationPath, "assets", "kept.bin"))
	assert.NoFileExists(t, filepath.Join(applicationPath, "config.toml"))
	// Moving the whole capture does not require editing paths or identities.
	moved := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(target, moved))
	movedSpec, err := rawarchive.LoadImportSpec(t.Context(), filepath.Join(moved, "capture.json"))
	require.NoError(t, err)
	assert.Equal(t, spec.Roots[0].ID, movedSpec.Roots[0].ID)
	assert.Equal(t, installation, movedSpec.DeviceID)
}
