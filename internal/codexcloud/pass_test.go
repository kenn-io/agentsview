package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	"go.kenn.io/agentsview/internal/parser"
)

const passKey AccountKey = "00112233445566778899aabbccddeeff"

type passClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *passClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *passClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *passClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type passEnv struct {
	host    *codexcloudtest.Host
	threads *codexcloudtest.Threads
	clock   *passClock
	syncer  *Syncer
	root    string
	homes   []string
	logins  map[string]Login
}

func newPassEnv(t *testing.T, homes ...string) *passEnv {
	t.Helper()
	if len(homes) == 0 {
		homes = []string{"/homes/a"}
	}
	host := codexcloudtest.NewHost(t)
	threads := codexcloudtest.NewThreads()
	host.ServeThreads(threads)
	root := t.TempDir()
	clock := &passClock{now: fetchNow}
	env := &passEnv{host: host, threads: threads, clock: clock, root: root, homes: homes, logins: map[string]Login{}}
	for i, home := range homes {
		env.logins[home] = Login{
			Home: home, Token: NewToken("token-" + strconv.Itoa(i)), Key: passKey, Store: "file",
			AuthModTime: fetchNow.Add(-time.Hour),
		}
	}
	endpoint := host.URL()
	env.syncer = &Syncer{
		Store: NewStore(root), Waits: NewWaits(clock.Now), Status: NewStatusBoard(), Now: clock.Now,
		Dial: Dial,
		Gates: func() Gates {
			return Gates{
				CodexCloud: config.CodexCloudConfig{Enabled: true, Endpoint: endpoint},
				Homes:      homes,
			}
		},
		ReadLogin: func(home string) (Login, HomeState, error) { return env.logins[home], HomeOK, nil },
		Version:   "test",
		Log:       log.New(io.Discard, "", 0),
	}
	return env
}

func (e *passEnv) run(t *testing.T) {
	t.Helper()
	require.NoError(t, e.syncer.Run(t.Context()))
}

func (e *passEnv) account(t *testing.T) AccountState {
	t.Helper()
	acct, err := e.syncer.Store.LoadAccount(passKey)
	require.NoError(t, err)
	return acct
}

func (e *passEnv) calls(method string) int {
	n := 0
	for _, r := range e.host.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// fetchedThreads lists the thread of every thread/turns/list call since
// call index from.
func (e *passEnv) fetchedThreads(from int) []string {
	var ids []string
	for _, r := range e.host.Requests()[from:] {
		if r.Method == "thread/turns/list" {
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(r.Params, &p)
			ids = append(ids, p.ThreadID)
		}
	}
	return ids
}

func (e *passEnv) mirrorPath(t *testing.T, id string) string {
	t.Helper()
	path, err := e.syncer.Store.MirrorPath(passKey, id)
	require.NoError(t, err)
	return path
}

func finishedThread(id string, updated int64, items ...codexcloudtest.Item) codexcloudtest.Thread {
	return codexcloudtest.Thread{
		ID: id, Name: "thread " + id, CreatedAt: updated - 3600, UpdatedAt: updated,
		Turns: []codexcloudtest.Turn{{
			ID: id + "-turn-1", Status: "completed", StartedAt: updated - 3600, CompletedAt: updated - 60,
			Items: items,
		}},
	}
}

// failThread makes history reads of id fail with a per-thread error.
func (e *passEnv) failThread(id string, fail *bool, mu *sync.Mutex) {
	e.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		mu.Lock()
		failing := *fail
		mu.Unlock()
		if failing && strings.Contains(string(params), `"`+id+`"`) {
			return nil, &codexcloudtest.Error{Code: -32600, Message: "no rollout found for thread id " + id}
		}
		return e.threads.TurnsList(params)
	})
}

func TestFailedFirstImportKeepsMetadata(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)

	env.run(t)

	acct := env.account(t)
	require.Contains(t, acct.Retry, "a")
	assert.NotEmpty(t, acct.Retry["a"].Listing)
	result, ok, err := parser.ParseCodexCloudFile(t.Context(), env.mirrorPath(t, "a"), "m")
	require.NoError(t, err)
	require.True(t, ok, "the session imports with its metadata")
	assert.Equal(t, "thread a", result.Session.SessionName)
}

func TestFailingThreadBacksOffAndDoesNotHoldWatermark(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "ho")))
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)

	env.run(t)
	acct := env.account(t)
	assert.Equal(t, ago(time.Hour), acct.Watermarks[ListDefaultActive], "a failing thread does not hold the watermark")
	assert.Equal(t, fetchNow.Add(5*time.Minute), acct.Retry["a"].NextAttempt)

	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = ago(30 * time.Minute) })
	env.clock.Advance(time.Minute)
	from := len(env.host.Requests())
	env.run(t)
	assert.NotContains(t, env.fetchedThreads(from), "a", "a thread at the top of a listing still waits out its backoff")

	waits := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for _, wait := range waits {
		env.clock.Set(env.account(t).Retry["a"].NextAttempt)
		env.run(t)
		assert.Equal(t, wait, env.account(t).Retry["a"].NextAttempt.Sub(env.clock.Now()))
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	env.clock.Set(env.account(t).Retry["a"].NextAttempt)
	env.run(t)
	assert.NotContains(t, env.account(t).Retry, "a", "a recovered thread leaves the retry set")
}

