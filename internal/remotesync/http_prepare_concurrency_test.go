package remotesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestPrepareHTTPSyncsOverlapsTransfersWithinBound(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan string, 6)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	var progress []string
	var lifecycle []string
	dataDir := t.TempDir()
	var syncs []RemoteSync
	for i := range 6 {
		remote := newMirrorTestRemote(t)
		remote.writeSession(t, "session.jsonl", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "session")
		host := fmt.Sprintf("host-%d", i)
		remote.onArchive = func(ArchiveRequest) {
			count := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
			}
			entered <- host
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		var hs RemoteSync
		if len(syncs) == 0 {
			_, hs = newMirrorSync(t, remote, dataDir)
		} else {
			hs = syncs[0]
			setHTTPSourceURL(t, &hs, remote.ts.URL)
		}
		hs.Host = host
		hs.Progress = func(p syncpkg.Progress) {
			runtime.Gosched()
			progress = append(progress, p.Detail)
		}
		hs.Lifecycle = &RemoteSyncLifecycle{
			PrepareStarted: func() {
				runtime.Gosched()
				lifecycle = append(lifecycle, host+":started")
			},
			PrepareFinished: func(error) {
				runtime.Gosched()
				lifecycle = append(lifecycle, host+":finished")
			},
		}
		syncs = append(syncs, hs)
	}
	type result struct {
		prepared *PreparedRemoteSyncs
		err      error
	}
	done := make(chan result, 1)
	go func() {
		prepared, err := PrepareRemoteSyncs(ctx, syncs)
		done <- result{prepared, err}
	}()
	started := 0
	timer := time.NewTimer(backgroundWaitTimeout)
	defer timer.Stop()
firstWave:
	for started < 4 {
		select {
		case <-entered:
			started++
		case <-timer.C:
			break firstWave
		}
	}
	if started == 4 {
		select {
		case host := <-entered:
			assert.Fail(t, "a fifth transfer started while four were blocked", host)
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // Assert no fifth transfer starts while all four worker slots are held.
		}
	}
	close(release)
	var got result
	select {
	case got = <-done:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "preparation did not finish after releasing transfers")
	}
	if got.prepared != nil {
		defer func() { require.NoError(t, got.prepared.Close()) }()
	}
	require.NoError(t, got.err)
	require.NotNil(t, got.prepared)
	assert.Equal(t, 4, started, "four host transfers must overlap")
	assert.LessOrEqual(t, maximum.Load(), int32(4))
	assert.NotEmpty(t, progress)
	assert.Len(t, lifecycle, 12)
	var hosts []string
	for _, source := range got.prepared.sources {
		hosts = append(hosts, source.sync.Host)
		assertMirrorLocked(t, MirrorDir(dataDir, source.sync.Host))
	}
	assert.Equal(t, []string{"host-0", "host-1", "host-2", "host-3", "host-4", "host-5"}, hosts)
}

func TestPrepareHTTPSyncsCancelsPeersAndRetainsSpoolCleanup(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup_failure=%t", failCleanup), func(t *testing.T) {
			dataDir := t.TempDir()
			started := make(chan struct{}, 2)
			archiveStopped := make(chan struct{}, 2)
			cleanupErr := errors.New("archive spool removal failed")
			var allowCleanup atomic.Bool
			allowCleanup.Store(!failCleanup)
			var syncs []RemoteSync
			for _, host := range []string{"host-a", "host-b"} {
				remote := newMirrorTestRemote(t)
				remote.writeSession(t, "session.jsonl", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "session")
				original := remote.ts.Config.Handler
				remote.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/remote-sync/archive" {
						original.ServeHTTP(w, r)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					SetProtocolHeader(w.Header())
					_, _ = w.Write([]byte("partial archive"))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					archiveStopped <- struct{}{}
				})
				var hs RemoteSync
				if len(syncs) == 0 {
					_, hs = newMirrorSync(t, remote, dataDir)
				} else {
					hs = syncs[0]
					setHTTPSourceURL(t, &hs, remote.ts.URL)
				}
				hs.Host = host
				var reported bool
				hs.Progress = func(p syncpkg.Progress) {
					if p.BytesDone > 0 && !reported {
						reported = true
						started <- struct{}{}
					}
				}
				hs.removeArchiveSpool = func(path string) error {
					if !allowCleanup.Load() {
						return cleanupErr
					}
					return os.RemoveAll(path)
				}
				syncs = append(syncs, hs)
			}
			remote := newMirrorTestRemote(t)
			remote.manifestStatus = http.StatusUnauthorized
			remote.onManifestRequest = func(r *http.Request) {
				for range 2 {
					select {
					case <-started:
					case <-r.Context().Done():
						return
					}
				}
			}
			failed := syncs[0]
			failed.Host, failed.Progress = "host-c", nil
			setHTTPSourceURL(t, &failed, remote.ts.URL)
			syncs = append(syncs, failed)
			ctx, cancel := context.WithTimeout(t.Context(), backgroundWaitTimeout)
			defer cancel()
			prepared, err := PrepareRemoteSyncs(ctx, syncs)
			require.Error(t, err)
			assert.Nil(t, prepared)
			require.NotErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
			var hostErr *HostError
			require.ErrorAs(t, err, &hostErr)
			assert.Equal(t, "host-c", hostErr.Host)
			var statusErr *StatusError
			require.ErrorAs(t, err, &statusErr)
			assert.Equal(t, http.StatusUnauthorized, statusErr.Code)
			for range 2 {
				select {
				case <-archiveStopped:
				case <-ctx.Done():
					require.FailNow(t, "peer archive request was not canceled")
				}
			}
			for _, hs := range syncs {
				assertMirrorUnlocked(t, MirrorDir(dataDir, hs.Host))
			}
			if failCleanup {
				require.ErrorIs(t, err, cleanupErr)
				spools, globErr := filepath.Glob(filepath.Join(dataDir, "remote-mirrors", "agentsview-http-archive-*"))
				require.NoError(t, globErr)
				require.Len(t, spools, 2)
				allowCleanup.Store(true)
				require.NoError(t, retryCleanup(err))
				for _, spool := range spools {
					assert.NoDirExists(t, spool)
				}
			}
		})
	}
}

func TestPrepareHTTPSyncsParentCancellationWaitsForWorkers(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "session")
	manifestStarted := make(chan struct{})
	original := remote.ts.Config.Handler
	remote.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/remote-sync/manifest" {
			original.ServeHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(manifestStarted)
		<-r.Context().Done()
	})
	_, hs := newMirrorSync(t, remote, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan struct{})
	releaseFinished := make(chan struct{})
	hs.Lifecycle = &RemoteSyncLifecycle{PrepareFinished: func(error) {
		close(finished)
		<-releaseFinished
	}}
	done := make(chan error, 1)
	go func() {
		prepared, err := PrepareAvailableRemoteSyncs(ctx, []RemoteSync{hs})
		if prepared != nil {
			err = errors.Join(err, prepared.Close())
		}
		done <- err
	}()
	select {
	case <-manifestStarted:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "manifest request did not start")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "preparation did not observe cancellation")
	}
	select {
	case <-done:
		assert.Fail(t, "group returned before its worker finished")
	default:
	}
	close(releaseFinished)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "group did not finish after its worker exited")
	}
	assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
}
