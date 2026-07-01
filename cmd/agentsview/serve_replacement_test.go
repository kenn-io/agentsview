package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
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
		cont, release, err := prepareForegroundServeDaemon(
			&config.Config{DataDir: dir}, serveReplacementOptions{},
		)
		t.Cleanup(release)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.Equal(t, os.Getpid(), stoppedPID)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Contains(t, out, "version 1.0.0")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonPreservesNoSyncWhenReplacingOlderDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		assert.True(t, rt.NoSync)
		RemoveDaemonRuntime(dir)
		return nil
	})

	cfg := config.Config{DataDir: dir}
	cont, release, err := prepareForegroundServeDaemon(
		&cfg, serveReplacementOptions{},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
	assert.True(t, cfg.NoSync)
}

func TestPrepareForegroundServeDaemonExplicitNoSyncOverridesRuntime(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		cfg config.Config, rt *DaemonRuntime,
	) error {
		assert.False(t, cfg.NoSync)
		assert.True(t, rt.NoSync)
		RemoveDaemonRuntime(dir)
		return nil
	})

	cfg := config.Config{DataDir: dir, NoSync: false}
	cont, release, err := prepareForegroundServeDaemon(
		&cfg,
		serveReplacementOptions{NoSyncExplicit: true},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
	assert.False(t, cfg.NoSync)
}

func TestPrepareForegroundServeDaemonMarksStartingBeforeStopping(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	var sawStarting bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		sawStarting = IsDaemonStarting(dir)
		RemoveDaemonRuntime(dir)
		return nil
	})

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	require.NoError(t, err)
	assert.True(t, cont)
	assert.True(t, sawStarting,
		"start marker must be visible before stopping old daemon")
	assert.True(t, IsDaemonStarting(dir),
		"replacement leaves marker held for runServe startup")
}

func TestPrepareForegroundServeDaemonRefusesReplacementWhenStartLockHeld(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	holdExternalDaemonStartLock(t, dir)

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t,
		"foreground replacement must not stop without owning start lock")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup")
	assert.NotNil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonRefusesReplacementWhenBackgroundLaunchHeld(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, launchLock.Unlock()) })

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t,
		"foreground replacement must not stop during background launch")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "background")
	assert.NotNil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonRefusesFreshStartWhenBackgroundLaunchHeld(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, launchLock.Unlock()) })

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "background")
	assert.False(t, IsDaemonStarting(dir))
}

func TestPrepareForegroundServeDaemonKeepsLaunchLockForFreshStart(
	t *testing.T,
) {
	dir := runtimeTestDir(t)

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	if ok {
		require.NoError(t, launchLock.Unlock())
	}
	assert.False(t, ok,
		"foreground startup must hold launch lock until startup handoff")

	release()
	launchLock, ok = acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	require.NoError(t, launchLock.Unlock())
}

func TestPrepareForegroundServeDaemonBackgroundChildUsesParentLaunchLock(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, launchLock.Unlock()) })
	t.Setenv(backgroundChildEnvVar, "1")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
}

func TestPrepareForegroundServeDaemonStopsUnderBackgroundLaunchLock(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		launchLock, ok := acquireBackgroundLaunchLock(dir)
		if ok {
			_ = launchLock.Unlock()
		}
		assert.False(t, ok,
			"foreground replacement stop must hold background launch lock")
		RemoveDaemonRuntime(dir)
		return nil
	})

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
}

func TestPrepareForegroundServeDaemonKeepsLaunchLockThroughDBOpen(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	dbPath := filepath.Join(dir, "sessions.db")
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		RemoveDaemonRuntime(dir)
		return nil
	})

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir, DBPath: dbPath},
		serveReplacementOptions{},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	if ok {
		require.NoError(t, launchLock.Unlock())
	}
	assert.False(t, ok,
		"foreground replacement must keep launch lock until startup handoff")

	database, writeLock, err := openWriteDB(
		context.Background(),
		config.Config{DataDir: dir, DBPath: dbPath},
	)
	require.NoError(t, err)
	require.NoError(t, writeLock.Close())
	require.NoError(t, database.Close())

	release()
	launchLock, ok = acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	require.NoError(t, launchLock.Unlock())
}

func TestPrepareForegroundServeDaemonUsesExistingCompatibleDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.1.0"),
	))
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t, "same-version daemon must be reused")

	out := captureStdout(t, func() {
		cont, release, err := prepareForegroundServeDaemon(
			&config.Config{DataDir: dir}, serveReplacementOptions{},
		)
		t.Cleanup(release)
		require.NoError(t, err)
		assert.False(t, cont)
	})

	assert.Contains(t, out, "agentsview already running")
	assert.Contains(t, out, "http://")
}

func TestServeDaemonReplacementDecisionRefusesCompatibleDowngrade(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.1.0"),
	))
	setTestVersion(t, "1.0.0")
	forbidStopDaemonRuntimeForUpgrade(t, "downgrade needs --replace")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementRefuse, decision.Action)
	assert.Contains(t, decision.Reason, "newer")
	assert.Contains(t, strings.Join(serveDaemonConflictLines(decision), "\n"),
		"--replace")
}

func TestServeDaemonReplacementDecisionRefusesGitDescribeDevBuild(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "v1.1.0-2-gabcdef")
	forbidStopDaemonRuntimeForUpgrade(t,
		"git-describe dev build needs --replace")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementRefuse, decision.Action)
	assert.Contains(t, decision.Reason, "dev build")
	assert.Contains(t, strings.Join(serveDaemonConflictLines(decision), "\n"),
		"--replace")
}

