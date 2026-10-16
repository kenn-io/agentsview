package requestsign

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplayRejectsUnrelatedDatabaseWithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; CREATE TABLE archive (content TEXT); INSERT INTO archive VALUES ('retained')")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, database.Close())
	_, err = OpenReplay(path)
	require.Error(t, err)
	database, err = sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer database.Close()
	var mode, content string
	require.NoError(t, database.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)
	require.NoError(t, database.QueryRowContext(t.Context(), "SELECT content FROM archive").Scan(&content))
	assert.Equal(t, "retained", content)
}

func TestReplayURIPathHandlesWindowsDrivePaths(t *testing.T) {
	assert.Equal(t, "/C:/data/replay.db", replayURIPath("C:/data/replay.db", "C:"))
	assert.Equal(t, "/tmp/replay.db", replayURIPath("/tmp/replay.db", ""))
	dsn, err := url.Parse(replayDSNForPath("C:/data/replay.db", "C:", "rw"))
	require.NoError(t, err)
	assert.Empty(t, dsn.Host)
	assert.Equal(t, "/C:/data/replay.db", dsn.Path)
}

func replayNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestReplayConcurrentAcrossStoresAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(path))
	a, err := OpenReplay(path)
	require.NoError(t, err)
	var synchronous int
	require.NoError(t, a.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))
	assert.Equal(t, 3, synchronous, "rollback journal commits must sync their directory before dispatch")
	b, err := OpenReplay(path)
	require.NoError(t, err)
	nonce := replayNonce(t)
	now := time.Now().Unix()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			store := a
			if i%2 == 0 {
				store = b
			}
			if store.Admit(t.Context(), "key", nonce, now, now+30, time.Now()) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, 1, accepted.Load())
	require.NoError(t, a.Close())
	require.NoError(t, b.Close())
	c, err := OpenReplay(path)
	require.NoError(t, err)
	defer c.Close()
	require.ErrorIs(t, c.Admit(t.Context(), "key", nonce, now, now+30, time.Now()), ErrReplay)
	// Independent keys do not share nonce identities.
	assert.NoError(t, c.Admit(t.Context(), "other-key", nonce, now, now+30, time.Now()))
}

func TestReplayMissingCorruptCapacityAndRollbackFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	_, err := OpenReplay(path)
	require.Error(t, err)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
	require.NoError(t, InitReplay(path))
	require.Error(t, InitReplay(path), "initialization cannot overwrite existing state")
	store, err := OpenReplay(path)
	require.NoError(t, err)
	defer store.Close()
	now := time.Now().Unix()
	require.NoError(t, store.Admit(t.Context(), "key", replayNonce(t), now, now+30, time.Now()))
	// Advance the persisted watermark without waiting for wall-clock rollback.
	_, err = store.db.ExecContext(t.Context(), "UPDATE signing_state SET highwater = ?", now+100)
	require.NoError(t, err)
	require.ErrorIs(t, store.Admit(t.Context(), "key", replayNonce(t), now, now+30, time.Now()), ErrReplay)
	_, err = store.db.ExecContext(t.Context(), "UPDATE signing_state SET highwater = ?", now)
	require.NoError(t, err)
	store.limit = 1
	require.ErrorIs(t, store.Admit(t.Context(), "key", replayNonce(t), now, now+30, time.Now()), ErrReplay)
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	require.NoError(t, os.WriteFile(corrupt, []byte("corrupt"), 0o600))
	_, err = OpenReplay(corrupt)
	require.Error(t, err)
}

func TestReplayChecksFreshnessAtReceiptAndRetainsNonceThroughGrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(path))
	store, err := OpenReplay(path)
	require.NoError(t, err)
	defer store.Close()
	now := time.Now()
	unix := now.Unix()
	require.ErrorIs(t, store.Admit(t.Context(), "key", replayNonce(t), unix-30, unix, now), ErrInvalid)
	require.ErrorIs(t, store.Admit(t.Context(), "key", replayNonce(t), unix+6, unix+36, now), ErrInvalid)
	require.ErrorIs(t, store.Admit(t.Context(), "key", replayNonce(t), unix, unix+31, now), ErrInvalid)

	// A slow body may finish after the signature expired: its headers were
	// fresh when received 40 seconds ago, and expiry was 30 seconds ago.
	slow := replayNonce(t)
	received := now.Add(-40 * time.Second)
	require.NoError(t, store.Admit(t.Context(), "key", slow, unix-60, unix-30, received))
	// A replay that was also fresh on receipt is rejected while the original
	// nonce is retained, even though both arrive after expiry.
	require.ErrorIs(t, store.Admit(t.Context(), "key", slow, unix-60, unix-30, received), ErrReplay)

	// Admission is refused once the grace period after expiry has passed.
	stale := now.Add(-200 * time.Second)
	require.ErrorIs(t,
		store.Admit(t.Context(), "key", replayNonce(t), stale.Unix()-10, stale.Unix()+20, stale),
		ErrInvalid)
}
