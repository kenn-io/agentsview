package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/config"
)

func TestMalformedHistoryReplyEndsAccountAsProtocolChange(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		if strings.Contains(string(params), `"a"`) {
			return map[string]any{"data": map[string]any{"x": 1}}, nil
		}
		return env.threads.TurnsList(params)
	})

	require.NoError(t, env.syncer.Run(t.Context()), "an account failure is not the job's error")

	acct := env.account(t)
	assert.Equal(t, FailProtocol.String(), acct.Wait.Failure)
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateError, row.State)
	assert.Equal(t, Message(FailProtocol, time.Time{}), row.Message)
}

func TestMalformedListingIsProtocolChangeNotNetwork(t *testing.T) {
	env := newPassEnv(t)
	env.host.Handle("thread/list", func(jsontext.Value) (any, *codexcloudtest.Error) {
		return map[string]any{"data": map[string]any{"x": 1}}, nil
	})
	env.run(t)
	assert.Equal(t, FailProtocol.String(), env.account(t).Wait.Failure)
}

func TestCorruptStateFilesAreSetAside(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.run(t)
	mirrorPath := env.mirrorPath(t, "a")
	before, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)
	threadPath, err := env.syncer.Store.path(passKey, "a", ".state.json")
	require.NoError(t, err)
	accountPath, err := env.syncer.Store.path(passKey, "account", ".json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(threadPath, []byte("{garbage"), 0o600))
	require.NoError(t, os.WriteFile(accountPath, []byte("{garbage"), 0o600))

	env.clock.Advance(10 * time.Minute)
	env.run(t)

	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateUpToDate, row.State, "a damaged state file counts as lost state")
	for _, path := range []string{threadPath, accountPath} {
		_, err := os.Stat(path + ".corrupt")
		require.NoError(t, err, "the damaged file is kept for inspection")
	}
	st, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	assert.Equal(t, ago(time.Hour), st.UpdatedAt)
	after, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "fetching again adds no duplicate records")
}

func TestCancelledPassRecordsNoFailure(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	ctx, cancel := context.WithCancel(t.Context())
	env.host.Handle("thread/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		cancel()
		<-env.host.Context().Done()
		return env.threads.ThreadList(params)
	})

	err := env.syncer.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, env.account(t).Wait, "shutdown is not an outage")
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.NotEqual(t, StateError, row.State)
}

func TestUnansweredCallEndsAccountPass(t *testing.T) {
	env := newPassEnv(t)
	env.syncer.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		o.CallTimeout = 200 * time.Millisecond
		return Dial(ctx, o)
	}
	env.host.Handle("thread/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		<-env.host.Context().Done()
		return env.threads.ThreadList(params)
	})

	env.run(t)

	assert.Equal(t, FailNetwork.String(), env.account(t).Wait.Failure)
}

func TestStalledThreadIsNotCheckedAgain(t *testing.T) {
	env := newPassEnv(t)
	updated := ago(30 * time.Second)
	env.threads.Set(codexcloudtest.Thread{
		ID: "s", Name: "s", CreatedAt: updated - 60, UpdatedAt: updated,
		Turns: []codexcloudtest.Turn{{ID: "s-1", Status: "inProgress", StartedAt: updated - 60}},
	})
	env.run(t)
	env.clock.Advance(25 * time.Hour)
	env.run(t)
	require.True(t, env.account(t).Unfinished["s"].Stalled)

	from := len(env.host.Requests())
	env.clock.Advance(5 * time.Minute)
	env.run(t)

	assert.Empty(t, env.fetchedThreads(from), "a stalled turn waits for its thread to change")
}

