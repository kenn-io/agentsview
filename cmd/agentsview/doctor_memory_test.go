package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/skills"
)

func init() {
	if runtime.GOOS != "windows" || os.Getenv("AGENTSVIEW_FAKE_AGENT") != "1" {
		return
	}
	executable, _ := os.Executable()
	if strings.EqualFold(filepath.Base(executable), "codex.exe") {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		fmt.Println(`{"id":1,"result":{}}`)
		scanner.Scan()
		scanner.Scan()
		fmt.Println(`{"id":2,"result":{"data":[{"name":"agentsview","tools":{"search_content":{}}}]}}`)
		for scanner.Scan() {
		}
	} else {
		fmt.Print(strings.ReplaceAll(os.Getenv("AGENTSVIEW_FAKE_RESPONSE"), `\n`, "\n"))
	}
	os.Exit(0)
}

func TestDoctorMemoryReadsLocalArchiveWithoutCreatingConfig(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	dataDir := testDataDir(t)
	database, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())

	out, err := executeCommand(newRootCommand(), "doctor", "memory", "--format", "json")
	require.NoError(t, err, "output: %s", out)

	var report doctorMemoryReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, "local", report.TargetSelection.Kind)
	assert.Equal(t, "sqlite", report.Target.Archive.Backend)
	_, statErr := os.Stat(filepath.Join(dataDir, "config.toml"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestDoctorMemoryReportsRemoteTargetAndClientState(t *testing.T) {
	setDoctorMemoryTestEnvironment(t, t.TempDir())
	dataDir := testDataDir(t)
	server := memoryStatusServer(t, service.MemoryStatus{
		Status:        service.MemoryPartial,
		ObservedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		ServerVersion: "v1.2.3",
		Archive: service.MemoryArchiveStatus{
			Backend: "postgresql", ReadOnly: true, Identity: "archive-1",
		},
		Lexical: service.MemoryCapabilityStatus{Status: service.MemoryReady},
		Semantic: service.MemoryVectorStatus{
			Status: service.MemoryPartial, Reason: "incomplete_generation",
			Generation: "gen-1", Embedded: new(int64(8)), Missing: new(int64(2)),
		},
		Sources: service.MemorySourceStatus{
			Status: service.MemoryUnknown, Reason: "source_telemetry_unavailable",
		},
	})
	pluginRoot := writeDoctorMemoryPlugin(t)

	out, err := executeCommand(newRootCommand(), "doctor", "memory",
		"--server", server.URL, "--plugin-root", pluginRoot, "--format", "json")
	require.NoError(t, err, "output: %s", out)

	var report doctorMemoryReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), "output: %s", out)
	assert.Equal(t, service.MemoryPartial, report.Status)
	assert.Equal(t, "server", report.TargetSelection.Kind)
	assert.Equal(t, "postgresql", report.Target.Archive.Backend)
	assert.Equal(t, service.MemoryReady, report.Client.Status)
	assert.Equal(t, service.MemoryReady, report.Client.Skill.Status)
	assert.Equal(t, service.MemoryReady, report.Client.MCP.Status)
	assert.Equal(t, service.MemoryReady, report.Client.Hook.Status)
	assert.NotContains(t, out, pluginRoot)
	entries, readErr := os.ReadDir(dataDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "remote diagnostics must not write local runtime state")
}

func TestDoctorMemoryOlderServerIsExplicitlyUnknown(t *testing.T) {
	setDoctorMemoryTestEnvironment(t, t.TempDir())
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	out, err := executeCommand(newRootCommand(), "doctor", "memory",
		"--server", server.URL, "--format", "json")
	require.NoError(t, err, "output: %s", out)

	var report doctorMemoryReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, service.MemoryUnknown, report.Target.Status)
	assert.Equal(t, "unsupported", report.Target.Lexical.Reason)
	assert.Equal(t, service.MemoryUnknown, report.Client.Status)
}

func TestDoctorMemoryPreservesAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	_, err := executeCommand(newRootCommand(), "doctor", "memory",
		"--server", server.URL, "--format", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
}

func TestDoctorMemoryFindsDuplicateStandaloneSkillWithoutPaths(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	pluginRoot := writeDoctorMemoryPlugin(t)

	pkg, err := skills.RenderPackage(skills.HarnessAgents, version, skills.Remote{})
	require.NoError(t, err)
	standalone := filepath.Join(home, pkg[0].RelativePath)
	require.NoError(t, os.MkdirAll(filepath.Dir(standalone), 0o755))
	require.NoError(t, os.WriteFile(standalone, []byte(pkg[0].Content), 0o644))

	client := inspectDoctorMemoryClient(pluginRoot, doctorMemoryTargetSelection{Kind: "local"})
	assert.Equal(t, service.MemoryPartial, client.Status)
	assert.Equal(t, "duplicate_standalone_install", client.Reason)
	require.Len(t, client.Standalone, 1)
	assert.Equal(t, "agents", client.Standalone[0].Harness)
	assert.Equal(t, "current", client.Standalone[0].State)
	encoded, err := json.Marshal(client)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), home)
}

