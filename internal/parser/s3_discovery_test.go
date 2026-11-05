package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3DiscoveryReportsListingFailure(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })
	denied := errors.New("listing denied")
	for _, agent := range []AgentType{AgentClaude, AgentCodex, AgentCursor, AgentIcodemate} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			localRel := "project/22222222-2222-4222-8222-222222222222.jsonl"
			remoteRel := "project/11111111-1111-4111-8111-111111111111.jsonl"
			if agent == AgentCodex {
				localRel = "2026/06/24/rollout-2026-06-24T00-00-00-22222222-2222-4222-8222-222222222222.jsonl"
				remoteRel = "2026/06/24/rollout-2026-06-24T00-00-00-11111111-1111-4111-8111-111111111111.jsonl"
			}
			if agent == AgentCursor {
				localRel = "project/agent-transcripts/parent/subagents/22222222-2222-4222-8222-222222222222.jsonl"
			}
			localPath := filepath.Join(root, filepath.FromSlash(localRel))
			require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
			require.NoError(t, os.WriteFile(localPath, []byte("{}\n"), 0o600))
			good := "s3://bucket/healthy/raw/" + string(agent)
			bad := "s3://bucket/denied/raw/" + string(agent)
			localRoots := []string{root}
			expectedPaths := []string{localPath, good + "/" + remoteRel}
			if agent == AgentIcodemate {
				opencodeRoot := t.TempDir()
				sessionPath := filepath.Join(opencodeRoot, "storage", "session_diff", "global", "ses_vscode.json")
				writeOpenCodeStorageFile(t, sessionPath, map[string]any{
					"id": "ses_vscode", "directory": "/demo/project", "title": "Available history",
					"time": map[string]any{"created": 1700000000000, "updated": 1700000060000},
				})
				localRoots = append(localRoots, opencodeRoot)
				expectedPaths = append(expectedPaths, sessionPath)
			}
			listS3Objects = func(prefix string) ([]S3Object, error) {
				if prefix == bad {
					return nil, denied
				}
				require.Equal(t, good, prefix)
				return []S3Object{{URI: good + "/" + remoteRel, Size: 3, LastModified: time.Unix(100, 0)}}, nil
			}

			for _, deniedFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("denied_first=%v", deniedFirst), func(t *testing.T) {
					roots := append([]string{bad, good}, localRoots...)
					if !deniedFirst {
						roots = append(append([]string(nil), localRoots...), good, bad)
					}
					factory, ok := ProviderFactoryByType(agent)
					require.True(t, ok)
					provider := factory.NewProvider(ProviderConfig{Roots: roots})
					sources, err := provider.Discover(t.Context())
					require.ErrorIs(t, err, denied)
					var incomplete DiscoveryIncompleteError
					require.ErrorAs(t, err, &incomplete)
					var paths []string
					for _, source := range sources {
						paths = append(paths, source.DisplayPath)
					}
					assert.ElementsMatch(t, expectedPaths, paths)
					paths = nil
					err = provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(source SourceRef) error { paths = append(paths, source.DisplayPath); return nil })
					require.ErrorIs(t, err, denied)
					assert.ElementsMatch(t, expectedPaths, paths)
				})
			}
			factory, ok := ProviderFactoryByType(agent)
			require.True(t, ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{bad, root, good}})
			consumerErr := errors.New("consumer stopped")
			calls := 0
			err := provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(SourceRef) error { calls++; return consumerErr })
			require.ErrorIs(t, err, consumerErr)
			assert.Equal(t, 1, calls)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls = 0
			err = provider.(StreamingDiscoverer).DiscoverEach(ctx, func(SourceRef) error { calls++; cancel(); return nil })
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, 1, calls)
		})
	}
}

