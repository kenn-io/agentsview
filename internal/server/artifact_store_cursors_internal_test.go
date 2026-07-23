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

func TestArtifactStoreCursorRegistryFinalPageClosesIterator(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 1,
	}

	origins, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"origin-00000-a1b2c3"}, origins)
	assert.Empty(t, token)
	assert.Equal(t, int32(1), store.releaseCalls.Load())
	assert.Zero(t, registry.active)
}

func TestArtifactStoreCursorRegistryReplacementClosesRetainedIterator(t *testing.T) {
	registry := newArtifactStoreCursorRegistry()
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 2,
	}
	_, token, err := registry.peerOriginPage(t.Context(), store, "", 1)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	server := &Server{artifactStoreCursors: registry}

	server.replaceArtifactStoreCursorRegistry()

	assert.Equal(t, int32(1), store.releaseCalls.Load())
	assert.Zero(t, registry.active)
	assert.NotSame(t, registry, server.artifactStoreCursors)
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

type failingArtifactOriginIterator struct {
	store  *pagedLifecycleStore
	err    error
	closed bool
}

func (i *failingArtifactOriginIterator) Next(
	context.Context, int,
) ([]string, error) {
	return nil, i.err
}

func (i *failingArtifactOriginIterator) Close() error {
	if !i.closed {
		i.closed = true
		i.store.releaseCalls.Add(1)
	}
	return nil
}

func (s *failingArtifactStoreCursor) Origins(
	context.Context,
) (artifact.OriginIterator, error) {
	return &failingArtifactOriginIterator{store: s.pagedLifecycleStore, err: s.err}, nil
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
	require.NotEmpty(t, token, "index fixture did not retain a cursor")
	_, _, err = registry.indexPage(t.Context(), store, "other-a1b2c3", token, 1)
	require.ErrorIs(t, err, artifact.ErrArtifactInvalid)
	assert.True(t, registry.release(token), fmt.Sprintf("release token %q", token))
	assert.Equal(t, int32(1), store.entryCloseCalls.Load())
}