func TestDoctorMemoryPluginMCPRequiresFocusedProfileInClaudeManifest(t *testing.T) {
	root := writeDoctorMemoryPlugin(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"agentsview-memory"}`), 0o644,
	))

	status := inspectDoctorMemoryPluginMCP(root)
	assert.Equal(t, service.MemoryPartial, status.Status)
	assert.Equal(t, "wrong_profile", status.Reason)
}

func TestDoctorMemoryReportsStandaloneTargetMismatch(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "codex", "")
	pkg, err := skills.RenderPackage(skills.HarnessAgents, version, skills.Remote{})
	require.NoError(t, err)
	path := filepath.Join(home, pkg[0].RelativePath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(pkg[0].Content), 0o644))

	out, err := executeCommand(newRootCommand(), "agent", "setup")
	require.NoError(t, err, out)
	client := inspectDoctorMemoryClient("", doctorMemoryTargetSelection{
		Kind: "server", server: "https://archive-b.invalid",
	})
	require.Len(t, client.Standalone, 1)
	assert.Equal(t, "target_mismatch", client.Standalone[0].Reason)
	assert.Equal(t, service.MemoryPartial, client.Status)
	client = inspectDoctorAgentSetup(t.Context(), client)
	assert.Equal(t, service.MemoryPartial, client.Status)
	assert.Equal(t, "standalone_target_mismatch", client.Reason)

	client = inspectDoctorMemoryClient("", doctorMemoryTargetSelection{Kind: "postgresql"})
	require.Len(t, client.Standalone, 1)
	assert.NotEqual(t, "target_mismatch", client.Standalone[0].Reason)
}

func TestDoctorMemoryHumanOutputSeparatesTargetAndClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedded *int64
		missing  *int64
	}{
		{name: "measured counts", embedded: new(int64(10)), missing: new(int64(0))},
		{name: "counts absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDoctorMemoryTestEnvironment(t, t.TempDir())
			server := memoryStatusServer(t, service.MemoryStatus{
				Status:        service.MemoryReady,
				ObservedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
				ServerVersion: "v1.2.3",
				Archive: service.MemoryArchiveStatus{
					Backend: "sqlite", Identity: "archive-1",
				},
				Lexical: service.MemoryCapabilityStatus{Status: service.MemoryReady},
				Semantic: service.MemoryVectorStatus{
					Status: service.MemoryReady, Generation: "gen-1", Embedded: tc.embedded, Missing: tc.missing,
				},
				Sources: service.MemorySourceStatus{
					Status: service.MemoryUnknown, Reason: "source_telemetry_unavailable",
				},
			})

			out, err := executeCommand(newRootCommand(), "doctor", "memory",
				"--server", server.URL)
			require.NoError(t, err)
			assert.Contains(t, out, "Memory Diagnostics")
			assert.Contains(t, out, "Target: server")
			assert.Contains(t, out, "Server version: v1.2.3")
			assert.Contains(t, out, "Archive: ready (sqlite)")
			if tc.embedded != nil {
				assert.Contains(t, out, "Semantic generation: gen-1 (10 embedded, 0 missing)")
			} else {
				assert.NotContains(t, out, "Semantic generation:")
			}
			assert.Contains(t, out, "Client integration: unknown")
			assert.Contains(t, out, "Native plugin root was not provided")
			assert.NotContains(t, out, server.URL)
		})
	}
}

func setDoctorMemoryTestEnvironment(t *testing.T, home string) {
	t.Helper()
	setTestHome(t, home)
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{
		"AGENTSVIEW_MEMORY_SERVER",
		"AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE",
		"AGENTSVIEW_MEMORY_PG",
		"AGENTSVIEW_SKILLS_SERVER",
		"AGENTSVIEW_SKILLS_SERVER_TOKEN_FILE",
		"PLUGIN_ROOT",
		"CLAUDE_PLUGIN_ROOT",
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
	} {
		t.Setenv(name, "")
	}
}

func fakeAgent(t *testing.T, name, response string) {
	t.Helper()
	if response == "" {
		response = "Scope: User config\\nStatus: Connected\\n"
	}
	if runtime.GOOS == "windows" {
		t.Setenv("AGENTSVIEW_FAKE_AGENT", "1")
		t.Setenv("AGENTSVIEW_FAKE_RESPONSE", response)
		executable, err := os.Executable()
		require.NoError(t, err)
		source, err := os.Open(executable)
		require.NoError(t, err)
		defer source.Close()
		target, err := os.Create(filepath.Join(os.Getenv("PATH"), name+".exe"))
		require.NoError(t, err)
		_, err = io.Copy(target, source)
		require.NoError(t, err)
		require.NoError(t, target.Close())
		return
	}
	script := "#!/bin/sh\nprintf '" + response + "'\n"
	if name == "codex" {
		script = "#!/bin/sh\nread line\nprintf '{\"id\":1,\"result\":{}}\\n'\nread line\nread line\nprintf '{\"id\":2,\"result\":{\"data\":[{\"name\":\"agentsview\",\"tools\":{\"search_content\":{}}}]}}\\n'\nwhile read line; do :; done\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("PATH"), name), []byte(script), 0o755))
}

func memoryStatusServer(t *testing.T, status service.MemoryStatus) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/memory/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(status))
	}))
	t.Cleanup(server.Close)
	return server
}

func writeDoctorMemoryPlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	artifacts, err := skills.RenderPluginPackage("0.1.0")
	require.NoError(t, err)
	for _, artifact := range artifacts {
		path := filepath.Join(root, artifact.RelativePath)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(artifact.Content), 0o644))
	}
	write := func(relative, body string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), mode))
	}
	write(".mcp.json", `{"mcpServers":{"agentsview":{"command":"agentsview","args":["mcp","--profile","memory"]}}}`, 0o644)
	write(".claude-plugin/plugin.json", `{"name":"agentsview-memory","mcpServers":{"agentsview":{"command":"agentsview","args":["mcp","--profile","memory"]}}}`, 0o644)
	write(".codex-plugin/plugin.json", `{"name":"agentsview-memory","skills":"./skills/","mcpServers":"./.mcp.json"}`, 0o644)
	write("hooks/hooks.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"scripts/session-start.sh"}]}]}}`, 0o644)
	write("scripts/session-start.sh", "#!/bin/sh\nexec agentsview memory session-start --hook\n", 0o755)
	return root
}

