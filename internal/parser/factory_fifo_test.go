//go:build unix

package parser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFactoryParseIgnoresFIFOSettings(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-dev-demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	transcript := filepath.Join(dir, factoryFixtureID+".jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"type":"session_start","id":"`+factoryFixtureID+`","title":"t"}
{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
`), 0o644))
	require.NoError(t, syscall.Mkfifo(factorySettingsPath(transcript), 0o644))

	provider, ok := NewProvider(AgentFactory, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	type outcome struct {
		out ParseOutcome
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
		done <- outcome{out, err}
	}()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Len(t, got.out.Results, 1)
		assert.Empty(t, got.out.Results[0].Result.UsageEvents)
	case <-time.After(30 * time.Second):
		// Hang guard only: the parse finishes in milliseconds unless the
		// FIFO is opened, which blocks until a writer appears.
		require.FailNow(t, "parse blocked on a FIFO settings sidecar")
	}
}
