package config

import (
	"errors"
	"os"
	"path/filepath"
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
		name        string
		id          string
		now         time.Time
		sendErr     error
		wantSent    bool
		wantClaimed bool
		wantRecord  string
	}{
		{"first start of the day sends", "install-one", morning, nil, true, true, "install-one 2026-03-09\n"},
		{"restart on the same day skips", "install-one", morning.Add(23 * time.Hour), nil, false, false, "install-one 2026-03-09\n"},
		{"failed send keeps the day open", "install-one", nextDayInNewYork, errors.New("queue full"), true, false, "install-one 2026-03-09\n"},
		{"next UTC day sends after a failure", "install-one", nextDayInNewYork, nil, true, true, "install-one 2026-03-10\n"},
		{"new installation ID sends on the same day", "install-two", nextDayInNewYork, nil, true, true, "install-two 2026-03-10\n"},
	} {
		c := Config{DataDir: dir, InstallationID: tc.id}
		sent := false
		claimed, err := c.ClaimDaemonActive(tc.now, func() error {
			sent = true
			return tc.sendErr
		})
		if tc.sendErr != nil {
			require.ErrorIs(t, err, tc.sendErr, tc.name)
		} else {
			require.NoError(t, err, tc.name)
		}
		assert.Equal(t, tc.wantSent, sent, tc.name)
		assert.Equal(t, tc.wantClaimed, claimed, tc.name)
		data, err := os.ReadFile(path)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.wantRecord, string(data), tc.name)
	}
}

func TestClaimDaemonActiveDoesNotSendWhenRecordIsUnreadable(t *testing.T) {
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	require.NoError(t, os.Mkdir(filepath.Join(c.DataDir, telemetryDaemonActiveFilename), 0o700))
	sent := false
	claimed, err := c.ClaimDaemonActive(time.Now(), func() error {
		sent = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, claimed)
	assert.False(t, sent)
}

func TestClaimDaemonActiveConcurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	var sends, claims atomic.Int32
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			c := Config{DataDir: dir, InstallationID: "install-one"}
			claimed, err := c.ClaimDaemonActive(now, func() error {
				sends.Add(1)
				return nil
			})
			errs <- err
			if claimed {
				claims.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, sends.Load())
	assert.EqualValues(t, 1, claims.Load())
}
