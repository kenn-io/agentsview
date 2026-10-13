package codexcloud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/shellquote"
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
	// unreadableAfter stops retries of a thread that kept failing with no
	// new updatedAt in any listing: a deleted thread, or one item larger
	// than the read limit.
	unreadableAfter = 7 * 24 * time.Hour
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
	// UsageClient carries plan usage requests; nil uses a default client.
	UsageClient *http.Client

	usageMu    sync.Mutex
	usageLines map[AccountKey]string
}

// passEndError stops a pass early for the whole account.
type passEndError struct {
	failure    Failure
	retryAfter time.Duration
	err        error
}

func (e *passEndError) Error() string { return e.err.Error() }
func (e *passEndError) Unwrap() error { return e.err }

// Run is the sync job. A failure that affects one account, remote or
// local, is recorded in that account's status, never returned, because the
// scheduler's backoff would delay every other account too.
func (s *Syncer) Run(ctx context.Context) error {
	gates := s.Gates()
	if !gates.Configured() {
		s.Status.Keep(nil)
		return nil
	}
	shown := map[string]bool{}
	readRow, readErr := gates.readErrRow()
	readErr = readErr && !s.NoSync
	if row, off := gates.offRow(s.NoSync); off {
		if readErr {
			// Both share the source row; the last good settings are what
			// keep import off, so the row says so.
			readRow.Message += " " + row.Message
			row = readRow
		}
		s.Status.Set(row)
		s.Status.Keep(map[string]bool{row.Key: true})
		return nil
	}
	if readErr {
		s.Status.Set(readRow)
		shown[readRow.Key] = true
	}
	// Rows for homes and accounts gone from the config drop at the end.
	defer func() { s.Status.Keep(shown) }()
	var usable []Login
	for _, home := range gates.Homes {
		login, state, err := s.ReadLogin(home)
		switch {
		case err != nil:
			s.Log.Printf("codex-cloud: %v", err)
			s.homeRow(gates, home, loginErrorMessage(err))
		case state == HomeKeyring:
			s.homeRow(gates, home, keyringHomeMessage)
		case state == HomeEphemeral:
			s.homeRow(gates, home, Message(FailEphemeral, time.Time{}))
		case state == HomeSignedOut:
			s.homeRow(gates, home, Message(FailSignedOut, time.Time{}))
		case !s.Waits.HomeReady(home, login.AuthModTime):
		default:
			// The home's row, if any, is stale; a failed dial sets it again.
			s.Status.Delete(home)
			usable = append(usable, login)
		}
		shown[home] = true
	}
	groups, order := GroupByKey(usable)
	for _, key := range order {
		shown[string(key)] = true
		if _, ok := s.Status.Row(string(key)); !ok {
			s.Status.Set(HomeStatus{Key: string(key), Homes: loginHomes(groups[key]), State: StateWaiting})
		}
	}
	for _, key := range order {
		err := s.runAccount(ctx, gates, key, groups[key])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// A local failure, such as a state file that cannot be
			// written, stops only this account; the log and status say so.
			s.Log.Printf("codex-cloud: %v", err)
			s.localErrorRow(key, groups[key], err)
		}
	}
	return nil
}

func (s *Syncer) localErrorRow(key AccountKey, logins []Login, err error) {
	row, _ := s.Status.Row(string(key))
	s.Status.Set(HomeStatus{
		Key: string(key), Homes: loginHomes(logins), State: StateError, LastSuccess: row.LastSuccess,
		// The error names a local path; the log has it, and the status
		// stays one plain message.
		Message: "AgentsView could not read or save its import state, so imports from this account are " +
			"paused. The daemon log names the file and the error. Check that AgentsView can read and " +
			"write its data directory; imports resume on the next sync after that.",
	})
}

func loginHomes(logins []Login) []string {
	homes := make([]string, 0, len(logins))
	for _, login := range logins {
		homes = append(homes, login.Home)
	}
	return homes
}

