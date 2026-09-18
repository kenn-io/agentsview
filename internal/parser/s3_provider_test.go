package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultS3ProviderSessionIDAndTempPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	p := DefaultS3Provider{
		Agent:      AgentCursor,
		IDPrefix:   "cursor:",
		Extensions: []string{".jsonl", ".txt"},
	}

	assert.Equal("cursor:abc", p.S3SessionID(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.jsonl",
	))
	assert.Equal("cursor:abc", p.S3SessionID(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.txt",
	))
	assert.Empty(p.S3SessionID("s3://bucket/laptop/raw/cursor/demo-proj/"))

	got, err := p.S3TempRelPath(
		"s3://bucket/laptop/raw/cursor/demo-proj/abc.jsonl",
	)
	require.NoError(err)
	assert.Equal(filepath.Join("demo-proj", "abc.jsonl"), got)

	got, err = p.S3TempRelPath(
		"s3://bucket/laptop/raw/cursor/demo-proj/agent-transcripts/abc/subagents/def.jsonl",
	)
	require.NoError(err)
	assert.Equal(filepath.Join("demo-proj", "agent-transcripts", "abc", "subagents", "def.jsonl"),
		got, "the materialized layout must keep the parent directory the parse derives the link from")

	_, err = p.S3TempRelPath(
		"s3://bucket/laptop/raw/cursor/demo-proj/../abc.jsonl",
	)
	require.Error(err)
	assert.Contains(err.Error(), "unsafe s3 object name")
}

func TestDefaultS3ProviderScannerKeepAndProject(t *testing.T) {
	assert := assert.New(t)

	scan := DefaultS3Provider{
		Agent:      AgentCursor,
		Extensions: []string{".jsonl", ".txt"},
	}.S3Scanner()

	assert.Equal(AgentCursor, scan.Agent)
	assert.True(scan.Keep("demo-proj/abc.jsonl", []string{"demo-proj", "abc.jsonl"}))
	assert.True(scan.Keep("demo-proj/abc.txt", []string{"demo-proj", "abc.txt"}))
	assert.False(scan.Keep("demo-proj/notes.md", []string{"demo-proj", "notes.md"}))
	assert.False(scan.Keep("abc.jsonl", []string{"abc.jsonl"}))
	assert.Equal("demo-proj", scan.Project("demo-proj/abc.jsonl", []string{"demo-proj", "abc.jsonl"}))
	assert.Nil(scan.Sidecars)
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
	assert := assert.New(t)

	assert.True(AgentSupportsS3Discovery(AgentClaude))
	assert.True(AgentSupportsS3Discovery(AgentCodex))
	assert.True(AgentSupportsS3Discovery(AgentCursor))
	assert.False(AgentSupportsS3Discovery(AgentTraeX))
	assert.False(AgentSupportsS3Discovery(AgentGrok))
	assert.False(AgentSupportsS3Discovery(AgentType("not-an-agent")))
}

func TestS3ProviderForRequiresS3DiscoveryCapability(t *testing.T) {
	assert := assert.New(t)

	provider, ok := S3ProviderFor(AgentTraeX)
	assert.False(ok)
	assert.Nil(provider)

	provider, ok = S3ProviderFor(AgentCursor)
	require.True(t, ok)
	assert.NotNil(provider)
}

func TestS3ProviderForCachedLookupDoesNotAllocate(t *testing.T) {
	require := require.New(t)

	provider, ok := S3ProviderFor(AgentCursor)
	require.True(ok)
	require.NotNil(provider)

	var cached S3Provider
	var found bool
	allocs := testing.AllocsPerRun(100, func() {
		cached, found = S3ProviderFor(AgentCursor)
	})

	require.True(found)
	require.NotNil(cached)
	assert.Zero(t, allocs)
}
