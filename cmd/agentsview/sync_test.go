package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestRunRemoteHosts_AttemptsAllAndCollectsFailures(t *testing.T) {
	hosts := []config.RemoteHost{
		{Host: "alpha"},
		{Host: "beta", User: "u", Port: 2222},
		{Host: "gamma"},
	}
	failBeta := errors.New("ssh down")

	var attempted []config.RemoteHost
	failures := runRemoteHosts(hosts, true, func(rh config.RemoteHost, full bool) error {
		attempted = append(attempted, rh)
		assert.True(t, full, "full flag should propagate to syncFn")
		if rh.Host == "beta" {
			return failBeta
		}
		return nil
	})

	// Every host attempted, in declared order, even after a failure.
	require.Equal(t, hosts, attempted)
	// Only beta failed; its full RemoteHost (user/port) is preserved.
	require.Len(t, failures, 1)
	assert.Equal(t, hosts[1], failures[0].Host)
	assert.Equal(t, failBeta, failures[0].Err)
}

func TestRunRemoteHosts_AllSucceedReturnsEmpty(t *testing.T) {
	hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
	failures := runRemoteHosts(hosts, false, func(config.RemoteHost, bool) error {
		return nil
	})
	assert.Empty(t, failures)
}

func TestSyncLocalAndRemotes_ResyncForcesRemoteFull(t *testing.T) {
	tests := []struct {
		name      string
		cfgFull   bool
		didResync bool
		wantFull  bool
	}{
		{"no full, no resync", false, false, false},
		{"automatic resync forces remote full", false, true, true},
		{"cli --full", true, false, true},
		{"both", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
			localCalled := false
			var gotFull []bool
			failures := syncLocalAndRemotes(hosts, tt.cfgFull,
				func() bool { localCalled = true; return tt.didResync },
				func(_ config.RemoteHost, full bool) error {
					gotFull = append(gotFull, full)
					return nil
				})

			require.True(t, localCalled, "local sync must run")
			assert.Empty(t, failures)
			require.Len(t, gotFull, len(hosts))
			for _, full := range gotFull {
				assert.Equal(t, tt.wantFull, full)
			}
		})
	}
}

func TestPrintSyncProgressClearsShorterOverwrites(t *testing.T) {
	out := captureStdout(t, func() {
		printSyncProgress(agentsync.Progress{
			Detail: "Rebuilding search index",
			Hint:   "Rebuilding the search index may take a while on large archives.",
		})
		printSyncProgress(agentsync.Progress{
			Detail: "Swapping rebuilt database into place",
		})
	})

	require.GreaterOrEqual(t, strings.Count(out, "\x1b[K"), 2,
		"each carriage-return progress line must clear stale text")
	assert.Contains(t, out, "\r  Swapping rebuilt database into place\x1b[K")
}