// loginErrorMessage is the status text for a home whose login could not
// be read. Signing in again does not fix every such error, such as a bad
// config.toml, so it is not the signed-out message.
func loginErrorMessage(err error) string {
	if readErr, ok := errors.AsType[*LoginReadError](err); ok {
		return readErr.Message
	}
	return "AgentsView could not read this Codex home's login. The daemon log has the details."
}

func (s *Syncer) homeRow(gates Gates, home, message string) {
	s.Status.Set(HomeStatus{Key: home, Homes: []string{home}, State: StateError,
		Message: withLoginCommand(gates, home, message)})
}

// withLoginCommand points the `codex login` in message at home. A plain
// `codex login` signs in the default home, not another one.
func withLoginCommand(gates Gates, home, message string) string {
	if home == "" || len(gates.Homes) == 0 || home == gates.Homes[0] {
		return message
	}
	return strings.ReplaceAll(message, "`codex login`", homeLoginCommand(home, runtime.GOOS))
}

// homeLoginCommand says how to run `codex login` for a home other than
// the default one: a VAR=value prefix in a POSIX shell, and on Windows a
// PowerShell line that unsets CODEX_HOME again afterwards.
func homeLoginCommand(home, goos string) string {
	if goos == "windows" {
		return "`$env:CODEX_HOME = " + shellquote.PowerShell(home) +
			"; codex login; Remove-Item Env:CODEX_HOME` in PowerShell"
	}
	return "`CODEX_HOME=" + shellquote.POSIX(home) + " codex login`"
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
	// skipUsage ends the pass without the plan usage step.
	skipUsage bool
}

type listedInfo struct {
	raw      jsontext.Value
	archived bool
	subAgent bool
}

// runAccount runs one account's thread step. Only a local failure, such as
// a state file that cannot be written, is returned.
func (s *Syncer) runAccount(ctx context.Context, gates Gates, key AccountKey, logins []Login) error {
	p := &pass{Syncer: s, key: key, gates: gates, now: s.Now(), listed: map[string]listedInfo{}, homes: loginHomes(logins)}
	acct, err := p.account()
	if err != nil {
		return err
	}
	if err := p.threadStep(ctx, acct, logins); err != nil || p.skipUsage {
		return err
	}
	// Plan usage runs even while the thread host waits.
	return s.runUsage(ctx, gates, key, logins)
}

