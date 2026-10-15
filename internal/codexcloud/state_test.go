package codexcloud_test

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
)

const stateKey codexcloud.AccountKey = "0123456789abcdef0123456789abcdef"

func TestEmptyListRemovesFile(t *testing.T) {
	root := t.TempDir()
	store := codexcloud.NewStore(root)
	require.NoError(t, store.SaveWaiting(stateKey, "thread-p", []string{"thread-a"}))
	require.NoError(t, store.SaveWaiting(stateKey, "thread-p", nil))
	assert.NoFileExists(t, filepath.Join(root, string(stateKey), "thread-p.waiting.json"))
	require.NoError(t, store.SaveDescendants(stateKey, "thread-q", nil), "removing a missing file is not an error")
}

func TestStateFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	root := t.TempDir()
	store := codexcloud.NewStore(root)
	require.NoError(t, store.SaveAccount(stateKey, codexcloud.AccountState{AccountID: "account-a"}))
	dir, err := os.Stat(filepath.Join(root, string(stateKey)))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	file, err := os.Stat(filepath.Join(root, string(stateKey), "account.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), file.Mode().Perm())
}

func TestLockSerializesWriters(t *testing.T) {
	store := codexcloud.NewStore(t.TempDir())
	errs := make(chan error, 40)
	update := func(change func(*codexcloud.AccountState)) {
		unlock := store.Lock(stateKey)
		defer unlock()
		st, err := store.LoadAccount(stateKey)
		if err == nil {
			change(&st)
			err = store.SaveAccount(stateKey, st)
		}
		errs <- err
	}
	var wg sync.WaitGroup
	for i := range 20 {
		id := "thread-" + string(rune('a'+i))
		wg.Go(func() { update(func(st *codexcloud.AccountState) { st.CheckAgain[id] = true }) })
		wg.Go(func() {
			update(func(st *codexcloud.AccountState) { st.Unfinished[id] = codexcloud.UnfinishedEntry{InProgress: true} })
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	st, err := store.LoadAccount(stateKey)
	require.NoError(t, err)
	assert.Len(t, st.CheckAgain, 20)
	assert.Len(t, st.Unfinished, 20)
}

func TestLostThreadStateLoadsZero(t *testing.T) {
	store := codexcloud.NewStore(t.TempDir())
	st, err := store.LoadThread(stateKey, "thread-a")
	require.NoError(t, err)
	assert.Equal(t, codexcloud.ThreadState{}, st)
	account, err := store.LoadAccount(stateKey)
	require.NoError(t, err)
	assert.NotNil(t, account.Retry, "sets are allocated so callers can write to them")
}

// Thread IDs come from the server and become file names.
func TestStateRejectsUnsafeThreadID(t *testing.T) {
	store := codexcloud.NewStore(t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, ".hidden", "a\x00b"} {
		_, err := store.MirrorPath(stateKey, id)
		require.Error(t, err, "%q", id)
		require.Error(t, store.SaveThread(stateKey, id, codexcloud.ThreadState{}), "%q", id)
	}
	path, err := store.MirrorPath(stateKey, "thr_01JAB-c.d")
	require.NoError(t, err)
	assert.Equal(t, "thr_01JAB-c.d.jsonl.zstd", filepath.Base(path))
}
