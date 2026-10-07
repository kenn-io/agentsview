package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestRunDailyTelemetryPings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		var sends []string
		send := func() error {
			sends = append(sends, time.Now().UTC().Format("2006-01-02 15:04"))
			if len(sends) == 1 {
				return errors.New("queue full")
			}
			return nil
		}
		run := func(d time.Duration) {
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				runDailyTelemetryPings(ctx, time.Hour, cfg, send)
				close(done)
			}()
			time.Sleep(d)
			cancel()
			<-done
		}

		// The bubble clock starts at 2000-01-01 00:00 UTC.
		time.Sleep(13 * time.Hour)
		// A daemon that stays up for three days. Its first send fails, so it
		// retries an hour later, then reports each new UTC day at midnight.
		run(3 * 24 * time.Hour)
		// Restarts later on the same day find it already reported.
		for range 3 {
			run(30 * time.Second)
		}

		assert.Equal(t, []string{
			"2000-01-01 13:00",
			"2000-01-01 14:00",
			"2000-01-02 00:00",
			"2000-01-03 00:00",
			"2000-01-04 00:00",
		}, sends)
	})
}
