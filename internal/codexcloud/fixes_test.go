package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"os"
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
	assert.Equal(t, Message(FailTooLarge, time.Time{}), row.Message)
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
	assert.Contains(t, second.Message, "Run `CODEX_HOME=/homes/b codex login`")
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
