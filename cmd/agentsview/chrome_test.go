package main

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestChromeSetup(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	executable := filepath.Join(dir, "bin with spaces", "agentsview")
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if runtime.GOOS == "windows" {
		executable += ".cmd"
		program = "@echo off\r\necho %~1\r\necho %~2\r\necho %~3\r\n"
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0700))
	require.NoError(t, os.WriteFile(executable, []byte(program), 0700))
	assets := fstest.MapFS{
		"chrome-extension/manifest.json": {Data: []byte(`{"key":"dGVzdA=="}`)},
		"chrome-extension/worker.js":     {Data: []byte("worker")},
	}
	var registered string
	folder, err := setupChrome(dir, home, executable, assets, func(path string) error { registered = path; return nil })
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "chrome", "extension"), folder)
	worker, err := os.ReadFile(filepath.Join(folder, "worker.js"))
	require.NoError(t, err)
	assert.Equal(t, "worker", string(worker))
	body, err := os.ReadFile(registered)
	require.NoError(t, err)
	var manifest struct {
		Name           string   `json:"name"`
		Path           string   `json:"path"`
		Type           string   `json:"type"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest))
	assert.Equal(t, "io.kenn.agentsview", manifest.Name)
	assert.Equal(t, "stdio", manifest.Type)
	assert.Equal(t, []string{"chrome-extension://jpignaibiiemhngfjkcpokkamffknabf/"}, manifest.AllowedOrigins)
	assert.True(t, filepath.IsAbs(manifest.Path))
	command := exec.CommandContext(t.Context(), manifest.Path)
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(t.Context(), "cmd", "/c", manifest.Path)
	}
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Equal(t, []string{"chrome-host", "--socket", filepath.Join(dir, "chrome", "host.sock")}, strings.FieldsFunc(strings.TrimSpace(string(output)), func(r rune) bool { return r == '\r' || r == '\n' }))
	if runtime.GOOS == "linux" {
		assert.Equal(t, filepath.Join(home, ".config/google-chrome/NativeMessagingHosts/io.kenn.agentsview.json"), registered)
	}
	if runtime.GOOS == "darwin" {
		assert.Equal(t, filepath.Join(home, "Library/Application Support/Google/Chrome/NativeMessagingHosts/io.kenn.agentsview.json"), registered)
	}
}

func TestChromeHostRelay(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	socket := filepath.Join(dir, "host.sock")
	listener, err := daemon.Listen(t.Context(), daemon.Endpoint{Network: daemon.NetworkUnix, Address: socket})
	require.NoError(t, err)
	defer listener.Close()
	input, writeInput := io.Pipe()
	defer input.Close()
	defer writeInput.Close()
	readOutput, output := io.Pipe()
	defer readOutput.Close()
	defer output.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- relayChromeHost(ctx, socket, input, output) }()
	conn, err := listener.Accept()
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	frame := func(body string) []byte {
		var header [4]byte
		binary.NativeEndian.PutUint32(header[:], uint32(len(body)))
		return append(header[:], body...)
	}
	request := frame(`{"id":"a","path":"/api/organizations"}`)
	_, err = conn.Write(request)
	require.NoError(t, err)
	got, err := chromeFrame(readOutput)
	require.NoError(t, err)
	assert.Equal(t, request, got)
	answer := frame(`{"id":"a","status":200,"body":"[]"}`)
	_, err = writeInput.Write(answer)
	require.NoError(t, err)
	got, err = chromeFrame(conn)
	require.NoError(t, err)
	assert.Equal(t, answer, got)
	require.NoError(t, writeInput.Close())
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not exit on stdin EOF")
	}
}

func TestChromeHostRelayEOFWhileServerDown(t *testing.T) {
	require.NoError(t, relayChromeHost(t.Context(), filepath.Join(t.TempDir(), "missing.sock"), strings.NewReader(""), io.Discard))
}

func TestChromeSyncResults(t *testing.T) {
	for _, tt := range []struct {
		name, body, wantError string
		status                int
		imported              int
	}{
		{"done", "event: progress\ndata: {}\n\nevent: done\ndata: {\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":0}\n\n", "", 200, 2},
		{"absent host", `{"error":"Chrome host not connected"}`, "Chrome host not connected", 409, 0},
		{"error", "event: error\ndata: {\"error\":\"Sign in required\"}\n\n", "Sign in required", 200, 0},
		{"fetch", "event: fetch\ndata: {\"id\":\"a\",\"path\":\"/api/organizations\"}\n\n", "page relay", 200, 0},
		{"EOF", "event: progress\ndata: {}\n\n", "without a result", 200, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := readChromeSync(&http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))})
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.imported, stats.Imported)
			}
		})
	}
}

func TestChromeImportArguments(t *testing.T) {
	for _, tt := range []struct {
		name        string
		flags, args []string
		valid       bool
	}{
		{"sync", []string{"--type", "claude-ai", "--sync"}, nil, true},
		{"file", []string{"--type", "claude-ai"}, []string{"conversations.json"}, true},
		{"sync path", []string{"--type", "claude-ai", "--sync"}, []string{"conversations.json"}, false},
		{"sync provider", []string{"--type", "chatgpt", "--sync"}, nil, false},
		{"sync replace", []string{"--type", "claude-ai", "--sync", "--replace", "claude-ai:chat"}, nil, false},
		{"missing path", []string{"--type", "claude-ai"}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newImportCommand()
			require.NoError(t, cmd.ParseFlags(tt.flags))
			err := cmd.ValidateArgs(tt.args)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
