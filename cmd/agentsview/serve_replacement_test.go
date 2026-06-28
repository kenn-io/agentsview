package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestServeDaemonReplacementDecisionAutoReplacesOlderRelease(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementAuto, decision.Action)
	assert.Contains(t, decision.Reason, "older")
}

func TestPrepareForegroundServeDaemonAutoReplacesOlderDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	var stoppedPID int
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stoppedPID = rt.Record.PID
		RemoveDaemonRuntime(dir)
		return nil
	})

	out := captureStdout(t, func() {
		cont, err := prepareForegroundServeDaemon(
			config.Config{DataDir: dir}, serveReplacementOptions{},
		)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.Equal(t, os.Getpid(), stoppedPID)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Contains(t, out, "version 1.0.0")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonRefusesDevWithoutReplace(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	forbidStopDaemonRuntimeForUpgrade(t, "dev build needs --replace")

	cont, err := prepareForegroundServeDaemon(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev builds")
	assert.Contains(t, err.Error(), "--replace")
}

func TestPrepareForegroundServeDaemonReplaceStopsWritableDevConflict(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		RemoveDaemonRuntime(dir)
		return nil
	})

	out := captureStdout(t, func() {
		cont, err := prepareForegroundServeDaemon(
			config.Config{DataDir: dir},
			serveReplacementOptions{Replace: true},
		)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.True(t, stopped)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonReplaceLeavesReadOnlyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeReadOnly(true),
		withRuntimeAPIVersion(0),
	))
	forbidStopDaemonRuntimeForUpgrade(t, "read-only daemon must not be stopped")

	cont, err := prepareForegroundServeDaemon(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	require.NoError(t, err)
	assert.True(t, cont)
	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
}

func TestServeDaemonReplacementDecisionRefusesDevBuild(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementRefuse, decision.Action)
	assert.Contains(t, decision.Reason, "dev builds")
	assert.Contains(t, strings.Join(serveDaemonConflictLines(decision), "\n"),
		"--replace")
}

func TestServeDaemonReplacementDecisionReplaceOverridesForwardData(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("9.9.9"),
		withRuntimeMetadata(runtimeDataVersion,
			strconv.Itoa(db.CurrentDataVersion()+1)),
	))
	setTestVersion(t, "dev")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	assert.Equal(t, serveReplacementExplicit, decision.Action)
	require.Error(t, decision.CompatibilityErr)
}

func TestServeDaemonReplacementDecisionIgnoresReadOnlyDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeReadOnly(true),
		withRuntimeAPIVersion(0),
	))

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)

	assert.Equal(t, serveReplacementNone, decision.Action)
	assert.Nil(t, decision.Runtime)
}

func TestServeDaemonReplacementLinesIncludeRuntimeDetails(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)
	lines := strings.Join(serveDaemonReplacementLines(decision), "\n")

	assert.Contains(t, lines, "will replace")
	assert.Contains(t, lines, "http://")
	assert.Contains(t, lines, "pid")
	assert.Contains(t, lines, "daemon version")
	assert.Contains(t, lines, "binary version")
	assert.Contains(t, lines, "API version")
	assert.Contains(t, lines, "data version")
	assert.Contains(t, lines, "serve stop")
}

func TestServeCommandHasReplaceFlag(t *testing.T) {
	cmd := newServeCommand()

	flag := cmd.Flags().Lookup("replace")

	require.NotNil(t, flag)
	assert.Equal(t,
		"Replace a running local daemon before starting",
		flag.Usage,
	)
}
