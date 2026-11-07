//go:build !(windows && arm64)

package duckdb

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushHeldSpareReusesRetiredSnapshot(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	original, err := os.Stat(path)
	require.NoError(t, err)
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	first, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(original, first))
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })

	// Alias opens in the same process don't enforce DuckDB's inode lock.
	// Keep a read-only handle at the live path to model a separate server.
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	second, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(original, second), "second publication must reuse the original inode")
	spare, err := os.Stat(sparePath(path))
	require.NoError(t, err)
	assert.True(t, os.SameFile(first, spare))

	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestPushHeldSpareResumesAfterCancellation(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = Push(cancelled, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, func(storage.MirrorPushProgress) { cancel() })
	require.Error(t, err)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.NoFileExists(t, path+".wal")
	leftovers, err := filepath.Glob(filepath.Join(mirrorWorkDirPath(path), filepath.Base(path)+".tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
	spare, err := os.Stat(sparePath(path))
	require.NoError(t, err)
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	live, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(spare, live), "retry must resume the consistent spare")
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestPushHeldSpareRejectsUnusableSnapshots(t *testing.T) {
	for _, mismatch := range []string{"half_copied", "scope", "schema", "archive"} {
		t.Run(mismatch, func(t *testing.T) {
			skipReopenTestOnWindows(t)
			ctx := t.Context()
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			if mismatch != "half_copied" {
				require.NoError(t, copyMirrorFile(ctx, path, sparePath(path)))
				conn, err := Open(ctx, sparePath(path))
				require.NoError(t, err)
				key, value := pushScopeMetadataKey, "other"
				switch mismatch {
				case "schema":
					key, value = schemaVersionMetadataKey, "1"
				case "archive":
					key = sourceArchiveIDMetadataKey
				}
				require.NoError(t, recordMetadataKey(ctx, conn, key, value))
				require.NoError(t, conn.Close())
			} else {
				require.NoError(t, os.WriteFile(sparePath(path), []byte("interrupted publication"), 0o600))
				require.NoError(t, os.WriteFile(sparePath(path)+".wal", []byte("leftover WAL"), 0o600))
			}
			store, err := NewStore(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			appendFrustrationPrompt(t, local, "sess-1")
			_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
			require.NoError(t, err)
			store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
			resp, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			assert.Equal(t, 1, resp.QualityHealth.Totals.FrustrationMarkerCount)
			assert.NoFileExists(t, sparePath(path)+".wal")
		})
	}
}

func TestPushHeldSpareLifecycle(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "in_place", true: "full"}[full], func(t *testing.T) {
			ctx := t.Context()
			local, path := newPushFixture(t, 1)
			_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			held, err := OpenReadOnly(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, held.Close()) })
			appendFrustrationPrompt(t, local, "sess-1")
			_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
			require.NoError(t, err)
			require.FileExists(t, sparePath(path))
			require.NoError(t, held.Close())
			_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, full, nil)
			require.NoError(t, err)
			if full {
				assert.NoFileExists(t, sparePath(path))
			} else {
				assert.FileExists(t, sparePath(path))
			}
			assertMirrorMessageCount(t, path, "sess-1", 3)
		})
	}
}

// The reader runs separately so DuckDB locks the inode even through aliases.
func holdSpareInProcess(t *testing.T, path string) func() {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHeldSpareReaderProcess$")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_TEST_HELD_SPARE="+path)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	released := false
	release := func() {
		if !released {
			released = true
			require.NoError(t, stdin.Close())
			require.NoError(t, cmd.Wait())
		}
	}
	t.Cleanup(release)
	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan())
	require.Equal(t, "ready", scanner.Text())
	return release
}

func TestHeldSpareReaderProcess(t *testing.T) {
	path := os.Getenv("AGENTSVIEW_TEST_HELD_SPARE")
	if path == "" {
		return
	}
	conn, err := OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	fmt.Println("ready")
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestPushHeldSpareKeepsPublishedSessionAfterUpdateError(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.NoError(t, held.Close())
	held, err = OpenReadOnly(ctx, path)
	require.NoError(t, err)
	before, err := os.Stat(path)
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET timestamp = 'invalid' WHERE session_id = 'sess-1' AND ordinal = 3`)
		return err
	}))
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.Error(t, err)
	assert.Equal(t, 1, res.Errors)
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, SameMirrorFile(before, after))
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 3)
	spare, err := os.Stat(sparePath(path))
	require.NoError(t, err)
	held, err = OpenReadOnly(ctx, path)
	require.NoError(t, err)
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE messages SET timestamp = '2026-01-03T00:00:00Z' WHERE session_id = 'sess-1' AND ordinal = 3`)
		return err
	}))
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	live, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(spare, live))
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 4)
}

