package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimDaemonActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, telemetryDaemonActiveFilename)
	morning := time.Date(2026, 3, 9, 0, 30, 0, 0, time.UTC)
	// 20:00 in UTC-5 is 01:00 the next UTC day.
	nextDayInNewYork := time.Date(2026, 3, 9, 20, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))
	for _, tc := range []struct {
		name       string
		id         string
		now        time.Time
		sendErr    error
		wantSent   bool
		wantRecord string
	}{
		{"failed first send leaves no record", "install-one", morning, errors.New("queue full"), true, ""},
		{"first start of the day sends", "install-one", morning, nil, true, "install-one 2026-03-09\n"},
		{"restart on the same day skips", "install-one", morning.Add(23 * time.Hour), nil, false, "install-one 2026-03-09\n"},
		{"failed send keeps the day open", "install-one", nextDayInNewYork, errors.New("queue full"), true, "install-one 2026-03-09\n"},
		{"next UTC day sends after a failure", "install-one", nextDayInNewYork, nil, true, "install-one 2026-03-10\n"},
		{"new installation ID sends on the same day", "install-two", nextDayInNewYork, nil, true, "install-two 2026-03-10\n"},
	} {
		c := Config{DataDir: dir, InstallationID: tc.id}
		sent := false
		err := c.ClaimDaemonActive(tc.now, func() error {
			sent = true
			return tc.sendErr
		})
		if tc.sendErr != nil {
			require.ErrorIs(t, err, tc.sendErr, tc.name)
		} else {
			require.NoError(t, err, tc.name)
		}
		assert.Equal(t, tc.wantSent, sent, tc.name)
		if tc.wantRecord == "" {
			assert.NoFileExists(t, path, tc.name)
			continue
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.wantRecord, string(data), tc.name)
	}
}

func TestClaimDaemonActiveDoesNotSendWhenRecordIsUnreadable(t *testing.T) {
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	require.NoError(t, os.Mkdir(filepath.Join(c.DataDir, telemetryDaemonActiveFilename), 0o700))
	sent := false
	err := c.ClaimDaemonActive(time.Now(), func() error {
		sent = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, sent)
}

func TestClaimDaemonActiveDoesNotSendWhenRecordIsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory blocks file creation only for unprivileged Unix users")
	}
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	day := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	require.NoError(t, c.ClaimDaemonActive(day, func() error { return nil }))
	require.NoError(t, os.Chmod(c.DataDir, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(c.DataDir, 0o700)) })

	// Restarts on the next day can read the record but cannot reserve the day.
	sends := 0
	for range 3 {
		err := c.ClaimDaemonActive(day.Add(24*time.Hour), func() error {
			sends++
			return nil
		})
		require.Error(t, err)
	}
	assert.Zero(t, sends)
}

func TestClaimDaemonActiveConcurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	var sends atomic.Int32
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			c := Config{DataDir: dir, InstallationID: "install-one"}
			errs <- c.ClaimDaemonActive(now, func() error {
				sends.Add(1)
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, sends.Load())
}

func TestClaimSetupFinished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, telemetrySetupFinishedFilename)
	queueErr := errors.New("queue full")
	for _, tc := range []struct {
		name, id, record string
		sendErr          error
		wantSent         bool
	}{
		{"failed first send", "install-one", "", queueErr, true},
		{"first content", "install-one", "install-one\n", nil, true},
		{"restart", "install-one", "install-one\n", nil, false},
		{"changed identity fails", "install-two", "install-one\n", queueErr, true},
		{"changed identity retries", "install-two", "install-two\n", nil, true},
	} {
		cfg := Config{DataDir: dir, InstallationID: tc.id}
		sent := false
		err := cfg.ClaimSetupFinished(func() error {
			sent = true
			return tc.sendErr
		})
		require.ErrorIs(t, err, tc.sendErr, tc.name)
		assert.Equal(t, tc.wantSent, sent, tc.name)
		if tc.record == "" {
			assert.NoFileExists(t, path)
		} else {
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tc.record, string(data))
		}
	}
}

func TestClaimSetupFinishedAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("AGENTSVIEW_TEST_SETUP_CLAIM_DIR"); dir != "" {
		cfg := Config{DataDir: dir, InstallationID: "install-one"}
		require.NoError(t, cfg.ClaimSetupFinished(func() error {
			file, err := os.OpenFile(filepath.Join(dir, "sent"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = file.WriteString("setup_finished\n")
			return errors.Join(err, file.Close())
		}))
		return
	}
	dir := t.TempDir()
	for range 2 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClaimSetupFinishedAcrossProcesses$")
		cmd.Env = append(os.Environ(), "AGENTSVIEW_TEST_SETUP_CLAIM_DIR="+dir)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sent"))
	require.NoError(t, err)
	assert.Equal(t, "setup_finished\n", string(data))
}
