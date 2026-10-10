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

func TestClaimScreenView(t *testing.T) {
	c := Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	today := time.Now().UTC().Format(time.DateOnly)
	for _, tc := range []struct {
		screen            string
		want              bool
		failure, identity string
	}{
		{"sessions", true, "", ""},
		{"sessions", false, "", ""},
		{"usage", true, "", ""},
		{"sessions", true, "", "install-two"},
		{"sessions", false, "enqueue", ""},
		{"sessions", true, "lock", ""},
		{"sessions", true, "write", ""},
	} {
		if tc.identity != "" {
			c.InstallationID = tc.identity
		}
		if tc.failure != "" {
			c.DataDir = t.TempDir()
		}
		path := filepath.Join(c.DataDir, telemetryScreensFilename)
		if tc.failure == "lock" {
			require.NoError(t, os.Mkdir(c.configPath()+".lock", 0o700))
		}
		sends := 0
		day, claimed, err := c.ClaimScreenView(tc.screen, func() error {
			sends++
			switch tc.failure {
			case "enqueue":
				return errors.New("enqueue failed")
			case "write":
				return os.Mkdir(path, 0o700)
			default:
				return nil
			}
		})
		assert.Equal(t, tc.want, claimed)
		assert.Equal(t, today, day)
		if tc.failure == "" {
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			wantScreens := "sessions"
			if tc.screen == "usage" {
				wantScreens += " usage"
			}
			assert.Equal(t, c.InstallationID+" "+today+" "+wantScreens+"\n", string(data))
			if tc.want {
				assert.Equal(t, 1, sends)
			} else {
				assert.Zero(t, sends)
			}
			continue
		}
		require.Error(t, err)
		switch tc.failure {
		case "lock", "write":
			assert.Equal(t, 1, sends)
		case "enqueue":
			assert.NoFileExists(t, path)
			day, claimed, err = c.ClaimScreenView(tc.screen, func() error { sends++; return nil })
			require.NoError(t, err)
			assert.Equal(t, today, day)
			assert.True(t, claimed)
			assert.Equal(t, 2, sends)
		}
	}
}

func TestClaimScreenViewConcurrent(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().UTC().Format(time.DateOnly)
	var wg sync.WaitGroup
	var sends atomic.Int32
	results := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			c := Config{DataDir: dir, InstallationID: "install-one"}
			day, claimed, err := c.ClaimScreenView("settings", func() error {
				sends.Add(1)
				return nil
			})
			results <- claimed
			assert.Equal(t, today, day)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	close(results)
	claims := 0
	for claimed := range results {
		if claimed {
			claims++
		}
	}
	assert.Equal(t, 1, claims)
	assert.EqualValues(t, 1, sends.Load())
}