// TestClaudeSourceSetDiscoversS3Sessions verifies the Claude source set
// enumerates s3:// roots through its provider Discover path and carries the
// durable object metadata (including folded tool-result sidecar size/mtime/
// fingerprint) in the S3DiscoveredSource opaque, rather than dropping the remote
// object at the local IsRegularFile gate.
func TestClaudeSourceSetDiscoversS3Sessions(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/claude"
	sessionURI := root + "/proj/session.jsonl"
	sessionMtime := time.Unix(100, 0)
	sidecarMtime := time.Unix(200, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{
				URI:          sessionURI,
				Size:         11,
				LastModified: sessionMtime,
				Fingerprint:  "s3-meta:session",
			},
			{
				URI:          root + "/proj/session/tool-results/out.txt",
				Size:         22,
				LastModified: sidecarMtime,
				Fingerprint:  "s3-meta:sidecar",
			},
		}, nil
	}

	sources, err := newClaudeSourceSet([]string{root}).Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	src := sources[0]
	assert.Equal(t, AgentClaude, src.Provider)
	assert.Equal(t, sessionURI, src.DisplayPath)
	assert.Equal(t, sessionURI, src.FingerprintKey)
	assert.Equal(t, "proj", src.ProjectHint)

	s3, ok := src.Opaque.(S3DiscoveredSource)
	require.True(t, ok, "s3 source carries S3DiscoveredSource opaque")
	assert.Equal(t, sessionURI, s3.URI)
	assert.Equal(t, "laptop", s3.Machine)
	assert.Equal(t, "proj", s3.Project)
	// Session plus its tool-result sidecar fold into one freshness identity.
	assert.Equal(t, int64(33), s3.Size)
	assert.Equal(t, sidecarMtime.UnixNano(), s3.MtimeNS)
	assert.Contains(t, s3.Fingerprint, "session")
	assert.Contains(t, s3.Fingerprint, "sidecar")
}

// TestClaudeSourceSetMixedLocalAndS3Roots verifies a config that mixes a local
// projects root and an s3:// root discovers sources from both, with only the
// remote object carrying the S3DiscoveredSource opaque.
func TestClaudeSourceSetMixedLocalAndS3Roots(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	localRoot := t.TempDir()
	localProj := filepath.Join(localRoot, "localproj")
	require.NoError(t, os.MkdirAll(localProj, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localProj, "11111111-1111-4111-8111-111111111111.jsonl"),
		[]byte("{}\n"), 0o644,
	))

	s3Root := "s3://bucket/laptop/raw/claude"
	s3URI := s3Root + "/remoteproj/22222222-2222-4222-8222-222222222222.jsonl"
	listS3Objects = func(string) ([]S3Object, error) {
		return []S3Object{{
			URI:          s3URI,
			Size:         11,
			LastModified: time.Unix(100, 0),
			Fingerprint:  "s3-meta:remote",
		}}, nil
	}

	sources, err := newClaudeSourceSet([]string{localRoot, s3Root}).
		Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	var s3Count, localCount int
	for _, src := range sources {
		if _, ok := src.Opaque.(S3DiscoveredSource); ok {
			s3Count++
			assert.Equal(t, s3URI, src.DisplayPath)
		} else {
			localCount++
		}
	}
	assert.Equal(t, 1, s3Count, "exactly one remote source")
	assert.Equal(t, 1, localCount, "exactly one local source")
}

// TestCodexSourceSetDiscoversS3Sessions verifies the Codex source set enumerates
// rollout objects under an s3:// sessions root through its provider Discover
// path and carries the object metadata in the S3DiscoveredSource opaque.
func TestCodexSourceSetDiscoversS3Sessions(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/coder/raw/codex"
	rolloutURI := root + "/2026/06/24/rollout-2026-06-24T00-00-00-" +
		"11111111-1111-4111-8111-111111111111.jsonl"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{{
			URI:          rolloutURI,
			Size:         11,
			LastModified: mtime,
			Fingerprint:  "s3-meta:rollout",
		}}, nil
	}

	sources, err := newCodexSourceSet(AgentCodex, []string{root}).Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	src := sources[0]
	assert.Equal(t, AgentCodex, src.Provider)
	assert.Equal(t, rolloutURI, src.DisplayPath)
	assert.Equal(t, rolloutURI, src.Key)

	s3, ok := src.Opaque.(S3DiscoveredSource)
	require.True(t, ok, "s3 source carries S3DiscoveredSource opaque")
	assert.Equal(t, rolloutURI, s3.URI)
	assert.Equal(t, "coder", s3.Machine)
	assert.Equal(t, int64(11), s3.Size)
	assert.Equal(t, mtime.UnixNano(), s3.MtimeNS)
	assert.Contains(t, s3.Fingerprint, "rollout")
}

