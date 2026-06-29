package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newShapeOnlyTestSingleFileSourceSet builds a single-file source set whose
// classifyPath accepts a stored path by SHAPE alone (no on-disk check, as
// Reasonix and Cowork do) and whose findFile re-resolves a raw ID to the live
// file. Only the FindSource-relevant hooks carry real behavior; the rest are
// inert stubs required by the constructor.
func newShapeOnlyTestSingleFileSourceSet(root, livePath string) singleFileSourceSet {
	return NewSingleFileSourceSet(
		AgentReasonix,
		[]string{root},
		WithFileDiscovery(func(string) []singleFileMatch { return nil }),
		WithFileWatchRoots(func([]string) []WatchRoot { return nil }),
		WithFileChangedPathClassifier(
			func(_, path string, _ bool) (singleFileMatch, bool) {
				if path == "" {
					return singleFileMatch{}, false
				}
				return singleFileMatch{Path: path}, true
			},
		),
		WithFileLookup(func(_, rawID string) (singleFileMatch, bool) {
			if rawID != "" && IsRegularFile(livePath) {
				return singleFileMatch{Path: livePath}, true
			}
			return singleFileMatch{}, false
		}),
		WithFileFingerprint(
			func(singleFileSource) (SourceFingerprint, error) {
				return SourceFingerprint{}, nil
			},
		),
		WithFileParse(
			func(singleFileSource, ParseRequest) ([]ParseResult, []string, error) {
				return nil, nil, nil
			},
		),
	)
}

// TestSingleFileFindSourceRejectsStaleStoredPath verifies the fresh-source guard
// in singleFileSourceSet.FindSource: a stored path that classifies by shape but
// no longer exists must not be returned under RequireFreshSource; the lookup
// falls through to raw-ID re-resolution to the live file. Without
// RequireFreshSource the stored path is honored, preserving prior behavior.
func TestSingleFileFindSourceRejectsStaleStoredPath(t *testing.T) {
	root := t.TempDir()
	livePath := filepath.Join(root, "archive", "sess.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(livePath), 0o755))
	require.NoError(t, os.WriteFile(livePath, []byte("{}\n"), 0o644))
	stalePath := filepath.Join(root, "sessions", "sess.jsonl") // never created

	s := newShapeOnlyTestSingleFileSourceSet(root, livePath)

	src, ok, err := s.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     stalePath,
		FingerprintKey:     stalePath,
		RawSessionID:       "sess",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok, "raw-ID re-resolution should still find the live file")
	assert.Equal(t, livePath, src.DisplayPath,
		"a stale stored path must re-resolve to the live file under RequireFreshSource")

	src2, ok2, err := s.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: stalePath,
		FingerprintKey: stalePath,
		RawSessionID:   "sess",
	})
	require.NoError(t, err)
	require.True(t, ok2)
	assert.Equal(t, stalePath, src2.DisplayPath,
		"without RequireFreshSource the stored-path hint is honored unchanged")
}