func TestPushHeldSparePublishesNonSessionChanges(t *testing.T) {
	for _, change := range []string{"delete", "star", "pin", "pricing", "usage", "label", "retained_link"} {
		t.Run(change, func(t *testing.T) {
			skipReopenTestOnWindows(t)
			ctx := t.Context()
			local, path := newPushFixture(t, 1)
			if change == "retained_link" {
				_, err := local.StoreTrailerCommitLinks(ctx, "sample", []gitlog.TrailerCommit{{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Sessions: []string{"claude:sess-1"}, Subject: "Original", CommittedAt: "2026-01-01T00:00:00Z"}})
				require.NoError(t, err)
				require.NoError(t, local.DeleteSession(ctx, "sess-1"))
			}
			_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			held, err := OpenReadOnly(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, held.Close()) })
			store, err := NewStore(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			query, want := "", 1
			switch change {
			case "delete":
				err = local.DeleteSession(ctx, "sess-1")
				query, want = `SELECT count(*) FROM sessions`, 0
			case "star":
				_, err = local.StarSession(ctx, "sess-1")
				query = `SELECT count(*) FROM starred_sessions`
			case "pin":
				msgs, loadErr := local.GetAllMessages(ctx, "sess-1")
				require.NoError(t, loadErr)
				_, err = local.PinMessage(ctx, "sess-1", msgs[0].ID, nil)
				query = `SELECT count(*) FROM pinned_messages`
			case "pricing":
				err = local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "held-test-model", InputPerMTok: money.MustParseDollars("123"), UpdatedAt: "2026-01-01T00:00:00Z"}})
				query = `SELECT count(*) FROM model_pricing WHERE model_pattern = 'held-test-model'`
			case "usage":
				err = local.InsertCursorUsageEvents(ctx, []db.CursorUsageEvent{{OccurredAt: "2026-01-01T00:00:00Z", Model: "held-test-model", Kind: "usage", InputTokens: 123}})
				query = `SELECT count(*) FROM cursor_usage_events WHERE input_tokens = 123`
			case "retained_link":
				_, err = local.StoreTrailerCommitLinks(ctx, "sample", []gitlog.TrailerCommit{{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Sessions: []string{"claude:sess-1"}, Subject: "Corrected", CommittedAt: "2026-01-01T00:00:00Z"}})
				query = `SELECT count(*) FROM commit_links WHERE subject = 'Corrected' AND retained`
			case "label":
				err = local.SetSyncState(ctx, "machine_"+change+":m", "updated")
				query = fmt.Sprintf(`SELECT count(*) FROM sync_metadata WHERE key = 'machine_%s:m' AND value = 'updated'`, change)
			}
			require.NoError(t, err)
			res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
			require.NoError(t, err)
			assert.Zero(t, res.SessionsPushed)
			require.NoError(t, held.Close())
			store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
			var count int
			require.NoError(t, store.DB().QueryRowContext(ctx, query).Scan(&count))
			assert.Equal(t, want, count)

		})
	}
}

func TestPushHeldSpareBootstrapsRecoveredWAL(t *testing.T) {
	skipReopenTestOnWindows(t)
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err)
	s := newTestSync(t, path, local, storage.MirrorPushOptions{})
	s.machine = "m"
	s.maintenance = &checkpointSpy{}
	_, err = s.runIncrementalPush(ctx, storage.MirrorPushOptions{}, probe, nil)
	require.NoError(t, err)
	// Copy the committed WAL before Close checkpoints it, as a crashed push leaves it.
	main, err := os.ReadFile(path)
	require.NoError(t, err)
	wal, err := os.ReadFile(path + ".wal")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.WriteFile(path, main, 0o600))
	require.NoError(t, os.WriteFile(path+".wal", wal, 0o600))
	store, err := NewStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.Equal(t, 3, mirrorMessageCountViaStore(t, store, "sess-1"))
	// A second session triggers publication; the copy must still catch up
	// the session only the WAL held.
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: syncSession("new-session", "alpha", "new", "2026-01-02T00:00:00Z", 1), Messages: []db.Message{syncMessage("new-session", 0, "user", "new", "2026-01-02T00:00:00Z")}, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "new-session")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{Automatic: true}, false, nil)
	require.NoError(t, err)
	store.checkMirrorReplacement(ctx, func(err error) { require.NoError(t, err) })
	assert.Equal(t, 3, mirrorMessageCountViaStore(t, store, "sess-1"))
	assert.Equal(t, 2, mirrorMessageCountViaStore(t, store, "new-session"))
	require.NoError(t, store.Close())
	assert.NoFileExists(t, path+".wal")
	assertMirrorMessageCount(t, path, "sess-1", 3)
}

func TestPushHeldSpareRetriesAfterReaderReleases(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	opts := storage.MirrorPushOptions{Automatic: true}
	_, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	held, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	appendFrustrationPrompt(t, local, "sess-1")
	_, err = Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	require.NoError(t, held.Close())
	held, err = OpenReadOnly(ctx, path)
	require.NoError(t, err)
	release := holdSpareInProcess(t, sparePath(path))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	spareBefore, err := os.ReadFile(sparePath(path))
	require.NoError(t, err)
	appendFrustrationPrompt(t, local, "sess-1")
	res, err := Push(ctx, path, local, "m", opts, false, nil)
	require.Error(t, err)
	assert.True(t, isMirrorHeldError(err))
	assert.False(t, res.Diagnostics.Deferred)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	spareAfter, err := os.ReadFile(sparePath(path))
	require.NoError(t, err)
	assert.Equal(t, spareBefore, spareAfter)
	release()
	_, err = Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err)
	require.NoError(t, held.Close())
	assertMirrorMessageCount(t, path, "sess-1", 4)
}
