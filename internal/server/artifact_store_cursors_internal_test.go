package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
)

func TestArtifactStoreCursorRegistryCapsRetainedSnapshots(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 2,
	}
	tokens := make([]string, 0, maxArtifactCursors)
	for range maxArtifactCursors {
		_, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
		require.NoError(t, err)
		require.NotEmpty(t, token)
		tokens = append(tokens, token)
	}

	_, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.ErrorIs(t, err, artifact.ErrArtifactConflict)
	assert.Empty(t, token)
	assert.Equal(t, maxArtifactCursors, registry.active)
	for _, token := range tokens {
		assert.True(t, registry.release(token))
	}
	assert.Zero(t, registry.active)
}

func TestArtifactStoreCursorRegistryExpiryReleasesAndRejectsReplay(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 2,
	}
	_, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	registry.mu.Lock()
	require.NotNil(t, registry.tokens[token])
	registry.tokens[token].timer.Reset(time.Millisecond)
	registry.mu.Unlock()
	assert.Eventually(t, func() bool { return store.releaseCalls.Load() == 1 },
		time.Second, time.Millisecond)
	_, _, err = registry.peerOriginPage(t.Context(), store, token, 1)
	require.ErrorIs(t, err, artifact.ErrArtifactInvalid)
	assert.Zero(t, registry.active)
}

func TestArtifactStoreCursorRegistryTokensAreSingleUseAndScopeBound(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 3,
	}
	_, first, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.NoError(t, err)
	_, second, err := registry.peerOriginPage(t.Context(), store, first, 1)
	require.NoError(t, err)
	require.NotEmpty(t, second)
	_, _, err = registry.peerOriginPage(t.Context(), store, first, 1)
	require.ErrorIs(t, err, artifact.ErrArtifactInvalid,
		"a consumed token must not be replayable")

	for _, scope := range []string{
		"origins", "index:other-a1b2c3", "index:origin-a1b2c3:segments",
	} {
		_, err := registry.claim(second, scope)
		require.ErrorIs(t, err, artifact.ErrArtifactInvalid, scope)
	}
	_, continued, err := registry.peerOriginPage(t.Context(), store, second, 1)
	require.NoError(t, err, "wrong-scope attempts must not consume the peer token")
	assert.Empty(t, continued)
}

type failingArtifactStoreCursor struct {
	*pagedLifecycleStore
	err error
}

func (s *failingArtifactStoreCursor) ListOrigins(
	context.Context, artifact.Cursor, int,
) ([]string, artifact.Cursor, error) {
	return nil, artifact.Cursor("owned-on-error"), s.err
}

func TestArtifactStoreCursorRegistryReleasesCursorsReturnedWithFailure(t *testing.T) {
	for _, failure := range []error{errors.New("fill failure"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			registry := newArtifactStoreCursorRegistry()
			store := &failingArtifactStoreCursor{
				pagedLifecycleStore: &pagedLifecycleStore{
					lifecycleArtifactStore: newLifecycleArtifactStore(),
				},
				err: failure,
			}
			_, _, err := registry.peerOriginPage(t.Context(), store, "", 1)
			require.ErrorIs(t, err, failure)
			assert.Equal(t, int32(1), store.releaseCalls.Load())
			assert.Zero(t, registry.active)
		})
	}
}

func TestArtifactStoreCursorRegistryShutdownDefersClaimedPageRelease(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 2,
	}
	_, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.NoError(t, err)
	lease, err := registry.claim(token, "peers")
	require.NoError(t, err)

	registry.close()
	assert.Zero(t, store.releaseCalls.Load(),
		"shutdown must not release a cursor while its page is claimed")
	err = registry.abandon(lease)
	require.NoError(t, err)
	assert.Equal(t, int32(1), store.releaseCalls.Load())
	_, _, err = registry.peerOriginPage(t.Context(), store, "", 1)
	require.ErrorIs(t, err, fs.ErrClosed)
	assert.Zero(t, registry.active)
}

func TestArtifactStoreCursorRegistryRejectsCrossOriginIndexContinuation(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), entryCount: 2,
	}
	_, token, err := registry.indexPage(t.Context(), store, "origin-a1b2c3", "", 1)
	require.NoError(t, err)
	if token == "" {
		t.Fatal("index fixture did not retain a cursor")
	}
	_, _, err = registry.indexPage(t.Context(), store, "other-a1b2c3", token, 1)
	require.ErrorIs(t, err, artifact.ErrArtifactInvalid)
	assert.True(t, registry.release(token), fmt.Sprintf("release token %q", token))
}
