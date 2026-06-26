package parser

import (
	"strings"
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
		root, provider, want string
	}{
		{"s3://bkt/harvest/laptop/raw/claude", "claude", "laptop"},
		{"s3://bkt/x/y/coder-gpu1/raw/codex", "codex", "coder-gpu1"},
		{"s3://bkt/archive/raw/laptop/raw/claude", "claude", "laptop"},
		{"s3://bkt/host/raw/qwen", "qwen", "host"}, // generalizes beyond claude/codex
		{"s3://bkt/host/raw/qwen", "codex", ""},    // provider segment must match
		{"s3://bkt/raw/claude", "claude", ""},      // nothing before "raw"
		{"s3://bkt/sessions/claude", "claude", ""}, // no "raw" segment
		{"s3://bkt", "claude", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, s3MachineFromRoot(c.root, c.provider), c.root)
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

func TestS3ClientRejectsNonLoopbackHTTPEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_S3_ENDPOINT", "http://example.com:9000")
	t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", "")

	_, err := s3Client()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "insecure S3 endpoint")
}

func TestS3ClientAllowsLoopbackHTTPEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", "")

	_, err := s3Client()

	require.NoError(t, err)
}

func TestS3ClientAllowsExplicitUnsafeHTTPEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_S3_ENDPOINT", "http://example.com:9000")
	t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", "true")

	_, err := s3Client()

	require.NoError(t, err)
}

// TestS3PrefixScanGeneralizesByScanner exercises the shared scan with an
// arbitrary provider and a caller-supplied Keep/Project, demonstrating that S3
// discovery over the .../<machine>/raw/<provider> layout is general-purpose and
// not limited to the Claude/Codex configurations.
func TestS3PrefixScanGeneralizesByScanner(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/host/raw/qwen"
	keepURI := root + "/proj/keep.jsonl"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{URI: keepURI, Size: 7, LastModified: mtime, Fingerprint: "s3-meta:keep"},
			{URI: root + "/proj/skip.txt", Size: 9, LastModified: mtime},
		}, nil
	}

	got := s3PrefixScan(root, s3SessionScanner{
		Agent: AgentQwen,
		Keep: func(rel string, _ []string) bool {
			return strings.HasSuffix(rel, ".jsonl")
		},
		Project: func(_ string, segs []string) string { return segs[0] },
	})

	require.Len(t, got, 1)
	assert.Equal(t, keepURI, got[0].Path)
	assert.Equal(t, AgentQwen, got[0].Agent)
	// Machine is derived from the layout for an arbitrary provider segment.
	assert.Equal(t, "host", got[0].Machine)
	assert.Equal(t, "proj", got[0].Project)
	assert.Equal(t, int64(7), got[0].SourceSize)
	assert.Equal(t, mtime.UnixNano(), got[0].SourceMtime)
	assert.Contains(t, got[0].SourceFingerprint, "keep")
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

func TestDiscoverCodexS3KeepsSessionIndexMetadataSeparate(t *testing.T) {
	oldList := listS3Objects
	oldStat := statS3Object
	t.Cleanup(func() {
		listS3Objects = oldList
		statS3Object = oldStat
	})

	root := "s3://bucket/laptop/raw/codex"
	rolloutURI := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		"11111111-1111-4111-8111-111111111111.jsonl"
	rolloutMtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{{
			URI:          rolloutURI,
			Size:         11,
			LastModified: rolloutMtime,
			Fingerprint:  "s3-meta:rollout",
		}}, nil
	}
	statS3Object = func(got string) (S3Object, error) {
		require.Failf(t, "unexpected index stat", "stat %s", got)
		return S3Object{}, nil
	}

	got := discoverCodexS3(root)

	require.Len(t, got, 1)
	assert.Equal(t, rolloutURI, got[0].Path)
	assert.Equal(t, int64(11), got[0].SourceSize)
	assert.Equal(t, rolloutMtime.UnixNano(), got[0].SourceMtime)
	assert.Contains(t, got[0].SourceFingerprint, "rollout")
	assert.NotContains(t, got[0].SourceFingerprint, "index")
}

func TestCodexS3SessionIndexURIPrefersRawCodexLayout(t *testing.T) {
	got, ok := CodexS3SessionIndexURI(
		"s3://bucket/backups/sessions/laptop/raw/codex/2026/06/24/" +
			"rollout-2026-06-24T00-00-00-11111111-1111-4111-8111-111111111111.jsonl",
	)

	require.True(t, ok)
	assert.Equal(
		t,
		"s3://bucket/backups/sessions/laptop/raw/session_index.jsonl",
		got,
	)
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
				Fingerprint:  "s3-meta:session",
			},
			{
				URI: "s3://bucket/laptop/raw/claude/" +
					"proj/session/tool-results/out.txt",
				Size:         22,
				LastModified: sidecarMtime,
				Fingerprint:  "s3-meta:sidecar",
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
	assert.Contains(t, got[0].SourceFingerprint, "session")
	assert.Contains(t, got[0].SourceFingerprint, "sidecar")
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

func TestS3SourceRefFromDiscoveredFile(t *testing.T) {
	uri := "s3://bucket/laptop/raw/codex/sessions/2026/06/abc.jsonl"
	file := DiscoveredFile{
		Path:              uri,
		Agent:             AgentCodex,
		Project:           "proj",
		Machine:           "laptop",
		SourceSize:        4096,
		SourceMtime:       1718900000000000000,
		SourceFingerprint: "fp-1",
	}

	ref := s3SourceRefFromDiscoveredFile(file)

	// The s3 URI is the stable identity across every key field so dedup and
	// fingerprinting agree on one source.
	assert.Equal(t, AgentCodex, ref.Provider)
	assert.Equal(t, uri, ref.Key)
	assert.Equal(t, uri, ref.DisplayPath)
	assert.Equal(t, uri, ref.FingerprintKey)
	assert.Equal(t, "proj", ref.ProjectHint)

	// The durable object metadata rides in the Opaque payload for the engine to
	// thread back into the DiscoveredFile.
	opaque, ok := ref.Opaque.(S3DiscoveredSource)
	require.True(t, ok, "Opaque must be an S3DiscoveredSource")
	assert.Equal(t, S3DiscoveredSource{
		URI:         uri,
		Project:     "proj",
		Machine:     "laptop",
		Size:        4096,
		MtimeNS:     1718900000000000000,
		Fingerprint: "fp-1",
	}, opaque)
}