func TestOversizedListingWaitsWithMessage(t *testing.T) {
	env := newPassEnv(t)
	for i := range 20 {
		env.threads.Set(finishedThread("t"+strconv.Itoa(i), ago(time.Duration(i+1)*time.Hour), message("m", "hi")))
	}
	env.syncer.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		o.ReadLimit = 1000
		return Dial(ctx, o)
	}

	env.run(t)

	acct := env.account(t)
	assert.Equal(t, FailTooLarge.String(), acct.Wait.Failure)
	assert.True(t, acct.Wait.Until.After(env.clock.Now()))
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Contains(t, row.Message, Message(FailTooLarge, time.Time{}))
	assert.Contains(t, row.Message, "Imports from this account are paused, and AgentsView retries automatically.")
}

func TestRefusedRedialWaitsAtTheHome(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", strings.Repeat("x", 4000))))
	env.syncer.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		o.ReadLimit = 3000
		return Dial(ctx, o)
	}
	env.host.Handle("thread/items/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		env.host.RefuseBearer("token-0", http.StatusUnauthorized)
		return env.threads.ItemsList(params)
	})

	env.run(t)

	home, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)
	assert.Equal(t, Message(FailSignedOut, time.Time{}), home.Message)
	_, ok = env.syncer.Status.Row(string(passKey))
	assert.False(t, ok, "the account row would claim a sync that is not running")
	assert.Zero(t, env.account(t).Wait, "a refused login is the home's problem, not the account's")
}

func TestAllHomesRefusedDropsAccountRow(t *testing.T) {
	env := newPassEnv(t)
	env.run(t)
	env.host.RefuseBearer("token-0", http.StatusUnauthorized)
	env.clock.Advance(5 * time.Minute)

	env.run(t)

	_, ok := env.syncer.Status.Row(string(passKey))
	assert.False(t, ok)
	home, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)
	assert.Equal(t, StateError, home.State)
}

func TestNonDefaultHomeLoginNamesCodexHome(t *testing.T) {
	env := newPassEnv(t, "/homes/a", "/homes/b")
	env.syncer.ReadLogin = func(home string) (Login, HomeState, error) {
		return Login{Home: home}, HomeSignedOut, nil
	}
	env.run(t)

	first, _ := env.syncer.Status.Row("/homes/a")
	assert.Contains(t, first.Message, "Run `codex login`")
	second, _ := env.syncer.Status.Row("/homes/b")
	assert.Contains(t, second.Message, "Run "+homeLoginCommand("/homes/b", runtime.GOOS))
}

func TestHomeLoginCommand(t *testing.T) {
	for _, tt := range []struct {
		home, goos, want string
	}{
		{"/homes/b", "linux", "`CODEX_HOME=/homes/b codex login`"},
		{"/homes/My Codex", "darwin", "`CODEX_HOME='/homes/My Codex' codex login`"},
		{"/homes/it's", "linux", "`CODEX_HOME='/homes/it'\\''s' codex login`"},
		{
			`C:\homes\it's`, "windows",
			"`$env:CODEX_HOME = 'C:\\homes\\it''s'; codex login; Remove-Item Env:CODEX_HOME` in PowerShell",
		},
	} {
		assert.Equal(t, tt.want, homeLoginCommand(tt.home, tt.goos), tt.home)
	}
}

func TestRetryKeepsNewestListing(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)
	env.run(t)

	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.Name, th.UpdatedAt = "renamed", ago(time.Minute)
	})
	env.clock.Advance(time.Minute)
	env.run(t)

	acct := env.account(t)
	require.Contains(t, acct.Retry, "a")
	assert.Contains(t, string(acct.Retry["a"].Listing), `"renamed"`)
}

func TestLateFailureUsesFetchTime(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(2*time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(time.Hour), message("n", "two")))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		if strings.Contains(string(params), `"a"`) {
			return nil, &codexcloudtest.Error{Code: -32600, Message: "no rollout found for thread id a"}
		}
		// Fetching b takes half an hour of a long backfill.
		env.clock.Advance(30 * time.Minute)
		return env.threads.TurnsList(params)
	})

	env.run(t)

	retry := env.account(t).Retry["a"]
	assert.Equal(t, fetchNow.Add(30*time.Minute), retry.FirstFailure)
	assert.Equal(t, fetchNow.Add(35*time.Minute), retry.NextAttempt)
}