func TestCheckAgainSameSecond(t *testing.T) {
	env := newPassEnv(t)
	// The local clock runs 90 seconds ahead of OpenAI's.
	serverNow := fetchNow.Add(-90 * time.Second).Unix()
	env.threads.Set(finishedThread("a", serverNow, message("m1", "first")))

	env.run(t)
	require.True(t, env.account(t).CheckAgain["a"])

	env.threads.Update("a", func(th *codexcloudtest.Thread) {
		th.Turns[0].Items = append(th.Turns[0].Items, message("m2", "same second"))
	})
	env.clock.Advance(5 * time.Minute)
	env.run(t)
	records, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "a"))
	require.NoError(t, err)
	assert.Contains(t, recordedItemIDs(records), "m2", "an update hidden behind an equal timestamp is fetched")

	env.clock.Advance(5 * time.Minute)
	env.run(t)
	assert.NotContains(t, env.account(t).CheckAgain, "a", "a later fetch with no change clears the flag")
}

func TestCheckAgainNotSetForOldThread(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", fetchNow.Add(-90*time.Second).Add(-time.Hour).Unix(), message("m", "old")))

	env.run(t)

	assert.NotContains(t, env.account(t).CheckAgain, "a")
}

func bigItem(id string, size int) codexcloudtest.Item {
	return codexcloudtest.Item{ID: id, Type: "agentMessage", Text: strings.Repeat("x", size)}
}

func smallLimitDial(ctx context.Context, o DialOptions) (*Conn, error) {
	o.ReadLimit = 64 << 10
	return Dial(ctx, o)
}

func TestOversizedReplyRetriedOnceThenRetrySet(t *testing.T) {
	env := newPassEnv(t)
	env.syncer.Dial = smallLimitDial
	env.threads.Set(finishedThread("huge", ago(time.Hour), bigItem("h", 70<<10)))
	env.threads.Set(finishedThread("pair", ago(2*time.Hour), bigItem("p1", 40<<10), bigItem("p2", 40<<10)))
	env.threads.Set(finishedThread("after", ago(3*time.Hour), message("m", "fine")))

	env.run(t)

	acct := env.account(t)
	require.Contains(t, acct.Retry, "huge")
	assert.Equal(t, Message(FailTooLarge, time.Time{}), acct.Retry["huge"].Message)
	assert.NotContains(t, acct.Retry, "pair", "one item per page fits")
	records, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "pair"))
	require.NoError(t, err)
	assert.Equal(t, []string{"p1", "p2"}, recordedItemIDs(records))
	_, err = os.Stat(env.mirrorPath(t, "after"))
	require.NoError(t, err, "the pass continues on a fresh connection")
	assert.Len(t, env.host.Handshakes(), 4, "one redial per overflow")
}

func TestOversizedTurnListingUnderFullFailsAtOnce(t *testing.T) {
	turnView = TurnViewFull
	t.Cleanup(func() { turnView = TurnViewNotLoaded })
	env := newPassEnv(t)
	env.syncer.Dial = smallLimitDial
	env.threads.Set(finishedThread("huge", ago(time.Hour), bigItem("h", 70<<10)))

	env.run(t)

	assert.Contains(t, env.account(t).Retry, "huge")
	assert.Equal(t, 1, env.calls("thread/turns/list"), "an oversized turn listing is not retried")
	assert.Zero(t, env.calls("thread/items/list"))
}

func TestRefreshRequestEndsPassEarly(t *testing.T) {
	env := newPassEnv(t, "/homes/a", "/homes/b")
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
	env.threads.Set(finishedThread("c", ago(3*time.Hour), message("o", "three")))
	var once sync.Once
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		if strings.Contains(string(params), `"b"`) {
			// The request goes out before this reply on the same connection,
			// so the client sees it first.
			once.Do(func() {
				_ = env.host.ServerRequest(t.Context(), "account/chatgptAuthTokens/refresh", map[string]any{})
			})
		}
		return env.threads.TurnsList(params)
	})

	env.run(t)

	acct := env.account(t)
	assert.Empty(t, acct.Retry, "cut-off threads go to no retry set")
	assert.Empty(t, acct.Watermarks, "no watermark advances")
	_, err := os.Stat(env.mirrorPath(t, "a"))
	require.NoError(t, err, "work made durable before the request stays")
	row, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)
	assert.Equal(t, Message(FailRefreshRequested, time.Time{}), row.Message)

	before := len(env.host.Handshakes())
	env.run(t)
	handshakes := env.host.Handshakes()[before:]
	require.NotEmpty(t, handshakes)
	assert.Contains(t, handshakes[0].Subprotocols, "openai-bearer.token-1", "the next pass uses the next home on the key")
}

