package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
)

func TestArtifactCursorRegistryCapsConcurrentSnapshotConstruction(t *testing.T) {
	root := t.TempDir()
	registry := newArtifactCursorRegistry()
	t.Cleanup(registry.close)
	release := make(chan struct{})
	started := make(chan struct{}, maxArtifactCursors)
	results := make(chan error, maxArtifactCursors)
	var concurrent atomic.Int32
	var maximum atomic.Int32

	for index := range maxArtifactCursors {
		go func() {
			lease, err := registry.buildSnapshot(t.Context(), root,
				fmt.Sprintf("scope:%d", index),
				func(context.Context, func(int, string) error) error {
					current := concurrent.Add(1)
					defer concurrent.Add(-1)
					for {
						observed := maximum.Load()
						if current <= observed || maximum.CompareAndSwap(observed, current) {
							break
						}
					}
					started <- struct{}{}
					<-release
					return nil
				})
			if lease != nil {
				lease.release()
			}
			results <- err
		}()
	}
	for range maxArtifactCursors {
		<-started
	}

	var overflowWork atomic.Bool
	overflow, err := registry.buildSnapshot(t.Context(), root, "overflow",
		func(context.Context, func(int, string) error) error {
			overflowWork.Store(true)
			return nil
		})
	require.Nil(t, overflow)
	require.Error(t, err)
	assert.ErrorIs(t, err, artifact.ErrArtifactConflict)
	assert.False(t, overflowWork.Load(), "capacity must be reserved before snapshot work")
	assert.Equal(t, int32(maxArtifactCursors), maximum.Load())
	cursorFiles, globErr := filepath.Glob(filepath.Join(root, ".peer-cursor-*.sqlite"))
	require.NoError(t, globErr)
	assert.LessOrEqual(t, len(cursorFiles), maxArtifactCursors)

	close(release)
	for range maxArtifactCursors {
		require.NoError(t, <-results)
	}
	cursorFiles, globErr = filepath.Glob(filepath.Join(root, ".peer-cursor-*.sqlite"))
	require.NoError(t, globErr)
	assert.Empty(t, cursorFiles)
}

func TestArtifactCursorRegistryClaimedSnapshotsRetainCapacity(t *testing.T) {
	root := t.TempDir()
	registry := newArtifactCursorRegistry()
	t.Cleanup(registry.close)
	claimed := make([]*artifactCursorLease, 0, maxArtifactCursors)
	for index := range maxArtifactCursors {
		scope := fmt.Sprintf("scope:%d", index)
		lease, err := registry.buildSnapshot(t.Context(), root, scope,
			func(_ context.Context, insert func(int, string) error) error {
				if err := insert(0, "a"); err != nil {
					return err
				}
				return insert(0, "b")
			})
		require.NoError(t, err)
		_, token, err := finishArtifactSnapshotPage(t.Context(), registry, lease, 1)
		require.NoError(t, err)
		require.NotEmpty(t, token)
		lease, err = registry.claim(token, scope)
		require.NoError(t, err)
		claimed = append(claimed, lease)
	}

	var overflowWork atomic.Bool
	overflow, err := registry.buildSnapshot(t.Context(), root, "overflow",
		func(context.Context, func(int, string) error) error {
			overflowWork.Store(true)
			return nil
		})
	require.Nil(t, overflow)
	require.Error(t, err)
	assert.ErrorIs(t, err, artifact.ErrArtifactConflict)
	assert.False(t, overflowWork.Load())

	for _, lease := range claimed {
		lease.release()
	}
	assertCursorReservationAvailable(t, registry, root)
}

func TestArtifactCursorRegistryReleasesFailedAndCanceledConstruction(t *testing.T) {
	for _, tt := range []struct {
		name string
		ctx  func() context.Context
		fill func(context.Context, func(int, string) error) error
		want error
	}{
		{
			name: "fill failure",
			ctx:  t.Context,
			fill: func(context.Context, func(int, string) error) error {
				return errors.New("fill failed")
			},
			want: errors.New("fill failed"),
		},
		{
			name: "caller cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			fill: func(ctx context.Context, _ func(int, string) error) error {
				return ctx.Err()
			},
			want: context.Canceled,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			registry := newArtifactCursorRegistry()
			t.Cleanup(registry.close)
			ctx := tt.ctx()
			lease, err := registry.buildSnapshot(ctx, root, "scope", tt.fill)
			require.Nil(t, lease)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want.Error())
			cursorFiles, globErr := filepath.Glob(filepath.Join(root, ".peer-cursor-*.sqlite"))
			require.NoError(t, globErr)
			assert.Empty(t, cursorFiles)
			assertCursorReservationAvailable(t, registry, root)
		})
	}
}

