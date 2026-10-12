package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

const (
	// checkAgainWindow covers an update later in the same second as the one
	// a fetch saw, allowing for clock difference.
	checkAgainWindow = 2 * time.Minute
	// stallLimit stops every-pass selection of a turn that shows no progress.
	stallLimit = 24 * time.Hour
	// retryFirst and retryLimit time a failing thread's retries.
	retryFirst = 5 * time.Minute
	retryLimit = time.Hour
)

// Syncer runs thread sync passes for every ChatGPT login it finds.
type Syncer struct {
	Store     *Store
	Waits     *Waits
	Status    *StatusBoard
	Now       func() time.Time
	Dial      func(context.Context, DialOptions) (*Conn, error)
	Gates     GateSource
	NoSync    bool
	ReadLogin func(home string) (Login, HomeState, error)
	Version   string
	Log       *log.Logger
}

// passEndError stops a pass early for the whole account.
type passEndError struct {
	failure    Failure
	retryAfter time.Duration
	err        error
}

func (e *passEndError) Error() string { return e.err.Error() }
func (e *passEndError) Unwrap() error { return e.err }

// Run is the sync job. A failure that affects one account is recorded in
// that account's state and status, never returned, because the scheduler's
// backoff would delay every other account too.
func (s *Syncer) Run(ctx context.Context) error {
	if s.NoSync {
		return nil
	}
	gates := s.Gates()
	if !gates.Fetching() {
		return nil
	}
	var usable []Login
	for _, home := range gates.Homes {
		login, state, err := s.ReadLogin(home)
		switch {
		case err != nil:
			s.Log.Printf("codex-cloud: %v", err)
			s.homeRow(home, FailSignedOut)
		case state == HomeKeyring:
			s.homeRow(home, FailKeyring)
		case state == HomeEphemeral:
			s.homeRow(home, FailEphemeral)
		case state == HomeSignedOut:
			s.homeRow(home, FailSignedOut)
		case !s.Waits.HomeReady(home, login.AuthModTime):
		default:
			usable = append(usable, login)
		}
	}
	groups, order := GroupByKey(usable)
	for _, key := range order {
		if err := s.runAccount(ctx, gates, key, groups[key]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	return nil
}

func (s *Syncer) homeRow(home string, f Failure) {
	s.Status.Set(HomeStatus{Key: home, Homes: []string{home}, State: StateError, Message: Message(f, time.Time{})})
}

// pass is one account's pass in progress.
type pass struct {
	*Syncer
	key    AccountKey
	gates  Gates
	login  Login
	homes  []string
	conn   *Conn
	now    time.Time
	listed map[string]listedInfo
}

type listedInfo struct {
	raw      jsontext.Value
	archived bool
	subAgent bool
}

// runAccount runs one account's thread step. Only a local failure, such as
// a state file that cannot be written, is returned.
func (s *Syncer) runAccount(ctx context.Context, gates Gates, key AccountKey, logins []Login) error {
	p := &pass{Syncer: s, key: key, gates: gates, now: s.Now(), listed: map[string]listedInfo{}}
	for _, login := range logins {
		p.homes = append(p.homes, login.Home)
	}
	acct, err := p.account()
	if err != nil {
		return err
	}
	if !s.Waits.AccountReady(&acct) {
		p.keepWaitRow(acct)
		return nil
	}
	if err := p.dialFirst(ctx, logins); err != nil {
		return p.finish(err)
	}
	defer func() { _ = p.conn.Close() }()
	return p.finish(p.threads(ctx))
}

// dialFirst connects with the first home whose token works. A 401 marks
// that home and moves to the next home with the same login key.
func (p *pass) dialFirst(ctx context.Context, logins []Login) error {
	var last error
	for _, login := range logins {
		conn, err := p.dial(ctx, login)
		if err == nil {
			p.login, p.conn = login, conn
			return nil
		}
		f := FailureFor(err, login)
		if f != FailSignedOut && f != FailKeyring {
			p.login = login
			return p.accountEnd(err, login)
		}
		p.Log.Printf("codex-cloud: thread host refused the login in %s: %v", login.Home, err)
		p.Waits.RecordHome(login.Home, f, login.AuthModTime)
		p.homeRow(login.Home, f)
		last = err
	}
	return &passEndError{failure: FailNone, err: last}
}

func (p *pass) dial(ctx context.Context, login Login) (*Conn, error) {
	return p.Dial(ctx, DialOptions{
		Endpoint: p.gates.CodexCloud.Endpoint, Login: login, Mode: ModeFetch, Version: p.Version,
	})
}

// accountEnd turns an error that ends the pass into its failure.
func (p *pass) accountEnd(err error, login Login) error {
	end := &passEndError{failure: FailureFor(err, login), err: Redact(err, login.Token)}
	if httpErr, ok := errors.AsType[*HTTPError](err); ok {
		end.retryAfter = httpErr.RetryAfter
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); ok &&
		Classify("thread/list", false, rpcErr) == CatProtocolChange {
		end.failure = FailProtocol
	}
	return end
}

// threads lists, selects, fetches, and then advances the watermarks.
func (p *pass) threads(ctx context.Context) error {
	acct, err := p.account()
	if err != nil {
		return err
	}
	results := map[ListingID]ListingResult{}
	var changed []ListedThread
	var active []string
	for _, id := range Listings {
		result, err := listChanged(ctx, p.conn, id, acct.Watermarks[id], p.storedUpdatedAt)
		if err != nil {
			return p.accountEnd(err, p.login)
		}
		results[id] = result
		archived := id == ListDefaultArchived || id == ListSubAgentArchived
		subAgent := id == ListSubAgentActive || id == ListSubAgentArchived
		for _, thread := range result.Changed {
			info, seen := p.listed[thread.ID]
			if !seen {
				changed = append(changed, thread)
				info.raw = thread.Raw
			}
			info.archived = info.archived || archived
			info.subAgent = info.subAgent || subAgent
			p.listed[thread.ID] = info
		}
		active = append(active, result.Active...)
	}

	selection := buildSelection(p.now, &acct, changed, active)
	backfilled := allBackfilled(acct)
	for i, id := range selection {
		p.progress(backfilled, i, len(selection))
		if err := p.thread(ctx, id); err != nil {
			return err
		}
	}

	return p.updateAccount(func(acct *AccountState) {
		for id, result := range results {
			acct.Watermarks[id] = max(acct.Watermarks[id], result.Newest)
			acct.Backfilled[id] = true
		}
		p.Waits.ResetAccount(acct)
	})
}

func allBackfilled(acct AccountState) bool {
	for _, id := range Listings {
		if !acct.Backfilled[id] {
			return false
		}
	}
	return true
}

func (p *pass) storedUpdatedAt(threadID string) (int64, bool, error) {
	st, err := p.Store.LoadThread(p.key, threadID)
	return st.UpdatedAt, st.UpdatedAt != 0, err
}

// thread fetches and commits one thread under the account lock, so the
// live token monitor's writes land between threads, never inside one.
func (p *pass) thread(ctx context.Context, id string) error {
	unlock := p.Store.Lock(p.key)
	defer unlock()
	st, err := p.Store.LoadThread(p.key, id)
	if err != nil {
		return err
	}
	acct, err := p.Store.LoadAccount(p.key)
	if err != nil {
		return err
	}
	info := p.listed[id]
	listing := info.raw
	retry, retrying := acct.Retry[id]
	if listing == nil && retrying {
		// The watermark may have passed the thread's listing page; without
		// a metadata record its mirror file would never be parsed.
		listing = retry.Listing
	}
	switch {
	case info.archived && st.ArchivedFirstSeen.IsZero():
		st.ArchivedFirstSeen = p.now
	case info.raw != nil && !info.archived:
		st.ArchivedFirstSeen = time.Time{}
	}
	st.IsSubAgent = st.IsSubAgent || info.subAgent

	result, err := p.fetch(ctx, id, st, listing)
	if err != nil {
		f, threadErr := p.threadFailure(err)
		if !threadErr {
			return err
		}
		p.Log.Printf("codex-cloud: thread %s: %v", id, Redact(err, p.login.Token))
		entry := RetryEntry{
			Attempts: retry.Attempts + 1, FirstFailure: retry.FirstFailure,
			Message: Message(f, time.Time{}), Listing: listing,
		}
		if entry.FirstFailure.IsZero() {
			entry.FirstFailure = p.now
		}
		wait := retryFirst
		for range entry.Attempts - 1 {
			wait = min(wait*2, retryLimit)
		}
		entry.NextAttempt = p.now.Add(wait)
		acct.Retry[id] = entry
		if listing != nil {
			// Store the metadata now, so the thread imports even while its
			// history cannot be fetched.
			record := mirror.Record{Kind: mirror.KindThread, FetchedAt: p.now, Data: listing}
			if _, _, err := commitThread(p.Store, p.key, id, st, []mirror.Record{record}, p.now); err != nil {
				return err
			}
		}
		return p.Store.SaveAccount(p.key, acct)
	}

	next, appended, err := commitThread(p.Store, p.key, id, result.Next, result.Records, p.now)
	if err != nil {
		return err
	}
	delete(acct.Retry, id)
	if result.Unfinished {
		acct.Unfinished[id] = UnfinishedEntry{
			InProgress: result.InProgress,
			Stalled: result.InProgress && !next.LastProgress.IsZero() &&
				p.now.Sub(next.LastProgress) >= stallLimit,
			SubAgent: next.IsSubAgent,
		}
	} else {
		delete(acct.Unfinished, id)
	}
	updated := time.Unix(next.UpdatedAt, 0)
	switch {
	case p.now.Sub(updated) < checkAgainWindow:
		acct.CheckAgain[id] = true
	case info.raw == nil && !appended && !result.Unfinished:
		delete(acct.CheckAgain, id)
	}
	return p.Store.SaveAccount(p.key, acct)
}

// fetch fetches one thread, redialing once after an oversized reply to
// retry with one item per page. An oversized turn listing is not retried:
// a smaller page would not shrink it.
func (p *pass) fetch(ctx context.Context, id string, st ThreadState, listing jsontext.Value) (fetchResult, error) {
	result, err := fetchThread(ctx, p.conn, id, st, listing, p.now)
	if !errors.Is(err, ErrReplyTooLarge) {
		return result, err
	}
	if err := p.redial(ctx); err != nil {
		return fetchResult{}, err
	}
	if errors.Is(err, errTurnListTooLarge) {
		return fetchResult{}, err
	}
	result, err = fetchThreadPaged(ctx, p.conn, id, st, listing, p.now, 1)
	if errors.Is(err, ErrReplyTooLarge) {
		if redialErr := p.redial(ctx); redialErr != nil {
			return fetchResult{}, redialErr
		}
	}
	return result, err
}

func (p *pass) redial(ctx context.Context) error {
	_ = p.conn.Close() // the connection already ended with the overflow
	conn, err := p.dial(ctx, p.login)
	if err != nil {
		return p.accountEnd(err, p.login)
	}
	p.conn = conn
	return nil
}

// threadFailure sorts a fetch error: true means it affects only this
// thread. Anything else ends the pass, or is a local failure.
func (p *pass) threadFailure(err error) (Failure, bool) {
	if errors.Is(err, ErrReplyTooLarge) {
		return FailTooLarge, true
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); ok &&
		Classify("thread/items/list", false, rpcErr) == CatThreadError {
		return FailThread, true
	}
	return FailNone, false
}

// finish records how the pass ended. A passEndError is an account failure or an
// early end; anything else is a local failure the job reports.
func (p *pass) finish(err error) error {
	if err == nil {
		acct, loadErr := p.account()
		if loadErr != nil {
			return loadErr
		}
		last := p.now
		row := HomeStatus{Key: string(p.key), Homes: p.homes, State: StateUpToDate, LastSuccess: &last}
		if n := len(acct.Retry); n > 0 {
			row.State, row.RetryCount = StateRetrying, n
			row.OldestFailure, row.Message = oldestFailure(acct)
		}
		p.Status.Set(row)
		return nil
	}
	end, ok := errors.AsType[*passEndError](err)
	if !ok {
		if p.threadEnd(err) {
			return p.finish(p.accountEnd(err, p.login))
		}
		return err
	}
	if end.failure == FailNone {
		// Every home on this key was refused; their rows already say so.
		return nil
	}
	if end.failure == FailRefreshRequested {
		return p.refreshRequested()
	}
	p.Log.Printf("codex-cloud: account pass ended: %v", end.err)
	if err := p.updateAccount(func(acct *AccountState) {
		if recordErr := p.Waits.RecordAccount(acct, end.failure, end.retryAfter); recordErr != nil {
			p.Log.Printf("codex-cloud: %v", recordErr)
		}
	}); err != nil {
		return err
	}
	acct, err := p.account()
	if err != nil {
		return err
	}
	p.keepWaitRow(acct)
	return nil
}

// threadEnd reports whether a fetch error ends the account's pass: a
// dropped connection, an overloaded server, or a protocol change.
func (p *pass) threadEnd(err error) bool {
	if errors.Is(err, ErrProtocolChange) {
		return true
	}
	if _, ok := errors.AsType[*RPCError](err); ok {
		return true
	}
	return p.conn != nil && p.conn.Err() != nil
}

// refreshRequested ends the pass early: committed threads stay, nothing
// goes to the retry set, and no watermark moves. The home waits until its
// login changes, so the next pass uses the next home on the key.
func (p *pass) refreshRequested() error {
	p.Waits.RecordHome(p.login.Home, FailRefreshRequested, p.login.AuthModTime)
	p.homeRow(p.login.Home, FailRefreshRequested)
	return nil
}

func (p *pass) keepWaitRow(acct AccountState) {
	f := failureFromString(acct.Wait.Failure)
	row, _ := p.Status.Row(string(p.key))
	row.Key, row.Homes, row.State = string(p.key), p.homes, StateError
	row.Message = Message(f, acct.Wait.Until)
	p.Status.Set(row)
}

// progress shows "found N so far" during a first backfill, then "N of M".
func (p *pass) progress(backfilled bool, done, total int) {
	row, _ := p.Status.Row(string(p.key))
	row.Key, row.Homes, row.State = string(p.key), p.homes, StateImporting
	row.Message = ""
	if backfilled {
		row.Imported, row.Total, row.Found = done, total, 0
	} else {
		row.Imported, row.Total, row.Found = 0, 0, p.mirrored()
	}
	p.Status.Set(row)
}

// mirrored counts the account's mirror files. It runs only during a first
// backfill.
func (p *pass) mirrored() int {
	dir := filepath.Join(p.Store.root, string(p.key))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), mirrorSuffix) {
			n++
		}
	}
	return n
}

func oldestFailure(acct AccountState) (*time.Time, string) {
	var oldest time.Time
	var message string
	var latest time.Time
	for _, entry := range acct.Retry {
		if oldest.IsZero() || entry.FirstFailure.Before(oldest) {
			oldest = entry.FirstFailure
		}
		if !entry.NextAttempt.Before(latest) {
			latest, message = entry.NextAttempt, entry.Message
		}
	}
	return &oldest, message + " AgentsView keeps retrying them. The daemon log lists each thread. " +
		"Sessions already imported are not affected."
}

func (p *pass) account() (AccountState, error) {
	unlock := p.Store.Lock(p.key)
	defer unlock()
	acct, err := p.Store.LoadAccount(p.key)
	if err != nil {
		return acct, fmt.Errorf("codex-cloud: account state: %w", err)
	}
	return acct, nil
}

func (p *pass) updateAccount(change func(*AccountState)) error {
	unlock := p.Store.Lock(p.key)
	defer unlock()
	acct, err := p.Store.LoadAccount(p.key)
	if err != nil {
		return err
	}
	change(&acct)
	return p.Store.SaveAccount(p.key, acct)
}