func TestServeDaemonReplacementDecisionRefusesCompatibleNonSemver(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("local-build"),
	))
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t, "non-semver daemon needs --replace")

	decision := decideServeDaemonReplacement(
		config.Config{DataDir: dir}, serveReplacementOptions{},
	)

	require.NotNil(t, decision.Runtime)
	assert.Equal(t, serveReplacementRefuse, decision.Action)
	assert.Contains(t, decision.Reason, "not newer")
	assert.Contains(t, strings.Join(serveDaemonConflictLines(decision), "\n"),
		"--replace")
}

func TestPrepareForegroundServeDaemonRefusesDevWithoutReplace(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port, withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	forbidStopDaemonRuntimeForUpgrade(t, "dev build needs --replace")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir}, serveReplacementOptions{},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev builds")
	assert.Contains(t, err.Error(), "--replace")
}

func TestPrepareForegroundServeDaemonReplaceStopsWritableDevConflict(t *testing.T) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
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
		cont, release, err := prepareForegroundServeDaemon(
			&config.Config{DataDir: dir},
			serveReplacementOptions{Replace: true},
		)
		t.Cleanup(release)
		require.NoError(t, err)
		assert.True(t, cont)
	})

	assert.True(t, stopped)
	assert.Contains(t, out, "Replacing agentsview daemon")
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestPrepareForegroundServeDaemonReplaceStopsConfirmedUnreachableDaemon(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	ln, port := freeTCPListener(t)
	require.NoError(t, ln.Close())
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", port, "1.0.0", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	require.Nil(t, FindDaemonRuntime(dir),
		"precondition: runtime record must be live but unprobeable")

	var stopped bool
	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		stopped = true
		assert.Equal(t, port, rt.Port)
		assert.True(t, stopTargetConfirmed(rt.Record, ""))
		RemoveDaemonRuntime(dir)
		return nil
	})

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)
	t.Cleanup(release)

	require.NoError(t, err)
	assert.True(t, cont)
	assert.True(t, stopped)
}

func TestPrepareForegroundServeDaemonBackstopRefusesSecondWritableDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	dbPath := filepath.Join(dir, "sessions.db")
	firstHost, firstPort := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		firstHost, firstPort, withRuntimeVersion("1.0.0"),
	))

	secondPID := startSleepProcess(t)
	secondHost, secondPort := testPingServer(t)
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		secondHost, secondPort,
		withRuntimePID(secondPID),
		withRuntimeVersion("1.0.0"),
	))
	require.NoError(t, err)
	setTestVersion(t, "1.1.0")

	stubStopDaemonRuntimeForUpgrade(t, func(
		_ config.Config, rt *DaemonRuntime,
	) error {
		assert.Equal(t, os.Getpid(), rt.Record.PID)
		if rt.Record.SourcePath != "" {
			require.NoError(t, os.Remove(rt.Record.SourcePath))
		}
		return nil
	})

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir, DBPath: dbPath},
		serveReplacementOptions{},
	)
	t.Cleanup(release)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	require.NoError(t, err)
	assert.True(t, cont)
	database, lock, err := openWriteDB(
		context.Background(),
		config.Config{DataDir: dir, DBPath: dbPath},
	)
	require.Error(t, err)
	assert.Nil(t, database)
	assert.Nil(t, lock)
	assert.Contains(t, err.Error(), "owns the SQLite archive")
}

func TestPrepareForegroundServeDaemonReplaceChecksTooNewDatabaseBeforeStop(t *testing.T) {
	dir := runtimeTestDir(t)
	dbPath := filepath.Join(dir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("9.9.9"),
		withRuntimeMetadata(runtimeDataVersion, strconv.Itoa(futureVersion)),
	))
	setTestVersion(t, "dev")
	forbidStopDaemonRuntimeForUpgrade(t,
		"too-new database must be rejected before stop")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir, DBPath: dbPath},
		serveReplacementOptions{Replace: true},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
	assert.False(t, IsDaemonStarting(dir),
		"failed precheck must not leave start marker held")
}

func TestPrepareForegroundServeDaemonReplaceChecksDatabaseEvenWithCurrentRuntimeData(t *testing.T) {
	dir := runtimeTestDir(t)
	dbPath := filepath.Join(dir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "dev")
	forbidStopDaemonRuntimeForUpgrade(t,
		"too-new database must be rejected before stop")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir, DBPath: dbPath},
		serveReplacementOptions{Replace: true},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	assert.NotNil(t, FindDaemonRuntime(dir))
	assert.False(t, IsDaemonStarting(dir))
}

func TestPrepareForegroundServeDaemonAutoReplaceChecksTooNewDatabaseBeforeStop(t *testing.T) {
	dir := runtimeTestDir(t)
	dbPath := filepath.Join(dir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	host, port := testPingServer(t)
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		host, port,
		withRuntimeVersion("1.0.0"),
	))
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t,
		"too-new database must be rejected before auto replacement stop")

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir, DBPath: dbPath},
		serveReplacementOptions{},
	)
	t.Cleanup(release)

	assert.False(t, cont)
	require.Error(t, err)
	assert.True(t, db.IsDataVersionTooNew(err))
	assert.NotNil(t, FindDaemonRuntime(dir))
	assert.False(t, IsDaemonStarting(dir))
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

	cont, release, err := prepareForegroundServeDaemon(
		&config.Config{DataDir: dir},
		serveReplacementOptions{Replace: true},
	)
	t.Cleanup(release)

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
