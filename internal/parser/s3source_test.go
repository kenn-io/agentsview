package parser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseS3URI(t *testing.T) {
	cases := []struct {
		uri, bucket, key string
	}{
		{"s3://bucket/a/b.jsonl", "bucket", "a/b.jsonl"},
		{"s3://bucket/", "bucket", ""},
		{"s3://bucket", "bucket", ""},
		{"s3://b/c/d/e.jsonl", "b", "c/d/e.jsonl"},
	}
	for _, c := range cases {
		b, k := parseS3URI(c.uri)
		assert.Equal(t, c.bucket, b, c.uri)
		assert.Equal(t, c.key, k, c.uri)
	}
}

func TestS3MachineFromRoot(t *testing.T) {
	cases := []struct {
		root, want string
	}{
		{"s3://bkt/harvest/laptop/raw/claude", "laptop"},
		{"s3://bkt/x/y/coder-gpu1/raw/codex", "coder-gpu1"},
		{"s3://bkt/archive/raw/laptop/raw/claude", "laptop"},
		{"s3://bkt/raw/claude", ""},      // nothing before "raw"
		{"s3://bkt/sessions/claude", ""}, // no "raw" segment
		{"s3://bkt", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, s3MachineFromRoot(c.root), c.root)
	}
}

func TestS3CredentialsIncludeSessionToken(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "session-token")

	got, err := s3Credentials().GetWithContext(nil)

	require.NoError(t, err)
	assert.Equal(t, "access-key", got.AccessKeyID)
	assert.Equal(t, "secret-key", got.SecretAccessKey)
	assert.Equal(t, "session-token", got.SessionToken)
}

func TestDiscoverCodexS3RequiresFullRootPrefix(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	mtime := time.Unix(100, 0)
	listS3Objects = func(root string) ([]S3Object, error) {
		require.Equal(t, "s3://bucket/root/codex", root)
		return []S3Object{
			{
				URI:          "s3://bucket/root/codex/2026/06/24/rollout-2026-06-24T00-00-00-good.jsonl",
				Size:         11,
				LastModified: mtime,
			},
			{
				URI:          "s3://bucket/root/codex-backup/rollout-2026-06-24T00-00-00-backup.jsonl",
				Size:         22,
				LastModified: mtime.Add(time.Second),
			},
			{
				URI:          "s3://bucket/root/codex2/rollout-2026-06-24T00-00-00-two.jsonl",
				Size:         33,
				LastModified: mtime.Add(2 * time.Second),
			},
		}, nil
	}

	got := discoverCodexS3("s3://bucket/root/codex")
	require.Len(t, got, 1)
	assert.Equal(t, "s3://bucket/root/codex/2026/06/24/rollout-2026-06-24T00-00-00-good.jsonl", got[0].Path)
	assert.Equal(t, int64(11), got[0].SourceSize)
	assert.Equal(t, mtime.UnixNano(), got[0].SourceMtime)
}

func TestDiscoverCodexS3FoldsSessionIndexMetadata(t *testing.T) {
	oldList := listS3Objects
	oldStat := statS3Object
	t.Cleanup(func() {
		listS3Objects = oldList
		statS3Object = oldStat
	})

	root := "s3://bucket/laptop/raw/codex"
	rolloutURI := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		"11111111-1111-4111-8111-111111111111.jsonl"
	indexURI := "s3://bucket/laptop/raw/session_index.jsonl"
	rolloutMtime := time.Unix(100, 0)
	indexMtime := time.Unix(200, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{{
			URI:          rolloutURI,
			Size:         11,
			LastModified: rolloutMtime,
		}}, nil
	}
	statS3Object = func(got string) (S3Object, error) {
		require.Equal(t, indexURI, got)
		return S3Object{
			URI:          indexURI,
			Size:         22,
			LastModified: indexMtime,
		}, nil
	}

	got := discoverCodexS3(root)

	require.Len(t, got, 1)
	assert.Equal(t, rolloutURI, got[0].Path)
	assert.Equal(t, int64(33), got[0].SourceSize)
	assert.Equal(t, indexMtime.UnixNano(), got[0].SourceMtime)
}

func TestDiscoverClaudeS3FoldsToolResultMetadata(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	sessionMtime := time.Unix(100, 0)
	sidecarMtime := time.Unix(200, 0)
	listS3Objects = func(root string) ([]S3Object, error) {
		require.Equal(t, "s3://bucket/laptop/raw/claude", root)
		return []S3Object{
			{
				URI: "s3://bucket/laptop/raw/claude/" +
					"proj/session.jsonl",
				Size:         11,
				LastModified: sessionMtime,
			},
			{
				URI: "s3://bucket/laptop/raw/claude/" +
					"proj/session/tool-results/out.txt",
				Size:         22,
				LastModified: sidecarMtime,
			},
		}, nil
	}

	got := discoverClaudeS3("s3://bucket/laptop/raw/claude")
	require.Len(t, got, 1)
	assert.Equal(
		t,
		"s3://bucket/laptop/raw/claude/proj/session.jsonl",
		got[0].Path,
	)
	assert.Equal(t, int64(33), got[0].SourceSize)
	assert.Equal(t, sidecarMtime.UnixNano(), got[0].SourceMtime)
}

func TestDiscoverClaudeS3RequiresSubagentsUnderParentSession(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	mtime := time.Unix(100, 0)
	listS3Objects = func(root string) ([]S3Object, error) {
		require.Equal(t, "s3://bucket/laptop/raw/claude", root)
		return []S3Object{
			{
				URI: "s3://bucket/laptop/raw/claude/" +
					"proj/subagents/agent-orphan.jsonl",
				Size:         11,
				LastModified: mtime,
			},
			{
				URI: "s3://bucket/laptop/raw/claude/" +
					"proj/parent-session/subagents/workflows/wf-1/agent-good.jsonl",
				Size:         22,
				LastModified: mtime,
			},
		}, nil
	}

	got := discoverClaudeS3("s3://bucket/laptop/raw/claude")

	require.Len(t, got, 1)
	assert.Equal(
		t,
		"s3://bucket/laptop/raw/claude/"+
			"proj/parent-session/subagents/workflows/wf-1/agent-good.jsonl",
		got[0].Path,
	)
}
