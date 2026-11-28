//go:build darwin || linux

package parser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMusePersonalRejectsFIFOWithoutBlocking(t *testing.T) {
	if root := os.Getenv("AGENTSVIEW_MUSE_FIFO_TEST_ROOT"); root != "" {
		p, ok := NewProvider(AgentMusePersonal, ProviderConfig{Roots: []string{root}})
		require.True(t, ok)
		sources, err := p.Discover(t.Context())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assert.Equal(t, "muse-personal:"+musePersonalTestID, sources[0].Key)
		complete, err := p.(StreamingRawCaptureSourceProvider).DiscoverRawCaptureSourcesEach(t.Context(), func(SourceRef) error { return nil })
		require.NoError(t, err)
		assert.True(t, complete)
		_, found, err := p.FindSource(t.Context(), FindSourceRequest{RawSessionID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
		require.NoError(t, err)
		assert.False(t, found)
		return
	}
	_, healthy := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"healthy"}`))
	root := filepath.Dir(filepath.Dir(filepath.Dir(healthy)))
	path := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMusePersonalRejectsFIFOWithoutBlocking$")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_MUSE_FIFO_TEST_ROOT="+root)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "discovery must reject the FIFO without waiting for a writer: %s", output)
}

func TestMusePersonalUnreadableDirectoryIsNotAuthoritative(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not exclude root")
	}
	p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"fixture"}`))
	agent := filepath.Dir(filepath.Dir(path))
	require.NoError(t, os.Chmod(agent, 0))
	defer func() { require.NoError(t, os.Chmod(agent, 0o700)) }()
	_, err := p.Discover(t.Context())
	require.ErrorIs(t, err, os.ErrPermission)
	complete, err := p.(StreamingRawCaptureSourceProvider).DiscoverRawCaptureSourcesEach(t.Context(), func(SourceRef) error { return nil })
	require.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, complete)
}

func TestMusePersonalUnreadableTranscriptCannotReplaceDiscoveredSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not exclude root")
	}
	p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"preserved"}`))
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fp, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0))
	defer func() { require.NoError(t, os.Chmod(path, 0o600)) }()
	out, err := p.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fp})
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Empty(t, out.Results)
	assert.False(t, out.ResultSetComplete)
	assert.False(t, out.ForceReplace)
	complete, err := StreamRawCaptureSources(t.Context(), p, func(SourceRef) error { return nil })
	require.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, complete)
}
