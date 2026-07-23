package server

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
)

func collectArtifactEntries(
	t *testing.T, store artifact.ArtifactStore, origin string, kind artifact.Kind, limit int,
) []artifact.Entry {
	t.Helper()
	iterator, err := store.Entries(t.Context(), origin, kind)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, iterator.Close()) })
	var result []artifact.Entry
	for {
		items, nextErr := iterator.Next(t.Context(), limit)
		require.True(t, nextErr == nil || errors.Is(nextErr, io.EOF))
		result = append(result, items...)
		if errors.Is(nextErr, io.EOF) {
			return result
		}
	}
}
