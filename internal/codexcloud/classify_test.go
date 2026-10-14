package codexcloud_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
)

func TestClassify(t *testing.T) {
	rpc := func(code int, message, data string) *codexcloud.RPCError {
		return &codexcloud.RPCError{Code: code, Message: message, Data: []byte(data)}
	}
	for _, tc := range []struct {
		name     string
		method   string
		anchored bool
		err      *codexcloud.RPCError
		want     codexcloud.Category
	}{
		{
			"environment ended wins over its message", "thread/resume", false,
			rpc(-32600, "Invalid request: environment ended", `{"errorType":"environment_terminated"}`),
			codexcloud.CatThreadError,
		},
		{"overloaded", "thread/items/list", false, rpc(-32001, "overloaded", ""), codexcloud.CatAccountUnavailable},
		{
			"shutting down", "thread/list", false,
			rpc(-32600, "server is shutting down", `{"reason":"serverShuttingDown"}`), codexcloud.CatAccountUnavailable,
		},
		{
			"unsupported method", "thread/turns/list", false,
			rpc(-32601, "unsupported app-server method: thread/turns/list", ""), codexcloud.CatProtocolChange,
		},
		{
			"other method-not-found on a thread request", "thread/items/list", false,
			rpc(-32601, "Method not found", ""), codexcloud.CatThreadError,
		},
		{
			"invalid request", "thread/items/list", false,
			rpc(-32600, "Invalid request: unknown field `itemsView`", ""), codexcloud.CatProtocolChange,
		},
		{
			"experimental field", "thread/list", false,
			rpc(-32600, "field requires experimentalApi capability", ""), codexcloud.CatProtocolChange,
		},
		{"anchored item read", "thread/items/list", true, rpc(-32602, "unknown item", ""), codexcloud.CatAnchoredRead},
		{
			"rejected parameter on an unanchored read", "thread/items/list", false,
			rpc(-32602, "bad cursor", ""), codexcloud.CatProtocolChange,
		},
		{
			"rejected itemsView", "thread/turns/list", false,
			rpc(-32602, "unknown variant `notLoaded`", ""), codexcloud.CatProtocolChange,
		},
		{
			"missing thread on a fetch", "thread/items/list", false,
			rpc(-32600, "no rollout found for thread id", ""), codexcloud.CatThreadError,
		},
		{
			"missing thread on a resume", "thread/resume", false,
			rpc(-32600, "no rollout found for thread id", ""), codexcloud.CatThreadError,
		},
		{
			"other error on thread/list", "thread/list", false,
			rpc(-32600, "no rollout found", ""), codexcloud.CatAccountServiceError,
		},
		{
			"other error on initialize", "initialize", false,
			rpc(-32000, "internal error", ""), codexcloud.CatAccountServiceError,
		},
		{
			"unreadable data matches no data rule", "thread/items/list", false,
			rpc(-32600, "no rollout found", `"text"`), codexcloud.CatThreadError,
		},
	} {
		assert.Equal(t, tc.want, codexcloud.Classify(tc.method, tc.anchored, tc.err), tc.name)
	}
}

func TestFailureFor(t *testing.T) {
	file := codexcloud.Login{Store: "file"}
	httpErr := func(status int) error {
		return fmt.Errorf("dial: %w", &codexcloud.HTTPError{Status: status})
	}
	for _, tc := range []struct {
		name string
		err  error
		want codexcloud.Failure
	}{
		{"401", httpErr(http.StatusUnauthorized), codexcloud.FailSignedOut},
		{"403", httpErr(http.StatusForbidden), codexcloud.FailRefused},
		{"429", httpErr(http.StatusTooManyRequests), codexcloud.FailRateLimited},
		{"503", httpErr(http.StatusServiceUnavailable), codexcloud.FailServer},
		{"302", httpErr(http.StatusFound), codexcloud.FailProtocol},
		{"404", httpErr(http.StatusNotFound), codexcloud.FailProtocol},
		{"refresh", codexcloud.ErrRefreshRequested, codexcloud.FailRefreshRequested},
		{"too large", codexcloud.ErrReplyTooLarge, codexcloud.FailTooLarge},
		{"subprotocol", fmt.Errorf("%w: x", codexcloud.ErrProtocolChange), codexcloud.FailProtocol},
		{"rpc protocol", &codexcloud.RPCError{Code: -32600, Message: "Invalid request: x"}, codexcloud.FailProtocol},
		{"rpc overloaded", &codexcloud.RPCError{Code: -32001, Message: "busy"}, codexcloud.FailServer},
		{"network", errors.New("dial tcp: connection refused"), codexcloud.FailNetwork},
	} {
		assert.Equal(t, tc.want, codexcloud.FailureFor(tc.err, file), tc.name)
	}
}

func TestAutoMode401IsKeyring(t *testing.T) {
	err := &codexcloud.HTTPError{Status: http.StatusUnauthorized}
	assert.Equal(t, codexcloud.FailKeyring, codexcloud.FailureFor(err, codexcloud.Login{Store: "auto"}))
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestWaitsDurations(t *testing.T) {
	for _, tc := range []struct {
		failure codexcloud.Failure
		want    []time.Duration
	}{
		{codexcloud.FailRefused, []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}},
		{codexcloud.FailProtocol, []time.Duration{time.Hour, 2 * time.Hour}},
		{codexcloud.FailRateLimited, []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}},
		{codexcloud.FailNetwork, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}},
		{codexcloud.FailServer, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}},
	} {
		clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
		waits := codexcloud.NewWaits(clock.Now)
		var st codexcloud.AccountState
		for i, want := range tc.want {
			require.NoError(t, waits.RecordAccount(&st, tc.failure, 0))
			assert.Equal(t, want, st.Wait.Until.Sub(clock.now), "%s attempt %d", tc.failure, i+1)
		}
	}
}

