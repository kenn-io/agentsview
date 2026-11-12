package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestServeLifetimesApplyProtectedPathSetting(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("protected path probing is macOS-specific")
	}
	home, dir := isolateServeTestHome(t)
	skillDir := filepath.Join(home, "Documents", "example-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: discovered-name\n---\n"), 0o600))
	cfg, err := config.Load(nil)
	require.NoError(t, err)
	cfg.Host, cfg.Port, cfg.NoSync = "127.0.0.1", 0, true
	t.Cleanup(func() { parser.SetAllowProtectedPathProbes(false) })
	for _, tc := range []struct {
		allow bool
		want  string
	}{{true, "discovered-name"}, {false, "example-skill"}} {
		cfg.ScanProtectedPaths = tc.allow
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, serveOptions{}, 0) }()
		t.Cleanup(cancel)
		require.Eventually(t, func() bool { return FindDaemonRuntime(dir) != nil }, 30*time.Second, 100*time.Millisecond)
		path := filepath.Join(skillDir, "SKILL.md")
		_, _, _, _, _, calls, _ := parser.ExtractTextContent(t.Context(), gjson.Parse(`[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":`+strconv.Quote(path)+`}}]`))
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			require.FailNow(t, "server did not stop")
		}
		require.Len(t, calls, 1)
		require.Equal(t, tc.want, calls[0].SkillName)
	}
}