func TestS3CapableWatchPlansExcludeS3Roots(t *testing.T) {
	localRoot := t.TempDir()
	for _, tc := range []struct {
		name  string
		agent AgentType
	}{
		{name: "claude", agent: AgentClaude},
		{name: "codex", agent: AgentCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s3Root := "s3://bucket/laptop/raw/" + string(tc.agent)
			provider, ok := NewProvider(tc.agent, ProviderConfig{
				Roots: []string{localRoot, s3Root},
			})
			require.True(t, ok)

			plan, err := provider.WatchPlan(t.Context())
			require.NoError(t, err)
			foundLocal := false
			for _, root := range plan.Roots {
				if root.Path == localRoot {
					foundLocal = true
				}
				assert.False(t, strings.HasPrefix(root.Path, "s3:"), root.Path)
			}
			assert.True(t, foundLocal, "watch plan should retain the local root")
		})
	}
}

func TestCursorSourceSetDiscoversS3Sessions(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	jsonlURI := root + "/demo-proj/11111111-1111-4111-8111-111111111111.jsonl"
	txtURI := root + "/demo-proj/22222222-2222-4222-8222-222222222222.txt"
	ignored := root + "/demo-proj/notes.md"
	mtime := time.Unix(100, 0)
	listS3Objects = func(got string) ([]S3Object, error) {
		require.Equal(t, root, got)
		return []S3Object{
			{
				URI: jsonlURI, Size: 11, LastModified: mtime,
				Fingerprint: "s3-meta:jsonl",
			},
			{
				URI: txtURI, Size: 7, LastModified: mtime,
				Fingerprint: "s3-meta:txt",
			},
			{
				URI: ignored, Size: 3, LastModified: mtime,
				Fingerprint: "s3-meta:md",
			},
		}, nil
	}

	sources, err := newCursorSourceSet([]string{root}).Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	byPath := make(map[string]SourceRef, len(sources))
	for _, src := range sources {
		byPath[src.DisplayPath] = src
		assert.Equal(t, AgentCursor, src.Provider)
		assert.Equal(t, "demo-proj", src.ProjectHint)
		s3, ok := src.Opaque.(S3DiscoveredSource)
		require.True(t, ok)
		assert.Equal(t, "laptop", s3.Machine)
		assert.Equal(t, "demo-proj", s3.Project)
	}
	require.Contains(t, byPath, jsonlURI)
	require.Contains(t, byPath, txtURI)
	assert.Equal(t, int64(11), byPath[jsonlURI].Opaque.(S3DiscoveredSource).Size)
	assert.Equal(t, int64(7), byPath[txtURI].Opaque.(S3DiscoveredSource).Size)
}

func TestCursorSourceSetDiscoverEachYieldsS3Sessions(t *testing.T) {
	oldList := listS3Objects
	t.Cleanup(func() { listS3Objects = oldList })

	root := "s3://bucket/laptop/raw/cursor"
	sessionURI := root + "/demo-proj/11111111-1111-4111-8111-111111111111.jsonl"
	listS3Objects = func(string) ([]S3Object, error) {
		return []S3Object{{
			URI:          sessionURI,
			Size:         11,
			LastModified: time.Unix(100, 0),
			Fingerprint:  "s3-meta:jsonl",
		}}, nil
	}

	var got []SourceRef
	err := newCursorSourceSet([]string{root}).DiscoverEach(
		t.Context(),
		func(src SourceRef) error {
			got = append(got, src)
			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, sessionURI, got[0].DisplayPath)
	assert.Equal(t, AgentCursor, got[0].Provider)
}

func TestCursorWatchPlanSkipsS3Roots(t *testing.T) {
	local := t.TempDir()
	plan, err := newCursorSourceSet([]string{
		local,
		"s3://bucket/laptop/raw/cursor",
	}).WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, local, plan.Roots[0].Path)
}
