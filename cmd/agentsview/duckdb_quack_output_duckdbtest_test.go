//go:build duckdbtest && !windows

package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartQuackServerDoesNotEmitAuthTokenToProcessOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "serve-no-token-fd-log.duckdb")
	bind := "quack:127.0.0.1:" + freeQuackServePort(t)
	const token = "agentsview-quack-serve-secret-token"

	server, err := sql.Open("duckdb", path)
	require.NoError(t, err)
	server.SetMaxOpenConns(1)
	server.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	_, err = server.ExecContext(ctx, "INSTALL quack")
	require.NoError(t, err)
	_, err = server.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err)

	stdout, stderr := captureProcessOutput(t, func() {
		_, err = startQuackServer(ctx, server, bind, token, false)
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, stopErr := server.ExecContext(context.Background(),
			`CALL quack_stop(?)`, bind)
		require.NoError(t, stopErr)
	})

	output := stdout + stderr
	assert.NotContains(t, output, token)
	assert.NotContains(t, strings.ToLower(output), "auth_token")
}

func captureProcessOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	oldStdout, err := syscall.Dup(int(os.Stdout.Fd()))
	require.NoError(t, err)
	oldStderr, err := syscall.Dup(int(os.Stderr.Fd()))
	require.NoError(t, err)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutDone := copyPipe(&stdoutBuf, stdoutR)
	stderrDone := copyPipe(&stderrBuf, stderrR)

	require.NoError(t, syscall.Dup2(int(stdoutW.Fd()), int(os.Stdout.Fd())))
	require.NoError(t, syscall.Dup2(int(stderrW.Fd()), int(os.Stderr.Fd())))
	defer func() {
		require.NoError(t, syscall.Dup2(oldStdout, int(os.Stdout.Fd())))
		require.NoError(t, syscall.Dup2(oldStderr, int(os.Stderr.Fd())))
		require.NoError(t, syscall.Close(oldStdout))
		require.NoError(t, syscall.Close(oldStderr))
	}()

	fn()

	require.NoError(t, syscall.Dup2(oldStdout, int(os.Stdout.Fd())))
	require.NoError(t, syscall.Dup2(oldStderr, int(os.Stderr.Fd())))
	require.NoError(t, stdoutW.Close())
	require.NoError(t, stderrW.Close())
	require.NoError(t, <-stdoutDone)
	require.NoError(t, <-stderrDone)
	require.NoError(t, stdoutR.Close())
	require.NoError(t, stderrR.Close())
	return stdoutBuf.String(), stderrBuf.String()
}

func copyPipe(dst *bytes.Buffer, src *os.File) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(dst, src)
		done <- err
	}()
	return done
}