// threadStep runs the thread step when the account wait allows it. Only a
// local failure is returned.
func (p *pass) threadStep(ctx context.Context, acct AccountState, logins []Login) error {
	if !p.Waits.AccountReady(&acct) {
		p.keepWaitRow(acct)
		return nil
	}
	if err := p.dialFirst(ctx, logins); err != nil {
		return p.finish(ctx, err)
	}
	defer func() { _ = p.conn.Close() }()
	return p.finish(ctx, p.threads(ctx))
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
		p.Log.Printf("codex-cloud: OpenAI refused the login in %s: %v", login.Home, Redact(err, login.Token))
		p.Waits.RecordHome(login.Home, f, login.AuthModTime)
		p.homeRow(p.gates, login.Home, Message(f, time.Time{}))
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
	// A thread state file that cannot be read is a local failure, not
	// OpenAI's; it must not start the account's network wait.
	var stateErr error
	stored := func(threadID string) (int64, bool, error) {
		have, ok, err := p.storedUpdatedAt(threadID)
		if err != nil {
			stateErr = err
		}
		return have, ok, err
	}
	for _, id := range Listings {
		result, err := listChanged(ctx, p.conn, id, acct.Watermarks[id], stored)
		if stateErr != nil {
			return stateErr
		}
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
	// A first backfill counts mirror files once, then adds each new one,
	// since reading the growing directory per thread is quadratic. A file
	// the live monitor creates meanwhile for another chat is counted from
	// the next pass; the number only shows progress.
	found := 0
	if !backfilled {
		found = p.mirrored()
	}
	for i, id := range selection {
		p.progress(backfilled, i, len(selection), found)
		had := !backfilled && p.hasMirror(id)
		if err := p.thread(ctx, id); err != nil {
			return err
		}
		if !backfilled && !had && p.hasMirror(id) {
			found++
		}
	}

	return p.updateAccount(func(acct *AccountState) {
		for id, result := range results {
			acct.Watermarks[id] = max(acct.Watermarks[id], result.Newest)
			acct.Backfilled[id] = true
		}
		// A thread waiting out its backoff keeps the newest listing, since
		// the watermark moves past it and the retry may run off this page.
		now := p.Now()
		for id, entry := range acct.Retry {
			if info, ok := p.listed[id]; ok && info.raw != nil {
				acct.Retry[id] = relisted(entry, info.raw, now)
			}
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
	// A first backfill can run for a long time; retry deadlines and turn
	// first-seen times use the time this thread is fetched.
	now := p.Now()
	st, err := p.Store.LoadThread(p.key, id)
	if err != nil {
		return err
	}
	acct, err := p.Store.LoadAccount(p.key)
	if err != nil {
		return err
	}
	// Only a changed thread reaches here once retired; its retries start
	// over.
	delete(acct.Retired, id)
	info := p.listed[id]
	listing := info.raw
	retry, retrying := acct.Retry[id]
	if listing == nil && retrying {
		// The watermark may have passed the thread's listing page; without
		// a metadata record its mirror file would never be parsed.
		listing = retry.Listing
	}
	lost, err := p.mirrorLost(id, st)
	if err != nil {
		return err
	}
	if lost {
		// The mirror file was deleted or cut short after the resume point
		// moved, so history before that point must be read again.
		p.Log.Printf("codex-cloud: chat %s: mirror file missing or shorter than recorded; fetching it again", id)
		st = forgetHistory(st)
		if listing == nil {
			listing = st.Listing
		}
	}
	if listing != nil {
		// A due retry off this pass's listing pages carries a newer record
		// than the one stored when it first failed.
		st.Listing = listing
	}
	switch {
	case info.archived && st.ArchivedFirstSeen.IsZero():
		st.ArchivedFirstSeen = now
	case info.raw != nil && !info.archived:
		st.ArchivedFirstSeen = time.Time{}
	}
	st.IsSubAgent = st.IsSubAgent || info.subAgent

	result, err := p.fetch(ctx, id, st, listing, now)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		f, threadErr := p.threadFailure(err)
		switch {
		case threadErr:
			p.Log.Printf("codex-cloud: chat %s: %v", id, Redact(err, p.login.Token))
			return p.retryLater(id, st, acct, retry, listing, Message(f, time.Time{}), now)
		case errors.Is(err, ErrCallTimeout):
			// The timeout ended the connection, so the pass ends. Without
			// a retry entry the next pass would pick this thread first
			// again, and a thread OpenAI never answers would stop every
			// thread after it.
			p.Log.Printf("codex-cloud: chat %s: %v", id, Redact(err, p.login.Token))
			// The wait counts from the timeout, not from the fetch's
			// start, which can be minutes earlier.
			if recordErr := p.retryLater(id, st, acct, retry, listing, timeoutMessage, p.Now()); recordErr != nil {
				return recordErr
			}
			return err
		default:
			return err
		}
	}

	// The thread state saved below makes the thread look unchanged to the
	// next listing. Mark it to check again first, so a failure before the
	// final account save still brings it back. A stalled turn is not
	// checked again, so the stall mark goes too; the final save sets it
	// again if the turn still shows no progress.
	checkAgain, hadCheckAgain := acct.CheckAgain[id]
	unfinished, stalled := acct.Unfinished[id]
	stalled = stalled && unfinished.Stalled
	if !checkAgain || stalled {
		acct.CheckAgain[id] = true
		if stalled {
			unfinished.Stalled = false
			acct.Unfinished[id] = unfinished
		}
		if err := p.Store.SaveAccount(p.key, acct); err != nil {
			return err
		}
	}
	// The parent is saved with the resume cursor: once the cursor moves,
	// the delegation tag in the first user message is not read again, so
	// a crash before linkUsage must not lose it.
	parent := resolveParent(id, listing, st.ResolvedParent, result.Records, st.ResumeTurnID == "")
	result.Next.ResolvedParent = parent
	next, appended, err := commitThread(p.Store, p.key, id, result.Next, result.Records, now)
	if err != nil {
		return err
	}
	delete(acct.Retry, id)
	if result.Unfinished {
		acct.Unfinished[id] = UnfinishedEntry{
			InProgress: result.InProgress,
			Stalled: result.InProgress && !next.LastProgress.IsZero() &&
				now.Sub(next.LastProgress) >= stallLimit,
			SubAgent: next.IsSubAgent,
		}
	} else {
		delete(acct.Unfinished, id)
	}
	if err := p.linkUsage(p.key, &acct, id, parent, now); err != nil {
		return err
	}
	updated := time.Unix(next.UpdatedAt, 0)
	switch {
	case now.Sub(updated) < checkAgainWindow:
		acct.CheckAgain[id] = true
	case info.raw == nil && !appended && !result.Unfinished:
		delete(acct.CheckAgain, id)
	case hadCheckAgain:
		acct.CheckAgain[id] = checkAgain
	default:
		delete(acct.CheckAgain, id)
	}
	return p.Store.SaveAccount(p.key, acct)
}

// timeoutMessage is the retry message for a thread whose history OpenAI
// did not answer within the call timeout.
const timeoutMessage = "OpenAI did not answer in time when reading a chat."

// retryLater puts a failed thread in the retry set with its backoff, or
// retires it after unreadableAfter. A listing record is stored now, so the
// thread imports with its metadata while its history cannot be fetched.
func (p *pass) retryLater(
	id string, st ThreadState, acct AccountState, retry RetryEntry, listing jsontext.Value,
	message string, now time.Time,
) error {
	entry := RetryEntry{
		Attempts: retry.Attempts + 1, FirstFailure: retry.FirstFailure,
		Message: message, Listing: retry.Listing,
	}
	if entry.Listing == nil {
		// A thread selected without a listing row keeps its stored
		// record, so a later listing with a new updatedAt restarts the
		// unreadable count.
		entry.Listing = st.Listing
	}
	if listing != nil {
		entry = relisted(entry, listing, now)
	}
	if entry.FirstFailure.IsZero() {
		entry.FirstFailure = now
	}
	if now.Sub(entry.FirstFailure) >= unreadableAfter {
		return p.unreadable(id, st, acct, entry, now)
	}
	wait := retryFirst
	for range entry.Attempts - 1 {
		wait = min(wait*2, retryLimit)
	}
	entry.NextAttempt = now.Add(wait)
	acct.Retry[id] = entry
	if listing != nil {
		// Store the metadata now, so the thread imports even while its
		// history cannot be fetched.
		record := mirror.Record{Kind: mirror.KindThread, FetchedAt: now, Data: listing}
		if _, _, err := commitThread(p.Store, p.key, id, st, []mirror.Record{record}, now); err != nil {
			return err
		}
		parent := resolveParent(id, listing, st.ResolvedParent, nil, false)
		if err := p.linkUsage(p.key, &acct, id, parent, now); err != nil {
			return err
		}
	}
	return p.Store.SaveAccount(p.key, acct)
}

// unreadable stops retrying a thread that has failed for unreadableAfter.
// Storing the listing's updatedAt makes later listings see the thread as
// unchanged, so only a new updatedAt selects it again; what is already
// stored stays.
func (p *pass) unreadable(id string, st ThreadState, acct AccountState, entry RetryEntry, now time.Time) error {
	p.Log.Printf("codex-cloud: chat %s: stopped retrying after failing since %s: %s "+
		"Its next change in OpenAI starts the retries again.",
		id, entry.FirstFailure.Format(time.RFC3339), entry.Message)
	if updated, ok := listingUpdatedAt(entry.Listing); ok {
		st.UpdatedAt = updated
	}
	var records []mirror.Record
	if entry.Listing != nil {
		records = []mirror.Record{{Kind: mirror.KindThread, FetchedAt: now, Data: entry.Listing}}
	}
	if _, _, err := commitThread(p.Store, p.key, id, st, records, now); err != nil {
		return err
	}
	delete(acct.Retry, id)
	delete(acct.Unfinished, id)
	delete(acct.CheckAgain, id)
	acct.Retired[id] = true
	return p.Store.SaveAccount(p.key, acct)
}

// relisted stores a newer listing record in a retry entry. A new updatedAt
// restarts the unreadable count, since the thread changed while failing.
func relisted(entry RetryEntry, listing jsontext.Value, now time.Time) RetryEntry {
	old, hadOld := listingUpdatedAt(entry.Listing)
	updated, ok := listingUpdatedAt(listing)
	if hadOld && ok && updated != old {
		entry.FirstFailure = now
	}
	entry.Listing = listing
	return entry
}

// listingUpdatedAt reads the whole-second updatedAt of a listing record.
func listingUpdatedAt(listing jsontext.Value) (int64, bool) {
	if len(listing) == 0 {
		return 0, false
	}
	var meta mirror.Thread
	if err := json.Unmarshal(listing, &meta); err != nil || meta.UpdatedAt == nil {
		return 0, false
	}
	return int64(math.Floor(*meta.UpdatedAt)), true
}

// mirrorLost reports a mirror file that is missing or shorter than the
// recorded offset while the thread has a resume point.
func (p *pass) mirrorLost(id string, st ThreadState) (bool, error) {
	if st.Offset == 0 && st.ResumeTurnID == "" {
		return false, nil
	}
	path, err := p.Store.MirrorPath(p.key, id)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("codex-cloud: stat mirror: %w", err)
	default:
		return info.Size() < st.Offset, nil
	}
}

// fetch fetches one thread, redialing once after an oversized reply to
// retry with one item per page. An oversized turn listing is not retried:
// a smaller page would not shrink it.
func (p *pass) fetch(
	ctx context.Context, id string, st ThreadState, listing jsontext.Value, now time.Time,
) (fetchResult, error) {
	result, err := fetchThread(ctx, p.conn, id, st, listing, now)
	if !errors.Is(err, ErrReplyTooLarge) {
		return result, err
	}
	if err := p.redial(ctx); err != nil {
		return fetchResult{}, err
	}
	if errors.Is(err, errTurnListTooLarge) {
		return fetchResult{}, err
	}
	result, err = fetchThreadPaged(ctx, p.conn, id, st, listing, now, 1)
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
// early end; anything else is a local failure the job reports. A pass cut
// off by shutdown records nothing: it is not OpenAI's failure.
func (p *pass) finish(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
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
			return p.finish(ctx, p.accountEnd(err, p.login))
		}
		return err
	}
	if end.failure == FailNone {
		// Every home on this key was refused; their rows say so, and the
		// account row would claim a sync that is not happening.
		p.Status.Delete(string(p.key))
		return nil
	}
	if end.failure == FailRefreshRequested {
		// The pass that saw a refresh request ends early, plan usage
		// included.
		p.skipUsage = true
	}
	if end.failure == FailRefreshRequested || end.failure == FailSignedOut || end.failure == FailKeyring {
		// A fresh login was requested, or a redial after an oversized
		// reply was refused.
		return p.homeEnd(end.failure)
	}
	p.Log.Printf("codex-cloud: account pass ended: %v", end.err)
	var recordErr error
	if err := p.updateAccount(func(acct *AccountState) {
		recordErr = p.Waits.RecordAccount(acct, end.failure, end.retryAfter)
	}); err != nil {
		return err
	}
	if recordErr != nil {
		return recordErr
	}
	acct, err := p.account()
	if err != nil {
		return err
	}
	p.keepWaitRow(acct)
	return nil
}

// threadEnd reports whether a fetch error ends the account's pass: a
// dropped connection, an overloaded server, or a protocol change. An error
// counts as the dropped connection's only when it is the connection's own
// error. A server can close the connection right after a reply, so a local
// error, such as a state file that cannot be read, can arrive after the
// connection ended and must still be reported as local.
func (p *pass) threadEnd(err error) bool {
	if errors.Is(err, ErrProtocolChange) {
		return true
	}
	if _, ok := errors.AsType[*RPCError](err); ok {
		return true
	}
	if p.conn == nil {
		return false
	}
	connErr := p.conn.Err()
	return connErr != nil && errors.Is(err, connErr)
}

// homeEnd ends the pass early because the current home's login cannot be
// used: committed threads stay, nothing goes to the retry set, and no
// watermark moves. The home waits until its login changes, so the next
// pass uses the next home on the key.
func (p *pass) homeEnd(f Failure) error {
	p.Waits.RecordHome(p.login.Home, f, p.login.AuthModTime)
	p.homeRow(p.gates, p.login.Home, Message(f, time.Time{}))
	p.Status.Delete(string(p.key))
	return nil
}

func (p *pass) keepWaitRow(acct AccountState) {
	f := failureFromString(acct.Wait.Failure)
	row, _ := p.Status.Row(string(p.key))
	row.Key, row.Homes, row.State = string(p.key), p.homes, StateError
	row.Message = Message(f, acct.Wait.Until)
	if f == FailTooLarge {
		// The same failure for one chat retries only that chat, so the
		// pause belongs to the account row alone.
		row.Message += " Imports from this account are paused, and AgentsView retries automatically."
	}
	p.Status.Set(row)
}

// progress shows "found N so far" during a first backfill, then "N of M".
func (p *pass) progress(backfilled bool, done, total, found int) {
	row, _ := p.Status.Row(string(p.key))
	row.Key, row.Homes, row.State = string(p.key), p.homes, StateImporting
	row.Message = ""
	if backfilled {
		row.Imported, row.Total, row.Found = done, total, 0
	} else {
		row.Imported, row.Total, row.Found = 0, 0, found
	}
	p.Status.Set(row)
}

// mirrored counts the account's mirror files. It runs only during a first
// backfill; the count is for display, so an unreadable directory counts
// as empty.
func (p *pass) mirrored() int {
	dir, err := p.Store.accountDir(p.key)
	if err != nil {
		return 0
	}
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

// hasMirror reports whether the thread has a mirror file, for the
// backfill count.
func (p *pass) hasMirror(id string) bool {
	path, err := p.Store.MirrorPath(p.key, id)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
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
	days := int(unreadableAfter / (24 * time.Hour))
	return &oldest, message + fmt.Sprintf(" AgentsView retries a failed chat for %d days; a change to the chat in OpenAI "+
		"restarts the %d days. The daemon log lists each chat by its ID. Chats already imported are not affected.", days, days)
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

// resolveParent applies the spec's resolved-parent rules: the listing's
// parentThreadId, else the parent its source names for a spawned
// sub-agent, else the parent a delegated dot task names in its first user
// message. The delegation tag is read only when fresh, that is when this
// fetch read the thread from its start; the result is then kept in the
// thread state as prior.
func resolveParent(threadID string, listing jsontext.Value, prior string, records []mirror.Record, fresh bool) string {
	if len(listing) > 0 {
		var meta mirror.Thread
		if err := json.Unmarshal(listing, &meta); err == nil {
			for _, parent := range []string{meta.ParentThreadID, parser.CodexCloudSpawnParent(meta.Source)} {
				if parent != "" && parent != threadID {
					return parent
				}
			}
		}
	}
	if prior != "" || !fresh {
		return prior
	}
	for _, record := range records {
		if record.Kind != mirror.KindItem {
			continue
		}
		var row mirror.ItemRecord
		var head mirror.ItemHeader
		if json.Unmarshal(record.Data, &row) != nil || json.Unmarshal(row.Item, &head) != nil {
			continue
		}
		if head.Type == "userMessage" {
			return parser.CodexCloudDelegatedParent(threadID, row.Item)
		}
	}
	return ""
}
