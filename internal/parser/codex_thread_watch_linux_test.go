package parser

import (
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
	null, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, null.Close()) })
	require.NoError(t, unix.Dup3(int(null.Fd()), cache.watch.set.fd, unix.O_CLOEXEC))
	_, err = cache.find(t.Context(), root, revertThread)
	require.ErrorIs(t, err, unix.ENOTTY)
	file := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	paths, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	assert.Equal(t, []string{file}, paths)
}