func TestLostMirrorAfterResumeRefetchesHistory(t *testing.T) {
	for _, damage := range []string{"deleted", "truncated"} {
		t.Run(damage, func(t *testing.T) {
			env := newPassEnv(t)
			updated := ago(time.Hour)
			env.threads.Set(codexcloudtest.Thread{
				ID: "t", Name: "thread t", CreatedAt: updated - 3600, UpdatedAt: updated,
				Turns: []codexcloudtest.Turn{{
					ID: "turn-1", Status: "inProgress", StartedAt: updated - 3600,
					Items: []codexcloudtest.Item{
						command("a", "completed", agoMs(time.Hour)), command("b", "completed", agoMs(time.Hour)),
						command("c", "inProgress", 0),
					},
				}},
			})
			env.run(t)
			st, err := env.syncer.Store.LoadThread(passKey, "t")
			require.NoError(t, err)
			require.Equal(t, "b", st.ResumeItemID, "the resume point moved past a and b")
			path := env.mirrorPath(t, "t")
			if damage == "deleted" {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.Truncate(path, 0))
			}

			env.clock.Advance(5 * time.Minute)
			env.run(t)

			records, err := mirror.ReadAll(t.Context(), path)
			require.NoError(t, err)
			assert.Equal(t, []string{"a", "b", "c"}, recordedItemIDs(records))
			assert.Equal(t, mirror.KindThread, records[0].Kind, "the rebuilt file keeps its metadata")
		})
	}
}

func TestDigestsDropTurnsBeforeResume(t *testing.T) {
	env := newPassEnv(t)
	updated := ago(time.Hour)
	env.threads.Set(codexcloudtest.Thread{
		ID: "t", Name: "thread t", CreatedAt: updated - 7200, UpdatedAt: updated,
		Turns: []codexcloudtest.Turn{
			{
				ID: "turn-1", Status: "completed", StartedAt: updated - 7200, CompletedAt: updated - 7100,
				Items: []codexcloudtest.Item{command("a", "completed", agoMs(2*time.Hour))},
			},
			{
				ID: "turn-2", Status: "completed", StartedAt: updated - 3600, CompletedAt: updated - 60,
				Items: []codexcloudtest.Item{command("b", "completed", agoMs(time.Hour))},
			},
		},
	})
	env.run(t)

	st, err := env.syncer.Store.LoadThread(passKey, "t")
	require.NoError(t, err)
	require.Equal(t, "turn-2", st.ResumeTurnID)
	for key := range st.ItemDigests {
		assert.NotContains(t, key, "turn-1", "turn-1 is never listed again")
	}
}

func TestFirstGateReadFailureKeepsStartupConfig(t *testing.T) {
	startup := config.Config{CodexCloud: config.CodexCloudConfig{Present: true, Enabled: true}}
	gates := NewGateSource("/data/config.toml", startup, func() (config.Config, error) {
		return config.Config{}, assert.AnError
	}, func(string) string { return "" }, "/users/a")()

	assert.True(t, gates.Fetching())
	require.ErrorIs(t, gates.ReadErr, assert.AnError)
	assert.Equal(t, "/data/config.toml", gates.ConfigPath)
}

func TestUnreadableThreadStateDuringListingIsLocal(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.run(t)
	path, err := env.syncer.Store.path(passKey, "a", ".state.json")
	require.NoError(t, err)
	// A directory in the state file's place cannot be read on any platform.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))

	env.clock.Advance(10 * time.Minute)
	env.run(t)

	assert.Zero(t, env.account(t).Wait, "a local file problem is not an outage")
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateError, row.State)
	assert.Contains(t, row.Message, "import state")
	assert.Contains(t, row.Message, "The daemon log names the file and the error.")
	assert.NotContains(t, row.Message, env.root, "the status is a sentence, not a raw error")
}