func TestDoctorMemoryReportsInstalledAgentSetup(t *testing.T) {
	home := t.TempDir()
	setDoctorMemoryTestEnvironment(t, home)
	fakeAgent(t, "claude", "")
	fakeAgent(t, "codex", "")
	out, err := executeCommand(newRootCommand(), "agent", "setup")
	require.NoError(t, err, out)
	server := memoryStatusServer(t, service.MemoryStatus{Status: service.MemoryReady, ObservedAt: time.Now()})
	out, err = executeCommand(newRootCommand(), "doctor", "memory", "--server", server.URL, "--format", "json")
	require.NoError(t, err, out)
	var report doctorMemoryReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Client.Rows, 4)
	for _, row := range report.Client.Rows {
		require.EqualValues(t, "ok", row.State)
	}
	for _, index := range []int{1, 3} {
		require.Equal(t, "mcp agentsview", report.Client.Rows[index].Piece)
	}
	require.NoError(t, os.Remove(filepath.Join(home, ".claude.json")))
	require.NoError(t, os.Remove(filepath.Join(home, ".codex", "config.toml")))
	out, err = executeCommand(newRootCommand(), "doctor", "memory", "--server", server.URL, "--format", "json")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, index := range []int{1, 3} {
		require.EqualValues(t, "missing", report.Client.Rows[index].State)
		require.Equal(t, "agentsview agent setup", report.Client.Rows[index].NextStep)
	}
	out, err = executeCommand(newRootCommand(), "doctor", "memory", "--server", server.URL)
	require.NoError(t, err, out)
	require.Contains(t, out, "Client integration: partial")
	require.Contains(t, out, "agentsview agent setup")
	require.NotContains(t, out, "agent_setup_incomplete")
}

func TestDoctorMemoryRedactsMCPFailure(t *testing.T) {
	for _, tc := range []struct {
		name, response, state string
	}{
		{"failed", "Scope: User config\\nStatus: Failed to connect\\nIssue: https://private.example/token\\n", "failed"},
		{"unknown", "Scope: User config\\nStatus: Waiting for https://private.example/token\\n", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setDoctorMemoryTestEnvironment(t, home)
			fakeAgent(t, "claude", tc.response)
			require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"agentsview":{"type":"stdio","command":"agentsview","args":["mcp","--profile","memory"]}}}`), 0o600))
			selection := doctorMemoryTargetSelection{Kind: "local"}
			client := inspectDoctorAgentSetup(t.Context(), inspectDoctorMemoryClient("", selection))
			require.Len(t, client.Rows, 2)
			require.EqualValues(t, tc.state, client.Rows[1].State)
			encoded, err := json.Marshal(client)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), home)
			require.NotContains(t, string(encoded), "private.example")
			var text bytes.Buffer
			require.NoError(t, writeDoctorMemoryReport(&text, doctorMemoryReport{Client: client}))
			require.NotContains(t, text.String(), home)
			require.NotContains(t, text.String(), "private.example")
		})
	}
}
