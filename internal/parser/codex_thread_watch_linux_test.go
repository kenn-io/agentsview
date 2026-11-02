package parser

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
	"golang.org/x/sys/unix"
)

func TestCodexForkInventoryWithEqualDirectoryTimes(t *testing.T) {
	for _, mode := range []string{"standalone", "batch completion", "stable snapshots"} {
		t.Run(mode, func(t *testing.T) {
			factory, cfg, source := codexForkInventoryFixture(t, 0)
			cfg.StableSourceSnapshots = mode == "stable snapshots"
			fork := testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(revertOther, revertThread, "/work/project", "user", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly),
				testjsonl.CodexMsgJSON("assistant", "copied page", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tsEarlyS1),
				testjsonl.CodexMsgJSON("assistant", "new answer", tsEarlyS1),
			)
			require.NoError(t, os.WriteFile(source.Opaque.(codexSource).Path, []byte(fork), 0o600))
			ctx := t.Context()
			if mode == "batch completion" {
				ctx = WithCodexParentInventoryBatch(ctx)
			}
			provider := factory.NewProvider(cfg)
			out, err := provider.Parse(ctx, ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, out.Results, 1)
			require.Len(t, out.Results[0].Result.Messages, 2)
			page := filepath.Join(cfg.Roots[0], "rollout-2026-09-01T12-00-00-"+revertThread+"_"+revertRollout+".jsonl")
			require.NoError(t, os.WriteFile(page, []byte(testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly))), 0o600))
			// Linux cannot set ctime from userspace. Establish the same cached
			// metadata a same-tick create produces, while retaining real name
			// notifications and the old filename inventory.
			cache := &factory.(*codexProviderFactory).threadFiles
			info, changeTime, err := statCodexThreadDirectory(cfg.Roots[0])
			require.NoError(t, err)
			cache.mu.Lock()
			directory := cache.roots[cfg.Roots[0]].Value.(*codexThreadDirectory)
			directory.info, directory.changeTime = info, changeTime
			cache.mu.Unlock()
			if mode == "batch completion" {
				affected, err := ValidateCodexParentInventoryBatch(ctx)
				require.NoError(t, err)
				assert.Equal(t, []CodexParentInventorySource{{Agent: AgentCodex, Path: source.Opaque.(codexSource).Path}}, affected)
			} else {
				out, err := provider.Parse(ctx, ParseRequest{Source: source})
				require.NoError(t, err)
				require.Len(t, out.Results, 1)
				require.Len(t, out.Results[0].Result.Messages, 1)
				assert.Equal(t, "new answer", out.Results[0].Result.Messages[0].Content)
			}
		})
	}
}

func TestCodexThreadWatchSharesAndReleasesDescriptor(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(root, alias))
	var cache codexThreadFileCache
	t.Cleanup(func() { cache.invalidate([]string{root, alias}) })
	for _, path := range []string{root, alias} {
		_, err := cache.find(t.Context(), path, revertThread)
		require.NoError(t, err)
	}
	fd := cache.watch.set.fd
	cache.invalidate([]string{root})
	file := filepath.Join(alias, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	paths, err := cache.find(t.Context(), alias, revertThread)
	require.NoError(t, err)
	assert.Equal(t, []string{file}, paths)
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err, "invalidating one alias must preserve the other watch")
	cache.invalidate([]string{alias})
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	assert.ErrorIs(t, err, unix.EBADF)
}