func TestSecondHomeAfter401(t *testing.T) {
	env := newPassEnv(t, "/homes/a", "/homes/b")
	env.host.RefuseBearer("token-0", http.StatusUnauthorized)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))

	env.run(t)

	_, err := os.Stat(env.mirrorPath(t, "a"))
	require.NoError(t, err)
	home, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)
	assert.Equal(t, Message(FailSignedOut, time.Time{}), home.Message)
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateUpToDate, row.State)
}

func TestInterruptedBackfillResumes(t *testing.T) {
	env := newPassEnv(t)
	for i := range 60 {
		env.threads.Set(finishedThread("t"+strconv.Itoa(i), ago(time.Duration(i+1)*time.Hour), message("m"+strconv.Itoa(i), "x")))
	}
	var mu sync.Mutex
	overloaded := true
	env.host.Handle("thread/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		mu.Lock()
		defer mu.Unlock()
		if overloaded && strings.Contains(string(params), "offset-50") {
			return nil, &codexcloudtest.Error{Code: -32001, Message: "overloaded"}
		}
		return env.threads.ThreadList(params)
	})

	env.run(t)
	acct := env.account(t)
	assert.Empty(t, acct.Watermarks)
	assert.Equal(t, FailServer.String(), acct.Wait.Failure)
	row, _ := env.syncer.Status.Row(string(passKey))
	assert.Equal(t, StateError, row.State)

	mu.Lock()
	overloaded = false
	mu.Unlock()
	env.clock.Advance(time.Minute)
	env.run(t)
	for i := range 60 {
		_, err := os.Stat(env.mirrorPath(t, "t"+strconv.Itoa(i)))
		assert.NoError(t, err, "thread %d", i)
	}
}

func TestFoundSoFarUntilAllListings(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
	var during HomeStatus
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		if strings.Contains(string(params), `"b"`) {
			during, _ = env.syncer.Status.Row(string(passKey))
		}
		return env.threads.TurnsList(params)
	})

	env.run(t)

	assert.Equal(t, StateImporting, during.State)
	assert.Equal(t, 1, during.Found, "one thread was mirrored when b started")
	row, _ := env.syncer.Status.Row(string(passKey))
	assert.Equal(t, StateUpToDate, row.State)

	env.threads.Set(finishedThread("c", ago(30*time.Minute), message("o", "three")))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		during, _ = env.syncer.Status.Row(string(passKey))
		return env.threads.TurnsList(params)
	})
	env.clock.Advance(5 * time.Minute)
	env.run(t)
	assert.Equal(t, StateImporting, during.State)
	assert.Equal(t, 1, during.Total, "after the backfill: importing N of M")
}

func TestFullResyncKeepsStateAndFetchesNothing(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	env.run(t)
	turns, items := env.calls("thread/turns/list"), env.calls("thread/items/list")

	env.clock.Advance(5 * time.Minute)
	env.run(t)

	assert.Equal(t, turns, env.calls("thread/turns/list"))
	assert.Equal(t, items, env.calls("thread/items/list"))
}

func TestLostStateRefetchesWithoutDuplicates(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one"), message("n", "two")))
	env.run(t)
	before, err := os.ReadFile(env.mirrorPath(t, "a"))
	require.NoError(t, err)
	states, err := filepath.Glob(filepath.Join(env.root, string(passKey), "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, states)
	for _, path := range states {
		require.NoError(t, os.Remove(path))
	}

	env.clock.Advance(5 * time.Minute)
	env.run(t)

	after, err := os.ReadFile(env.mirrorPath(t, "a"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "recovered records are recognized, so nothing is appended again")
}

// An unchanged pass costs the same calls whatever the archive's size.
func TestCardinalityScaling(t *testing.T) {
	unchangedCalls := func(n int) (int, int) {
		env := newPassEnv(t)
		for i := range n {
			env.threads.Set(finishedThread("t"+strconv.Itoa(i), ago(time.Duration(i+1)*time.Minute*20), message("m"+strconv.Itoa(i), "x")))
		}
		env.run(t)
		lists, turns := env.calls("thread/list"), env.calls("thread/turns/list")
		env.clock.Advance(5 * time.Minute)
		env.run(t)
		return env.calls("thread/list") - lists, env.calls("thread/turns/list") - turns
	}
	smallLists, smallTurns := unchangedCalls(5)
	largeLists, largeTurns := unchangedCalls(120)

	assert.Equal(t, 4, smallLists)
	assert.Equal(t, smallLists, largeLists)
	assert.Zero(t, smallTurns)
	assert.Zero(t, largeTurns)
}
