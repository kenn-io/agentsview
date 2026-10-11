package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestCodexThreadWatchPath(t *testing.T) {
	tail := strings.Repeat(`directory\`, 30)
	for _, tc := range []struct{ name, path, want string }{
		{"short", `C:\sessions`, `C:\sessions`},
		{"drive", `C:\sessions\` + tail, `\\?\C:\sessions\` + tail},
		{"unc", `\\server\share\` + tail, `\\?\UNC\server\share\` + tail},
		{"extended", `\\?\C:\sessions\` + tail, `\\?\C:\sessions\` + tail},
		{"extended unc", `\\?\UNC\server\share\` + tail, `\\?\UNC\server\share\` + tail},
		{"device", `\\.\C:\sessions\` + tail, `\\.\C:\sessions\` + tail},
		{"native", `\??\C:\sessions\` + tail, `\??\C:\sessions\` + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, codexThreadWatchPath(tc.path))
		})
	}
}

func TestCodexForkInventoryLongWindowsRoot(t *testing.T) {
	factory, cfg, _ := codexForkInventoryFixture(t, 0)
	root := filepath.Join(t.TempDir(), strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	require.NoError(t, os.MkdirAll(root, 0o700))
	entries, err := os.ReadDir(cfg.Roots[0])
	require.NoError(t, err)
	for _, entry := range entries {
		require.NoError(t, os.Rename(filepath.Join(cfg.Roots[0], entry.Name()), filepath.Join(root, entry.Name())))
	}
	cfg.Roots = []string{root}
	t.Cleanup(func() { factory.(*codexProviderFactory).threadFiles.invalidate(cfg.Roots) })
	provider := factory.NewProvider(cfg)
	source := requireCodexProviderSource(t, provider, revertOther)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	result := out.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Empty(t, result.RetryReason)
	require.Len(t, result.Result.Messages, 1)
	assert.Equal(t, "new answer", result.Result.Messages[0].Content)
}

func TestCodexThreadWatchFindsChangesWithUnchangedDirectoryTimes(t *testing.T) {
	root := t.TempDir()
	var cache codexThreadFileCache
	t.Cleanup(func() { cache.invalidate([]string{root}) })
	paths, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	require.Empty(t, paths)

	path, err := windows.UTF16PtrFromString(root)
	require.NoError(t, err)
	handle, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, windows.CloseHandle(handle)) })
	var before codexIndexWindowsFileBasicInfo
	require.NoError(t, windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&before)), uint32(unsafe.Sizeof(before))))

	for _, name := range []string{
		"rollout-2026-09-01T10-00-00-" + revertThread + ".jsonl",
		"rollout-2026-09-01T12-00-00-" + revertThread + "_" + revertRollout + ".jsonl",
	} {
		file := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		// Hold both timestamps fixed to exercise notifications independently
		// of the filesystem's timestamp update timing.
		require.NoError(t, windows.SetFileInformationByHandle(handle, windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&before)), uint32(unsafe.Sizeof(before))))
		paths, err = cache.find(t.Context(), root, revertThread)
		require.NoError(t, err)
		assert.Contains(t, paths, file)
	}
	require.Len(t, paths, 2)
}

func TestCodexThreadWatchClosesInvalidatedRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "2026", "09", "01"), 0o700))
	var cache codexThreadFileCache
	t.Cleanup(func() { cache.invalidate([]string{root}) })
	_, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	directory := cache.roots[root].Value.(*codexThreadDirectory)
	handles := []windows.Handle{directory.watch.handle, directory.children["2026"].children["09"].children["01"].watch.handle}
	cache.invalidate([]string{root})
	for _, handle := range handles {
		_, err = windows.WaitForSingleObject(handle, 0)
		require.ErrorIs(t, err, windows.ERROR_INVALID_HANDLE)
	}
	file := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	paths, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	assert.Equal(t, []string{file}, paths)
}

func TestCodexThreadWatchRecoversAfterNotificationFailure(t *testing.T) {
	root := t.TempDir()
	var cache codexThreadFileCache
	t.Cleanup(func() { cache.invalidate([]string{root}) })
	_, err := cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	handle := cache.roots[root].Value.(*codexThreadDirectory).watch.handle
	require.NoError(t, windows.FindCloseChangeNotification(handle))
	paths, err := cache.find(t.Context(), root, revertThread)
	require.ErrorIs(t, err, windows.ERROR_INVALID_HANDLE)
	assert.Empty(t, paths)
	file := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	paths, err = cache.find(t.Context(), root, revertThread)
	require.NoError(t, err)
	assert.Equal(t, []string{file}, paths)
}
