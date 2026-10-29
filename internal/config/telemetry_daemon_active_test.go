package config

import (
	"errors"
	"os"
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
	for _, tc := range []struct {
		name, id, record string
		wantSent         bool
	}{
		{"first content", "install-one", "install-one\n", true},
		{"restart", "install-one", "install-one\n", false},
		{"changed identity retries", "install-two", "install-two\n", true},
	} {
		cfg := Config{DataDir: dir, InstallationID: tc.id}
		sent := false
		err := cfg.ClaimSetupFinished(func() error {
			sent = true
			return nil
		})
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.wantSent, sent, tc.name)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, tc.record, string(data))
	}
}

func TestTelemetryClaimsDoNotBlockConfigSaves(t *testing.T) {
	for _, tc := range []struct {
		name          string
		claim         func(Config, func() error) error
		sendErr       error
		waitForReader bool
	}{
		{"daemon active", func(c Config, send func() error) error { return c.ClaimDaemonActive(time.Now(), send) }, nil, false},
		{"setup finished", func(c Config, send func() error) error { return c.ClaimSetupFinished(send) }, errors.New("queue full"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			started, release := make(chan struct{}), make(chan struct{})
			releaseSend := sync.OnceFunc(func() { close(release) })
			claimDone := make(chan struct{})
			var claimErr error
			var savesDone, readDone chan struct{}
			var claimed bool
			var readErr error
			go func() {
				claimErr = tc.claim(Config{DataDir: dir, InstallationID: "install-one"}, func() error {
					close(started)
					<-release
					return tc.sendErr
				})
				close(claimDone)
			}()
			defer func() {
				releaseSend()
				<-claimDone
				if readDone != nil {
					<-readDone
				}
				if savesDone != nil {
					<-savesDone
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "claim callback did not start")
			}
			if tc.waitForReader {
				readStarted := make(chan struct{})
				readDone = make(chan struct{})
				go func() {
					defer close(readDone)
					close(readStarted)
					readConfig := Config{DataDir: dir, InstallationID: "install-one"}
					claimed, readErr = readConfig.SetupFinishedClaimed()
				}()
				select {
				case <-readStarted:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "setup read did not start")
				}
				select {
				case <-readDone:
					require.FailNow(t, "setup read returned while the claim was pending", "claimed=%v err=%v", claimed, readErr)
				case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: timer shows the read waits while the claim holds the setup record lock
				}
			}
			saved := make(chan error, 1)
			savesDone = make(chan struct{})
			go func() {
				defer close(savesDone)
				cfg := Config{DataDir: dir}
				if err := cfg.SaveSettings(map[string]any{"chart_palette": ChartPaletteMatplotlib}); err != nil {
					saved <- err
					return
				}
				saved <- cfg.SaveTerminalConfig(TerminalConfig{Mode: "clipboard"})
			}()
			select {
			case err := <-saved:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "config saves waited for the claim callback")
			}
			<-savesDone
			cfg := readConfigFile(t, dir)
			assert.Equal(t, ChartPaletteMatplotlib, cfg.ChartPalette)
			assert.Equal(t, "clipboard", cfg.Terminal.Mode)
			releaseSend()
			<-claimDone
			if tc.waitForReader {
				<-readDone
				require.ErrorIs(t, claimErr, tc.sendErr)
				require.NoError(t, readErr)
				assert.False(t, claimed, "a failed send rolls back the reservation the read waited on")
			} else {
				require.NoError(t, claimErr)
			}
		})
	}
}