func TestCodexThreadWatchRecoversAfterNotificationFailure(t *testing.T) {
	root := t.TempDir()
	var cache codexThreadFileCache
	t.Cleanup(func() { cache.invalidate([]string{root}) })
	_, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	previous := cache.watch.set
	file := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	queued, err := unix.IoctlGetInt(previous.fd, unix.TIOCINQ)
	require.NoError(t, err)
	require.Positive(t, queued)
	reads := 0
	cache.watch.readEvents = func(fd int, data []byte) (int, error) {
		require.Equal(t, previous.fd, fd)
		reads++
		// A kernel overflow event has wd=-1 and no name; keep the real inotify fd.
		clear(data[:unix.SizeofInotifyEvent])
		binary.NativeEndian.PutUint32(data, ^uint32(0))
		binary.NativeEndian.PutUint32(data[4:], unix.IN_Q_OVERFLOW)
		return unix.SizeofInotifyEvent, nil
	}
	_, err = cache.find(t.Context(), root, revertThread)
	require.ErrorContains(t, err, "directory notification queue overflowed")
	require.Equal(t, 1, reads)
	assert.Equal(t, -1, previous.fd)
	cache.watch.readEvents = nil
	paths, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	assert.NotSame(t, previous, cache.watch.set)
	assert.Equal(t, []string{file}, paths)
}

func TestCodexForkInventoryWithoutNotificationCapacity(t *testing.T) {
	for _, operation := range []string{"initialize", "register"} {
		t.Run(operation, func(t *testing.T) {
			factory, cfg, source := codexForkInventoryFixture(t, 0)
			cache := &factory.(*codexProviderFactory).threadFiles
			unavailable, attempts := true, 0
			cache.watch.initialize = func(flags int) (int, error) {
				if unavailable && operation == "initialize" {
					attempts++
					return -1, unix.EMFILE
				}
				return unix.InotifyInit1(flags)
			}
			cache.watch.addWatch = func(fd int, path string, mask uint32) (int, error) {
				if unavailable && operation == "register" {
					attempts++
					return -1, unix.ENOSPC
				}
				return unix.InotifyAddWatch(fd, path, mask)
			}
			provider := factory.NewProvider(cfg)
			planner := provider.(RawCaptureProvider)
			plan, err := planner.PlanRawCapture(t.Context(), source)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(plan.Entries), 2)
			out, err := provider.Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.True(t, out.ResultSetComplete)
			require.Len(t, out.Results, 1)
			require.Equal(t, DataVersionCurrent, out.Results[0].DataVersion)
			require.Len(t, out.Results[0].Result.Messages, 1)
			assert.Equal(t, "new answer", out.Results[0].Result.Messages[0].Content)
			page := filepath.Join(cfg.Roots[0], "rollout-2026-09-01T12-00-00-"+revertThread+"_"+revertRollout+".jsonl")
			require.NoError(t, os.WriteFile(page, []byte(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly)), 0o600))
			// Model same-tick metadata while notifications remain unavailable.
			info, changeTime, err := statCodexThreadDirectory(cfg.Roots[0])
			require.NoError(t, err)
			directory := cache.roots[cfg.Roots[0]].Value.(*codexThreadDirectory)
			directory.info, directory.changeTime = info, changeTime
			plan, err = planner.PlanRawCapture(t.Context(), source)
			require.NoError(t, err)
			var paths []string
			for _, entry := range plan.Entries {
				paths = append(paths, entry.LocalPath)
			}
			assert.Contains(t, paths, page)
			require.Positive(t, attempts)
			unavailable = false
			_, err = planner.PlanRawCapture(t.Context(), source)
			require.NoError(t, err)
			require.NotNil(t, directory.watch.state)
			assert.GreaterOrEqual(t, cache.watch.set.fd, 0)
		})
	}
}

func TestCodexStableSnapshotsDoNotInitializeNotifications(t *testing.T) {
	factory, cfg, source := codexForkInventoryFixture(t, 0)
	cfg.StableSourceSnapshots = true
	cache := &factory.(*codexProviderFactory).threadFiles
	calls := 0
	cache.watch.initialize = func(int) (int, error) { calls++; return -1, unix.EMFILE }
	cache.watch.addWatch = func(int, string, uint32) (int, error) { calls++; return -1, unix.ENOSPC }
	out, err := factory.NewProvider(cfg).Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	require.Equal(t, DataVersionCurrent, out.Results[0].DataVersion)
	require.Len(t, out.Results[0].Result.Messages, 1)
	assert.Equal(t, "new answer", out.Results[0].Result.Messages[0].Content)
	assert.Zero(t, calls)
}
