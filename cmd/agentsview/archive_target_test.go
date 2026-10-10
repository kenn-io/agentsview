package main

import (
	"bytes"
	"database/sql"
	"fmt"
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

func TestArchiveCommandsRequireDedicatedTargetBeforeOpening(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sourceData := testDataDir(t)
	dbtest.WriteTestFile(t, filepath.Join(sourceData, "telemetry-install-id"), []byte(owner))
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(sourceData, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, source.Close())
	providerRoot := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(providerRoot, "project-a", "saved.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "captured history", "saved").String()))
	capture := filepath.Join(t.TempDir(), "capture")
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: sourceData, Destination: capture, Roots: []rawarchive.RootSpec{{Provider: "claude", Path: providerRoot}}, Settings: rawarchive.RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	commands := [][]string{{"archive", "import", "--spec", filepath.Join(capture, "capture.json")}, {"archive", "reparse", "--all"}}
	for _, mode := range []string{"missing", "ordinary", "dedicated"} {
		t.Run(mode, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")
			t.Setenv("AGENTSVIEW_DATA_DIR", target)
			path := filepath.Join(target, "sessions.db")
			var before []byte
			if mode != "missing" {
				dbtest.WriteTestFile(t, filepath.Join(target, "telemetry-install-id"), []byte(owner))
				database, err := db.OpenIsolatedContext(ctx, path)
				require.NoError(t, err)
				if mode == "dedicated" {
					require.NoError(t, database.EnableArchiveOnly(ctx))
				}
				require.NoError(t, database.Close())
				if mode == "ordinary" {
					// Even a schema-version change must not happen before refusal.
					closed, err := sql.Open("sqlite3", path)
					require.NoError(t, err)
					_, err = closed.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", db.CurrentDataVersion()-1))
					require.NoError(t, err)
					require.NoError(t, closed.Close())
					before, err = os.ReadFile(path)
					require.NoError(t, err)
				}
			}
			for _, args := range commands {
				command := newRootCommand()
				var output bytes.Buffer
				command.SetOut(&output)
				command.SetErr(&output)
				command.SetArgs(args)
				err := command.ExecuteContext(ctx)
				if mode == "dedicated" {
					require.NoError(t, err)
					continue
				}
				require.ErrorContains(t, err, "archive-only target")
				assert.Contains(t, err.Error(), "--seed")
				assert.NoDirExists(t, filepath.Join(target, rawarchive.Directory))
				if mode == "missing" {
					assert.NoDirExists(t, target)
				} else {
					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, after, "refusing an ordinary target must not migrate or modify it")
				}
			}
			if mode == "dedicated" {
				database, err := db.OpenReadOnly(ctx, path)
				require.NoError(t, err)
				defer database.Close()
				messages, err := database.GetAllMessages(ctx, "saved")
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "captured history", messages[0].Content)
			}
		})
	}
}
