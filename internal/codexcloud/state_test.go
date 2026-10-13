package codexcloud_test

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
)

const stateKey codexcloud.AccountKey = "0123456789abcdef0123456789abcdef"

func TestStateRoundTrip(t *testing.T) {
	store := codexcloud.NewStore(t.TempDir())
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	thread := codexcloud.ThreadState{
		UpdatedAt: 1759320000, ResumeTurnID: "turn-2", ResumeItemID: "item-9", Offset: 4096,
		ResolvedParent: "thread-p",
		Turns: []codexcloud.TurnState{{
			ID: "turn-2", Status: "completed", FirstSeen: started, EndedFirstSeen: started.Add(time.Minute),
			StartedAt: &started, Digest: "d1",
		}},
		LastProgress: started, ItemDigests: map[string]string{"turn-2/item-9": "d2"},
		LastHandledTurnID: "turn-1", IsSubAgent: true, ArchivedFirstSeen: started,
	}
	require.NoError(t, store.SaveThread(stateKey, "thread-a", thread))
	gotThread, err := store.LoadThread(stateKey, "thread-a")
	require.NoError(t, err)
	assert.Equal(t, thread, gotThread)

	account := codexcloud.AccountState{
		Watermarks: map[codexcloud.ListingID]int64{codexcloud.ListDefaultActive: 1759320000},
		Wait:       codexcloud.WaitState{Failure: "signed-out", Attempts: 2, Until: started},
		Retry: map[string]codexcloud.RetryEntry{"thread-b": {
			Attempts: 1, NextAttempt: started, FirstFailure: started, Message: "reply too large",
			Listing: jsontext.Value(`{"id":"thread-b"}`),
		}},
		CheckAgain:   map[string]bool{"thread-c": true},
		Unfinished:   map[string]codexcloud.UnfinishedEntry{"thread-d": {InProgress: true, SubAgent: true}},
		Retired:      map[string]bool{"thread-f": true},
		UsagePending: map[string]codexcloud.PendingEntry{"thread-a": {Since: started, NextAttempt: started}},
		Refresh:      map[string]codexcloud.RefreshEntry{"thread-e": {DataAsOf: started, LastChange: started, Reentries: 2}},
		Backfilled:   map[codexcloud.ListingID]bool{codexcloud.ListSubAgentArchived: true},
		AccountID:    "account-a", UserID: "user-a",
	}
	require.NoError(t, store.SaveAccount(stateKey, account))
	gotAccount, err := store.LoadAccount(stateKey)
	require.NoError(t, err)
	assert.Equal(t, account, gotAccount)

	require.NoError(t, store.SaveDescendants(stateKey, "thread-a", []string{"thread-b", "thread-c"}))
	descendants, err := store.LoadDescendants(stateKey, "thread-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"thread-b", "thread-c"}, descendants)
	require.NoError(t, store.SaveWaiting(stateKey, "thread-p", []string{"thread-a"}))
	waiting, err := store.LoadWaiting(stateKey, "thread-p")
	require.NoError(t, err)
	assert.Equal(t, []string{"thread-a"}, waiting)
}

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

// A crash between staging and rename leaves a temporary file beside the
// state. It must never be read as the state or block the next save.
func TestStateAtomicWrite(t *testing.T) {
	root := t.TempDir()
	store := codexcloud.NewStore(root)
	require.NoError(t, store.SaveThread(stateKey, "thread-a", codexcloud.ThreadState{Offset: 10}))
	dir := filepath.Join(root, string(stateKey))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".thread-a.state.json.tmp-crash"), []byte(`{"offset":99`), 0o600))

	got, err := store.LoadThread(stateKey, "thread-a")
	require.NoError(t, err)
	assert.Equal(t, int64(10), got.Offset)
	require.NoError(t, store.SaveThread(stateKey, "thread-a", codexcloud.ThreadState{Offset: 20}))
	got, err = store.LoadThread(stateKey, "thread-a")
	require.NoError(t, err)
	assert.Equal(t, int64(20), got.Offset)
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
