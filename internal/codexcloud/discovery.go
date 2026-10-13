package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
)

// discoveryRow is the part of a listing row discovery reads.
type discoveryRow struct {
	ID             string         `json:"id"`
	UpdatedAt      *float64       `json:"updatedAt"`
	ThreadSource   string         `json:"threadSource"`
	ParentThreadID string         `json:"parentThreadId"`
	Source         jsontext.Value `json:"source"`
}

// subAgent reports a thread the monitor does not join until a probe shows
// that joining a running sub-agent thread is a warm join with no side
// effects. A delegated dot task names its parent only in its first
// message, so it is not one.
func (r discoveryRow) subAgent() bool {
	if r.ParentThreadID != "" || r.ThreadSource == "subagent" {
		return true
	}
	if r.Source.Kind() != '{' {
		return false
	}
	var source map[string]jsontext.Value
	if err := json.Unmarshal(r.Source, &source); err != nil {
		return false
	}
	_, ok := source["subAgent"]
	return ok
}

// Discover is the live discovery job. It finds threads that may be running
// and hands them to Join; it never waits for joined threads. Work per run
// follows recent activity, not archive size: it reads listing pages only
// while they hold threads updated within the join window.
func (m *Monitor) Discover(ctx context.Context) error {
	gates := m.Gates()
	if !gates.Configured() {
		m.liveOff()
		return nil
	}
	m.flushPending()
	live := !m.NoSync && gates.Fetching() && gates.CodexCloud.LiveTokens
	logins := m.readyLogins(gates)
	groups, order := GroupByKey(logins)
	if !live {
		m.liveOff()
		var text string
		switch {
		case m.NoSync:
			text = "Live tokens: off, because source sync is off for this daemon."
		case !gates.Fetching():
			text = "Live tokens: off, because Codex cloud sync is off."
		default:
			text = "Live tokens: off, because live_tokens = false under [agents.codex-cloud]."
		}
		for _, key := range order {
			m.Status.SetLive(string(key), text)
		}
		return nil
	}
	for _, key := range order {
		err := m.discoverAccount(ctx, gates, key, groups[key][0])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			m.Log.Printf("codex-cloud: live tokens: %v", err)
		}
		m.updateLive(key)
	}
	return nil
}

// readyLogins reads every home's login, skipping homes without a usable
// login and homes waiting after a 401 or a refresh request. Sync reports
// those homes; discovery only skips them.
func (m *Monitor) readyLogins(gates Gates) []Login {
	var logins []Login
	for _, home := range gates.Homes {
		login, state, err := m.ReadLogin(home)
		if err != nil || state != HomeOK || !m.Waits.HomeReady(home, login.AuthModTime) {
			continue
		}
		logins = append(logins, login)
	}
	return logins
}

// liveOff closes every join and forgets stopped accounts: a gate closed,
// and capture starts again when the user turns live_tokens back on.
func (m *Monitor) liveOff() {
	m.closeJoins(func(*join) bool { return true })
	m.wg.Wait()
	m.mu.Lock()
	m.stopped = nil
	m.mu.Unlock()
}