func TestStalledThreadRecoversAfterStopBeforeAccountSave(t *testing.T) {
	env := newPassEnv(t)
	updated := ago(30 * time.Second)
	env.threads.Set(codexcloudtest.Thread{
		ID: "s", Name: "s", CreatedAt: updated - 60, UpdatedAt: updated,
		Turns: []codexcloudtest.Turn{{ID: "s-1", Status: "inProgress", StartedAt: updated - 60}},
	})
	env.run(t)
	env.clock.Advance(25 * time.Hour)
	env.run(t)
	require.True(t, env.account(t).Unfinished["s"].Stalled)

	// The thread changes. The pass commits it, then fails before its final
	// account save: a directory in place of a usage file makes the usage
	// link that runs between the two saves fail.
	env.threads.Update("s", func(th *codexcloudtest.Thread) {
		th.UpdatedAt = env.clock.Now().Add(-10 * time.Second).Unix()
		th.Turns[0].Items = append(th.Turns[0].Items, message("m1", "progress"))
	})
	blocker, err := env.syncer.Store.path(passKey, "s", ".waiting.json")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(blocker, 0o700))
	env.clock.Advance(5 * time.Minute)
	env.run(t)
	row, _ := env.syncer.Status.Row(string(passKey))
	require.Equal(t, StateError, row.State, "the pass stopped between the saves")
	require.NoError(t, os.Remove(blocker))

	// A later item arrives without a new updatedAt.
	env.threads.Update("s", func(th *codexcloudtest.Thread) {
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "more"))
	})
	from := len(env.host.Requests())
	env.clock.Advance(5 * time.Minute)
	env.run(t)

	assert.Contains(t, env.fetchedThreads(from), "s", "the interrupted thread is checked again")
	records, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "s"))
	require.NoError(t, err)
	assert.Contains(t, recordedItemIDs(records), "m2")
}

func TestOffPageRetryStoresNewestListing(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(3*time.Hour), message("m", "one")))
	env.run(t)

	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.Name, th.UpdatedAt = "second name", ago(2*time.Hour) })
	env.clock.Advance(time.Minute)
	env.run(t)
	require.Contains(t, env.account(t).Retry, "a")

	// Renamed again while waiting out its backoff, and pushed off the first
	// listing page by a page of newer threads.
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.Name, th.UpdatedAt = "third name", ago(90*time.Minute) })
	for i := range listPageSize {
		env.threads.Set(finishedThread("t"+strconv.Itoa(i), ago(time.Hour)-int64(i), message("n", "x")))
	}
	env.clock.Advance(time.Minute)
	env.run(t)
	require.Contains(t, string(env.account(t).Retry["a"].Listing), `"third name"`)

	mu.Lock()
	fail = false
	mu.Unlock()
	env.clock.Set(env.account(t).Retry["a"].NextAttempt)
	from := len(env.host.Requests())
	env.run(t)
	require.Equal(t, []string{"a"}, env.fetchedThreads(from), "only the due retry is fetched")

	// The stored record is what a rebuilt mirror file gets as metadata.
	st, err := env.syncer.Store.LoadThread(passKey, "a")
	require.NoError(t, err)
	assert.Contains(t, string(st.Listing), `"third name"`)
}

