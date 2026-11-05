package parser

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3DiscoveryRetainsHealthyLocalSources(t *testing.T) {
	for _, agent := range []AgentType{AgentClaude, AgentIcodemate, AgentCodex, AgentCursor} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			var path string
			switch agent {
			case AgentCodex:
				path = writeCodexProviderSession(t, root, "019eb791-cf7d-75c1-8439-9ed74c1229e5", "healthy")
			case AgentCursor:
				path = cursorProviderWriteJSONLTranscript(t, filepath.Join(root, "project", "agent-transcripts"), "healthy.jsonl", "healthy")
			default:
				path = filepath.Join(root, "project", "healthy.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(claudeProviderFixture("healthy")), 0o600))
			}
			denied := errors.New("AccessDenied")
			oldList := listS3Objects
			t.Cleanup(func() { listS3Objects = oldList })
			listS3Objects = func(root string) ([]S3Object, error) {
				if root == "s3://bucket/denied" {
					return nil, denied
				}
				return nil, nil
			}
			provider, ok := NewProvider(agent, ProviderConfig{Roots: []string{"s3://bucket/denied", root, "s3://bucket/later"}})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.ErrorIs(t, err, denied)
			require.ErrorAs(t, err, new(DiscoveryIncompleteError))
			require.Len(t, sources, 1)
			assert.Equal(t, path, sources[0].DisplayPath)
			stream := provider.(StreamingDiscoverer)
			var paths []string
			err = stream.DiscoverEach(t.Context(), func(source SourceRef) error {
				paths = append(paths, source.DisplayPath)
				return nil
			})
			require.ErrorIs(t, err, denied)
			assert.Equal(t, []string{path}, paths)
			// A consumer's incomplete-discovery error must still stop delivery.
			stop := DiscoveryIncompleteError{Reason: "consumer stopped"}
			calls := 0
			err = stream.DiscoverEach(t.Context(), func(SourceRef) error {
				calls++
				return stop
			})
			require.ErrorIs(t, err, stop)
			require.NotErrorIs(t, err, denied, "consumer cancellation returns before the accumulated traversal failure")
			assert.Equal(t, 1, calls)
		})
	}
}
