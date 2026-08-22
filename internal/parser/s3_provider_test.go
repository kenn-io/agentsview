package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultS3ProviderSessionIDAndTempPath(t *testing.T) {
	p := DefaultS3Provider{
		Agent:      AgentCursor,
		IDPrefix:   "cursor:",
		Extensions: []string{".jsonl", ".txt"},
	}

	assert.Equal(t, "cursor:abc", p.S3SessionID(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.jsonl",
	))
	assert.Equal(t, "cursor:abc", p.S3SessionID(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.txt",
	))
	assert.Empty(t, p.S3SessionID("s3://bucket/laptop/raw/cursor/demo-proj/"))

	got, err := p.S3TempRelPath(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.jsonl",
	)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("demo-proj", "abc.jsonl"), got)

	_, err = p.S3TempRelPath(
		"s3://bucket/laptop/raw/cursor/demo-proj/../abc.jsonl",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe s3 object name")
}

func TestDefaultS3ProviderScannerKeepAndProject(t *testing.T) {
	scan := DefaultS3Provider{
		Agent:      AgentCursor,
		Extensions: []string{".jsonl", ".txt"},
	}.S3Scanner()

	assert.Equal(t, AgentCursor, scan.Agent)
	assert.True(t, scan.Keep("demo-proj/abc.jsonl", []string{"demo-proj", "abc.jsonl"}))
	assert.True(t, scan.Keep("demo-proj/abc.txt", []string{"demo-proj", "abc.txt"}))
	assert.False(t, scan.Keep("demo-proj/notes.md", []string{"demo-proj", "notes.md"}))
	assert.False(t, scan.Keep("abc.jsonl", []string{"abc.jsonl"}))
	assert.Equal(t, "demo-proj", scan.Project("demo-proj/abc.jsonl", []string{"demo-proj", "abc.jsonl"}))
	assert.Nil(t, scan.Sidecars)
}

func TestDefaultS3ProviderStatSessionUsesPlainObjectStat(t *testing.T) {
	const uri = "s3://bucket/laptop/raw/cursor/demo-proj/abc.jsonl"
	oldStat := statS3Object
	t.Cleanup(func() { statS3Object = oldStat })
	statS3Object = func(got string) (S3Object, error) {
		require.Equal(t, uri, got)
		return S3Object{URI: uri, Size: 42}, nil
	}

	got, err := DefaultS3Provider{Agent: AgentCursor}.S3StatSession(uri)
	require.NoError(t, err)
	assert.Equal(t, int64(42), got.Size)
	assert.Equal(t, uri, got.URI)
}

func TestAgentSupportsS3Discovery(t *testing.T) {
	assert.True(t, AgentSupportsS3Discovery(AgentClaude))
	assert.True(t, AgentSupportsS3Discovery(AgentCodex))
	assert.True(t, AgentSupportsS3Discovery(AgentCursor))
	assert.False(t, AgentSupportsS3Discovery(AgentTraeX))
	assert.False(t, AgentSupportsS3Discovery(AgentGrok))
	assert.False(t, AgentSupportsS3Discovery(AgentType("not-an-agent")))
}

func TestS3ProviderForRequiresS3DiscoveryCapability(t *testing.T) {
	provider, ok := S3ProviderFor(AgentTraeX)
	assert.False(t, ok)
	assert.Nil(t, provider)

	provider, ok = S3ProviderFor(AgentCursor)
	require.True(t, ok)
	assert.NotNil(t, provider)
}