func TestArtifactCursorRegistryShutdownCancelsConstructionAndRemovesSnapshot(t *testing.T) {
	root := t.TempDir()
	registry := newArtifactCursorRegistry()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		lease, err := registry.buildSnapshot(t.Context(), root, "scope",
			func(ctx context.Context, _ func(int, string) error) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		if lease != nil {
			lease.release()
		}
		result <- err
	}()
	<-started

	registry.close()
	err := <-result
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled) || errors.Is(err, fs.ErrClosed))
	cursorFiles, globErr := filepath.Glob(filepath.Join(root, ".peer-cursor-*.sqlite"))
	require.NoError(t, globErr)
	assert.Empty(t, cursorFiles)
}

func TestArtifactCursorRegistryForcedShutdownDoesNotWaitForClaimedPage(t *testing.T) {
	root := t.TempDir()
	registry := newArtifactCursorRegistry()
	lease, err := registry.buildSnapshot(t.Context(), root, "scope",
		func(_ context.Context, insert func(int, string) error) error {
			if err := insert(0, "a"); err != nil {
				return err
			}
			return insert(0, "b")
		})
	require.NoError(t, err)
	_, token, err := finishArtifactSnapshotPage(t.Context(), registry, lease, 1)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	lease, err = registry.claim(token, "scope")
	require.NoError(t, err)
	require.NotNil(t, lease.cursor)
	cursor := lease.cursor
	cursorPath := cursor.path
	cursor.db.SetMaxOpenConns(1)
	queryEntered := make(chan struct{})
	releaseQuery := make(chan struct{})
	var enterOnce sync.Once
	connection, err := cursor.db.Conn(t.Context())
	require.NoError(t, err)
	err = connection.Raw(func(driverConnection any) error {
		sqliteConnection, ok := driverConnection.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("snapshot connection is %T, want *sqlite3.SQLiteConn", driverConnection)
		}
		return sqliteConnection.RegisterFunc("hold_cursor_page", func(name string) string {
			enterOnce.Do(func() { close(queryEntered) })
			<-releaseQuery
			return name
		}, true)
	})
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	_, err = cursor.db.ExecContext(t.Context(), `ALTER TABLE items RENAME TO base_items`)
	require.NoError(t, err)
	_, err = cursor.db.ExecContext(t.Context(), `
		CREATE VIEW items AS
		SELECT kind, hold_cursor_page(name) AS name
		FROM base_items`)
	require.NoError(t, err)

	type pageResult struct {
		err error
	}
	pageDone := make(chan pageResult, 1)
	pageCtx, cancelPage := context.WithCancel(t.Context())
	defer cancelPage()
	go func() {
		_, _, pageErr := lease.page(pageCtx, 1)
		pageDone <- pageResult{err: pageErr}
	}()
	select {
	case <-queryEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "claimed page never entered the SQLite query")
	}
	require.Equal(t, 1, cursor.db.Stats().InUse)

	closeDone := make(chan struct{})
	go func() {
		registry.close()
		close(closeDone)
	}()
	closedPromptly := false
	select {
	case <-closeDone:
		closedPromptly = true
	case <-time.After(200 * time.Millisecond):
	}
	assert.FileExists(t, cursorPath,
		"snapshot cleanup must wait until the active page releases its pin")
	cancelPage()
	close(releaseQuery)
	result := <-pageDone
	<-closeDone

	assert.True(t, closedPromptly, "forced shutdown must not wait for an active page query")
	assert.ErrorIs(t, result.err, context.Canceled)
	assert.ErrorIs(t, result.err, fs.ErrClosed)
	assert.NoFileExists(t, cursorPath)
	assert.True(t, cursor.cleaned)
	assert.Nil(t, cursor.db)
	registry.close()
}

func assertCursorReservationAvailable(
	t *testing.T, registry *artifactCursorRegistry, root string,
) {
	t.Helper()
	lease, err := registry.buildSnapshot(t.Context(), root, "available",
		func(context.Context, func(int, string) error) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, lease)
	lease.release()
}
