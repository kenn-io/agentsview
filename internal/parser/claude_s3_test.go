package parser

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	got := ClaudeProjectSessionFiles("s3://bucket/laptop/raw/claude")
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

	got := ClaudeProjectSessionFiles("s3://bucket/laptop/raw/claude")

	require.Len(t, got, 1)
	assert.Equal(
		t,
		"s3://bucket/laptop/raw/claude/"+
			"proj/parent-session/subagents/workflows/wf-1/agent-good.jsonl",
		got[0].Path,
	)
}

// TestClaudeSubagentTranscriptPathsS3 covers the S3 branch of the on-demand
// subagent enumeration behind `session usage`: an s3:// Claude session lists
// its subagents/ prefix instead of walking a local directory, so delegated
// spend from S3-sourced sessions is refreshed like local spend.
func TestClaudeSubagentTranscriptPathsS3(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	const root = "s3://bucket/laptop/raw/claude"
	sessionPath := root + "/-home-proj/sess-1.jsonl"
	tests := []struct {
		name       string
		path       string
		list       func(t *testing.T, prefix string) ([]S3Object, error)
		want       []string
		wantListed bool
	}{
		{
			name: "lists the session's subagents prefix",
			path: sessionPath,
			list: func(t *testing.T, prefix string) ([]S3Object, error) {
				assert.Equal(t,
					root+"/-home-proj/sess-1/subagents", prefix)
				return []S3Object{
					{URI: root + "/-home-proj/sess-1/subagents/agent-b.jsonl"},
					{URI: root + "/-home-proj/sess-1/subagents/agent-b.meta.json"},
					{URI: root + "/-home-proj/sess-1/subagents/notes.jsonl"},
					{URI: root + "/-home-proj/sess-1/subagents/workflows/wf-1/agent-a.jsonl"},
				}, nil
			},
			want: []string{
				root + "/-home-proj/sess-1/subagents/agent-b.jsonl",
				root + "/-home-proj/sess-1/subagents/workflows/wf-1/agent-a.jsonl",
			},
			wantListed: true,
		},
		{
			name: "a subagent lists the enclosing root tree",
			path: root + "/-home-proj/sess-1/subagents/agent-b.jsonl",
			list: func(t *testing.T, prefix string) ([]S3Object, error) {
				assert.Equal(t,
					root+"/-home-proj/sess-1/subagents", prefix)
				return []S3Object{
					{URI: root + "/-home-proj/sess-1/subagents/agent-b.jsonl"},
					{URI: root + "/-home-proj/sess-1/subagents/workflows/wf-1/agent-a.jsonl"},
				}, nil
			},
			want: []string{
				root + "/-home-proj/sess-1/subagents/workflows/wf-1/agent-a.jsonl",
			},
			wantListed: true,
		},
		{
			name: "a listing error reads as no subagents",
			path: sessionPath,
			list: func(_ *testing.T, _ string) ([]S3Object, error) {
				return nil, errors.New("store unavailable")
			},
			wantListed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listed := false
			listS3Objects = func(prefix string) ([]S3Object, error) {
				listed = true
				require.NotNil(t, tt.list,
					"unexpected S3 listing for %s", tt.path)
				return tt.list(t, prefix)
			}
			got := ClaudeSubagentTranscriptPaths(tt.path)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantListed, listed)
		})
	}
}