func TestWaitsRetryAfterAndReady(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	waits := codexcloud.NewWaits(clock.Now)
	var st codexcloud.AccountState
	assert.True(t, waits.AccountReady(&st))

	require.NoError(t, waits.RecordAccount(&st, codexcloud.FailRateLimited, 90*time.Second))
	assert.False(t, waits.AccountReady(&st))
	clock.Advance(89 * time.Second)
	assert.False(t, waits.AccountReady(&st))
	clock.Advance(time.Second)
	assert.True(t, waits.AccountReady(&st))

	require.NoError(t, waits.RecordAccount(&st, codexcloud.FailServer, 0))
	assert.Equal(t, time.Minute, st.Wait.Until.Sub(clock.now), "a new kind of failure starts its own schedule")
	waits.ResetAccount(&st)
	assert.True(t, waits.AccountReady(&st))
	require.NoError(t, waits.RecordAccount(&st, codexcloud.FailServer, 0))
	assert.Equal(t, time.Minute, st.Wait.Until.Sub(clock.now), "success resets the doubling")

	require.Error(t, waits.RecordAccount(&st, codexcloud.FailThread, 0))
}

func TestHomeWaitEndsOnAuthChange(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	waits := codexcloud.NewWaits(clock.Now)
	authTime := clock.now.Add(-time.Hour)
	waits.RecordHome("/homes/a", codexcloud.FailRefreshRequested, authTime)

	assert.False(t, waits.HomeReady("/homes/a", authTime))
	assert.True(t, waits.HomeReady("/homes/b", authTime), "other homes are not held")
	assert.True(t, waits.HomeReady("/homes/a", authTime.Add(time.Second)), "a new login ends the wait")
	assert.True(t, waits.HomeReady("/homes/a", authTime), "an ended wait is forgotten")

	waits.RecordHome("/homes/a", codexcloud.FailSignedOut, authTime)
	clock.Advance(59 * time.Minute)
	assert.False(t, waits.HomeReady("/homes/a", authTime))
	clock.Advance(time.Minute)
	assert.True(t, waits.HomeReady("/homes/a", authTime), "the wait ends after an hour")
}

// A connection set up with a login read before the user signed in again
// carries an older auth.json time. It must not end a wait recorded for
// the newer login.
func TestHomeWaitSurvivesAnOlderLogin(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	waits := codexcloud.NewWaits(clock.Now)
	older := clock.now.Add(-time.Hour)
	newer := clock.now.Add(-time.Minute)
	waits.RecordHome("/homes/a", codexcloud.FailRefreshRequested, newer)

	assert.False(t, waits.HomeReady("/homes/a", older))
	assert.False(t, waits.HomeReady("/homes/a", newer), "the newer login's wait is kept")

	waits.RecordUsageHome("/homes/a", newer)
	assert.False(t, waits.UsageHomeReady("/homes/a", older), "an older login keeps the plan usage wait")
	assert.False(t, waits.UsageHomeReady("/homes/a", newer))
	assert.True(t, waits.UsageHomeReady("/homes/a", newer.Add(time.Second)), "a newer login ends it")
}

func TestAccountsIndependent(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	waits := codexcloud.NewWaits(clock.Now)
	var a, b codexcloud.AccountState
	require.NoError(t, waits.RecordAccount(&a, codexcloud.FailRefused, 0))
	assert.False(t, waits.AccountReady(&a))
	assert.True(t, waits.AccountReady(&b))
}

func TestStatusBoardKeepsLatestRowPerKey(t *testing.T) {
	board := codexcloud.NewStatusBoard()
	board.Set(codexcloud.HomeStatus{Key: "a", Homes: []string{"/homes/a"}, State: codexcloud.StateWaiting})
	board.Set(codexcloud.HomeStatus{Key: "b", State: codexcloud.StateError})
	board.Set(codexcloud.HomeStatus{Key: "a", Homes: []string{"/homes/a"}, State: codexcloud.StateUpToDate})

	rows := board.Snapshot()
	require.Len(t, rows, 2)
	assert.Equal(t, codexcloud.StateUpToDate, rows[0].State)
	assert.Equal(t, "b", rows[1].Key)
	rows[0].Homes[0] = "changed"
	assert.Equal(t, "/homes/a", board.Snapshot()[0].Homes[0], "a snapshot is a copy")
}

func TestMessages(t *testing.T) {
	retryAt := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC)
	assert.Equal(t, "OpenAI is limiting requests from this account. AgentsView retries at "+
		retryAt.Local().Format("Jan 2 15:04")+".",
		codexcloud.Message(codexcloud.FailRateLimited, retryAt), "the time is shown in local time")
	assert.Equal(t, "OpenAI returned an error for a chat. It may have been deleted.",
		codexcloud.Message(codexcloud.FailThread, time.Time{}))
	assert.Contains(t, codexcloud.Message(codexcloud.FailProtocol, time.Time{}),
		"If config.toml sets endpoint or usage_endpoint under [agents.codex-cloud], check those test addresses too.",
		"a mistyped test endpoint also fails the handshake")
	for f := codexcloud.FailSignedOut; f <= codexcloud.FailThread; f++ {
		message := codexcloud.Message(f, retryAt)
		assert.NotEmpty(t, message, "%s", f)
		assert.True(t, strings.HasSuffix(message, "."), "%s", f)
	}
}