func (m *Monitor) discoverAccount(ctx context.Context, gates Gates, key AccountKey, login Login) error {
	if m.isStopped(key) {
		return nil
	}
	wait, ready, err := m.accountWait(key)
	if err != nil || !ready {
		return err
	}
	conn, err := m.Dial(ctx, DialOptions{
		Endpoint: gates.CodexCloud.Endpoint, Login: login, Mode: ModeFetch, Version: m.Version,
	})
	if err != nil {
		return m.failure(ctx, key, login, "", "", err)
	}
	defer func() { _ = conn.Close() }() // the run is over; a close error changes nothing
	cutoff := m.Now().Add(-joinWindow).Unix()
	params := listParams{Limit: listPageSize, SortKey: "updated_at", SortDirection: "desc"}
	for {
		var page listPage
		if err := conn.Call(ctx, "thread/list", params, &page); err != nil {
			return m.failure(ctx, key, login, "", "thread/list", err)
		}
		if err := m.resetAccount(key, wait); err != nil {
			return err
		}
		wait = WaitState{}
		recentToEnd := len(page.Data) > 0
		for _, raw := range page.Data {
			var row discoveryRow
			if err := json.Unmarshal(raw, &row); err != nil || row.ID == "" || row.UpdatedAt == nil {
				return m.stopCapture(key, fmt.Errorf("%w: unreadable thread/list row", ErrProtocolChange))
			}
			updated := int64(math.Floor(*row.UpdatedAt))
			if updated < cutoff {
				recentToEnd = false
				continue
			}
			if row.subAgent() || m.isJoined(key, row.ID) {
				continue
			}
			end, err := m.check(ctx, conn, key, login, row.ID, updated)
			if err != nil || end {
				return err
			}
		}
		// The listing runs newest first, so a page that ends with an older
		// thread is the last that can hold a recent one.
		if !recentToEnd || len(page.NextCursor) == 0 || page.NextCursor.Kind() == 'n' {
			return nil
		}
		params.Cursor = page.NextCursor
	}
}

func (m *Monitor) isStopped(key AccountKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped[key] != nil
}

func (m *Monitor) isJoined(key AccountKey, threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.joins[key][threadID]
	return ok
}

// check reads a recent thread's newest turn and joins it when the turn is
// in progress. It reports end when the account's run must stop: capture
// stopped, the account must wait, or the connection is gone. An error
// for this thread alone skips it. Only a local failure is returned.
func (m *Monitor) check(
	ctx context.Context, conn *Conn, key AccountKey, login Login, threadID string, updated int64,
) (end bool, err error) {
	turnID, running, err := newestTurn(ctx, conn, threadID)
	if err != nil {
		if rpcErr, ok := errors.AsType[*RPCError](err); ok &&
			Classify("thread/turns/list", false, rpcErr) == CatThreadError {
			m.Log.Printf("codex-cloud: chat %s: thread/turns/list failed: %v", threadID, rpcErr)
			return false, nil
		}
		return true, m.failure(ctx, key, login, threadID, "thread/turns/list", err)
	}
	if !running {
		return false, nil
	}
	handled, err := m.lastHandled(key, threadID)
	if err != nil || turnID == handled {
		return err != nil, err
	}
	if err := m.Join(ctx, key, login, threadID, updated); err != nil {
		return true, err
	}
	// A join that hit an account-wide failure set the account wait or
	// stopped capture; later threads wait for the next run.
	if m.isStopped(key) || !m.Waits.HomeReady(login.Home, login.AuthModTime) {
		return true, nil
	}
	ready, err := m.accountReady(key)
	return !ready || err != nil, err
}

func (m *Monitor) accountReady(key AccountKey) (bool, error) {
	_, ready, err := m.accountWait(key)
	return ready, err
}

// accountWait reads the account's wait and whether it has ended.
func (m *Monitor) accountWait(key AccountKey) (WaitState, bool, error) {
	unlock := m.Store.Lock(key)
	defer unlock()
	acct, err := m.Store.LoadAccount(key)
	if err != nil {
		return WaitState{}, false, err
	}
	return acct.Wait, m.Waits.AccountReady(&acct), nil
}

// resetAccount clears the wait discovery saw before a successful request,
// as a successful sync pass does, so failures between successes start
// again from the shortest wait. A wait another job set since then is
// newer than this success and stays.
func (m *Monitor) resetAccount(key AccountKey, seen WaitState) error {
	if seen == (WaitState{}) {
		return nil
	}
	unlock := m.Store.Lock(key)
	defer unlock()
	acct, err := m.Store.LoadAccount(key)
	if err != nil {
		return err
	}
	if acct.Wait.Failure != seen.Failure || acct.Wait.Attempts != seen.Attempts || !acct.Wait.Until.Equal(seen.Until) {
		return nil
	}
	m.Waits.ResetAccount(&acct)
	return m.Store.SaveAccount(key, acct)
}