func TestUnansweredThreadWaitsSoLaterThreadsImport(t *testing.T) {
	env := newPassEnv(t)
	env.syncer.Dial = func(ctx context.Context, o DialOptions) (*Conn, error) {
		o.CallTimeout = 200 * time.Millisecond
		return Dial(ctx, o)
	}
	env.threads.Set(finishedThread("slow", ago(time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
	hang, mu := true, &sync.Mutex{}
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		mu.Lock()
		hanging := hang && strings.Contains(string(params), `"slow"`)
		mu.Unlock()
		if hanging {
			<-env.host.Context().Done()
		}
		return env.threads.TurnsList(params)
	})

	env.run(t)
	acct := env.account(t)
	require.Equal(t, FailNetwork.String(), acct.Wait.Failure, "the timeout still ends the pass")
	require.Contains(t, acct.Retry, "slow")

	// The next pass may come after the retry is due, and OpenAI still
	// does not answer: the other thread imports first all the same.
	env.clock.Set(acct.Retry["slow"].NextAttempt)
	env.run(t)
	_, err := os.Stat(env.mirrorPath(t, "b"))
	require.NoError(t, err, "the thread after the unanswered one imports")
	assert.Contains(t, env.account(t).Retry, "slow")
}

func TestBackfillFoundCountsEachNewMirror(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
	env.threads.Set(finishedThread("c", ago(3*time.Hour), message("o", "three")))
	var found []int
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		row, _ := env.syncer.Status.Row(string(passKey))
		found = append(found, row.Found)
		if strings.Contains(string(params), `"a"`) {
			return nil, &codexcloudtest.Error{Code: -32600, Message: "no rollout found for thread id a"}
		}
		return env.threads.TurnsList(params)
	})

	env.run(t)

	assert.Equal(t, []int{0, 1, 2}, found, "a failed thread's metadata-only file counts too")
}

func TestUnreadableLoginShowsItsOwnProblem(t *testing.T) {
	env := newPassEnv(t)
	env.syncer.ReadLogin = func(home string) (Login, HomeState, error) {
		return Login{}, HomeSignedOut, &LoginReadError{Message: "config.toml is broken.", err: assert.AnError}
	}

	env.run(t)

	row, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)
	assert.Equal(t, StateError, row.State)
	assert.Equal(t, "config.toml is broken.", row.Message, "signing in again would not fix it")
}

func TestLoginOutsideAuthJSONGivesFileStoreFix(t *testing.T) {
	for _, state := range []HomeState{HomeKeyring, HomeEphemeral} {
		env := newPassEnv(t)
		env.syncer.ReadLogin = func(home string) (Login, HomeState, error) { return Login{Home: home}, state, nil }

		env.run(t)

		row, ok := env.syncer.Status.Row("/homes/a")
		require.True(t, ok)
		assert.Contains(t, row.Message, "add `cli_auth_credentials_store = \"file\"` near the top of its "+
			"config.toml, before any [section], and run `codex login`; AgentsView picks up the new login on its next sync.")
		assert.Contains(t, row.Message, "auth.json, a plain file that any program running as you can read",
			"the fix trades the keyring's protection for access")
	}
}

func TestUnreadableConfigSaysLastGoodSettingsKeepImportOff(t *testing.T) {
	startup := config.Config{CodexCloud: config.CodexCloudConfig{Present: true}}
	env := newPassEnv(t)
	env.syncer.Gates = NewGateSource("/data/config.toml", startup, func() (config.Config, error) {
		return config.Config{}, assert.AnError
	}, func(string) string { return "" }, "/users/a")

	env.run(t)

	row, ok := env.syncer.Status.Row(SourceRowKey)
	require.True(t, ok)
	assert.Contains(t, row.Message, "/data/config.toml could not be read")
	assert.Contains(t, row.Message, "Off: enabled is false.", "the user must know import stays off")
	assert.Zero(t, env.calls("thread/list"))
}

func TestLocalErrorStaysLocalWhenTheConnectionEndsRightAfter(t *testing.T) {
	env := newPassEnv(t)
	path, err := env.syncer.Store.path(passKey, "a", ".state.json")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	_, readErr := env.syncer.Store.LoadThread(passKey, "a")
	require.Error(t, readErr)
	// The listing reply arrived, then the server closed the connection
	// before the pass looked at the error.
	conn := &Conn{done: make(chan struct{})}
	conn.setErr(fmt.Errorf("%w: connection reset", ErrClosed))
	p := &pass{Syncer: env.syncer, key: passKey, gates: env.syncer.Gates(), now: env.clock.Now(), conn: conn}

	err = p.finish(t.Context(), readErr)

	require.ErrorIs(t, err, readErr, "the local error is returned for the local error row")
	assert.Zero(t, env.account(t).Wait, "a local file problem is not an outage")
}
