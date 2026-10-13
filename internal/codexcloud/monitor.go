package codexcloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

const (
	// joinWindow is the join rule's recency limit: a thread whose listing
	// updatedAt is older is never resumed, because an old inProgress turn
	// is usually save lag, and resuming it would be a cold resume. The
	// limit compares OpenAI's clock with this machine's, so it is wide
	// enough that a few minutes of clock drift do not skip running turns.
	joinWindow = 5 * time.Minute
	// maxJoins caps the joined threads of one account; each holds its own
	// connection.
	maxJoins = 10
	// defaultFlushEvery and defaultJoinLimit are the spec's buffer flush
	// interval and the longest a join stays open.
	defaultFlushEvery = time.Minute
	defaultJoinLimit  = 6 * time.Hour

	tokenUsageMethod    = "thread/tokenUsage/updated"
	turnCompletedMethod = "turn/completed"
	modelReroutedMethod = "model/rerouted"
)

// Monitor joins running threads to record their token counts, which OpenAI
// sends only to a client joined while the turn runs. It owns every joined
// connection; discovery only hands it threads to join.
type Monitor struct {
	Store     *Store
	Waits     *Waits
	Status    *StatusBoard
	Now       func() time.Time
	Dial      func(context.Context, DialOptions) (*Conn, error)
	Gates     GateSource
	NoSync    bool
	ReadLogin func(home string) (Login, HomeState, error)
	// Trigger queues one sync run after a joined turn completes, so its
	// items are fetched soon. A trigger already queued counts as success.
	Trigger func() error
	Version string
	Log     *log.Logger

	// flushEvery and joinLimit default to the spec's values; tests shorten
	// them.
	flushEvery time.Duration
	joinLimit  time.Duration
	// flushes counts mirror appends, so tests can see buffering.
	flushes atomic.Int64

	mu      sync.Mutex
	joins   map[AccountKey]map[string]*join
	stopped map[AccountKey]error
	// pending holds records a failed save left behind, per chat. They are
	// saved before any later record of the same chat, because the parser
	// reads token totals in file order.
	pending map[threadKey]pendingBatch
	closed  bool
	wg      sync.WaitGroup
}

type threadKey struct {
	key      AccountKey
	threadID string
}

// pendingBatch is what one chat still has to save: token and turn
// completion records, and the newest turn the monitor saw complete.
// failure is the last save error logged for it, so a save that keeps
// failing the same way is logged once.
type pendingBatch struct {
	records []mirror.Record
	handled string
	failure string
}

// pendingCompactAt is the pending batch size above which compactPending
// drops token records that later ones make redundant.
const pendingCompactAt = 256

// join is one joined thread. A nil *join in Monitor.joins reserves a slot
// while the join is being set up.
type join struct {
	key      AccountKey
	threadID string
	login    Login
	conn     *Conn
	id       string
	model    string
	handled  string
	stop     chan struct{}
	stopOnce sync.Once
}

func (j *join) close() { j.stopOnce.Do(func() { close(j.stop) }) }

// resumeReply is the part of a thread/resume reply the monitor reads.
// resumeKind is sent only by the hosted server.
type resumeReply struct {
	Thread     jsontext.Value `json:"thread"`
	Model      string         `json:"model"`
	ResumeKind string         `json:"resumeKind"`
}

// Join resumes threadID through login when its newest turn is still
// running and listedUpdatedAt is within the join window, then keeps the
// connection until the turn completes. It returns once the join is set
// up. A failure from OpenAI is handled here, as the spec's error rules
// say; only a local failure, such as a state file that cannot be written,
// is returned.
func (m *Monitor) Join(ctx context.Context, key AccountKey, login Login, threadID string, listedUpdatedAt int64) error {
	if !m.reserve(key, threadID) {
		return nil
	}
	joined := false
	defer func() {
		if !joined {
			m.release(key, threadID)
		}
	}()
	if !m.recent(listedUpdatedAt) || !m.Waits.HomeReady(login.Home, login.AuthModTime) {
		return nil
	}
	conn, err := m.Dial(ctx, DialOptions{
		Endpoint: m.Gates().CodexCloud.Endpoint, Login: login, Mode: ModeMonitor, Version: m.Version,
	})
	if err != nil {
		return m.failure(ctx, key, login, threadID, "", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close() // the join ends here; a close error changes nothing
		}
	}()
	turnID, running, err := newestTurn(ctx, conn, threadID)
	if err != nil {
		return m.failure(ctx, key, login, threadID, "thread/turns/list", err)
	}
	handled, err := m.lastHandled(key, threadID)
	if err != nil {
		return err
	}
	if !running || turnID == handled {
		return nil
	}
	// The dial and the turn check can take minutes. The join rule must
	// still hold when the resume is sent, or a stale turn gets a cold
	// resume.
	if !m.recent(listedUpdatedAt) {
		return nil
	}
	var reply resumeReply
	if err := conn.Call(ctx, resumeMethod, ResumeParams{ThreadID: threadID, ExcludeTurns: true}, &reply); err != nil {
		return m.failure(ctx, key, login, threadID, resumeMethod, err)
	}
	switch reply.ResumeKind {
	case "warm":
		if err := m.writeResumeMetadata(key, threadID, reply.Thread); err != nil {
			return err
		}
		id, err := joinID()
		if err != nil {
			return err
		}
		j := &join{
			key: key, threadID: threadID, login: login, conn: conn, id: id, model: reply.Model,
			stop: make(chan struct{}),
		}
		if !m.start(j) {
			return nil
		}
		joined, keep = true, true
		m.updateLive(key)
		return nil
	case "cold":
		// The turn ended before the join, or the listing still shows a
		// turn the server no longer runs. Joining again waits for a newer
		// turn, so a stuck turn causes one cold resume.
		m.Log.Printf("codex-cloud: chat %s: turn %s had ended when AgentsView connected", threadID, turnID)
		// Through flush, so a pending batch with an older handled turn
		// cannot overwrite this one later.
		return m.flush(key, threadID, nil, turnID)
	default:
		return m.stopCapture(key, fmt.Errorf("%w: thread/resume returned resumeKind %q", ErrProtocolChange, reply.ResumeKind))
	}
}

// recent reports whether a listing updatedAt is inside the join window.
func (m *Monitor) recent(updatedAt int64) bool {
	return m.Now().Sub(time.Unix(updatedAt, 0)) <= joinWindow
}

// Joined lists the threads joined for key.
func (m *Monitor) Joined(key AccountKey) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, j := range m.joins[key] {
		if j != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// CloseAll flushes and closes every join and accepts no new ones. The
// daemon calls it on shutdown.
func (m *Monitor) CloseAll() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.closeJoins(func(*join) bool { return true })
	m.wg.Wait()
}

func (m *Monitor) closeJoins(match func(*join) bool) {
	m.mu.Lock()
	var matched []*join
	for _, joins := range m.joins {
		for _, j := range joins {
			if j != nil && match(j) {
				matched = append(matched, j)
			}
		}
	}
	m.mu.Unlock()
	for _, j := range matched {
		j.close()
	}
}

func (m *Monitor) init() {
	if m.joins == nil {
		m.joins = map[AccountKey]map[string]*join{}
	}
	if m.stopped == nil {
		m.stopped = map[AccountKey]error{}
	}
	if m.pending == nil {
		m.pending = map[threadKey]pendingBatch{}
	}
}

// reserve takes a join slot for threadID unless the thread is joined, the
// account is at the cap or has capture stopped, or the monitor is closed.
func (m *Monitor) reserve(key AccountKey, threadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.closed || m.stopped[key] != nil {
		return false
	}
	joins := m.joins[key]
	if joins == nil {
		joins = map[string]*join{}
		m.joins[key] = joins
	}
	if _, ok := joins[threadID]; ok || len(joins) >= maxJoins {
		return false
	}
	joins[threadID] = nil
	return true
}

func (m *Monitor) release(key AccountKey, threadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.joins[key], threadID)
}

// start registers j and runs it. It refuses when the monitor closed,
// capture stopped, or j's home began waiting for a fresh login while the
// join was set up. ended records the home wait before it closes the
// home's joins, so a join it misses is refused here.
func (m *Monitor) start(j *join) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.stopped[j.key] != nil || !m.Waits.HomeReady(j.login.Home, j.login.AuthModTime) {
		return false
	}
	m.joins[j.key][j.threadID] = j
	m.wg.Go(func() { m.run(j) })
	return true
}

// run reads one joined connection until its turn completes, the join
// limit passes, the connection ends, or the monitor closes it.
func (m *Monitor) run(j *join) {
	defer m.updateLive(j.key)
	defer m.release(j.key, j.threadID)
	defer func() { _ = j.conn.Close() }() // leaving: the close error changes nothing
	var buf []mirror.Record
	flush := func() {
		// flush logs a failure and keeps buf as the chat's pending batch.
		_ = m.flush(j.key, j.threadID, buf, j.handled)
		buf = nil
	}
	tick := time.NewTicker(m.every())
	defer tick.Stop()
	leave := time.NewTimer(m.limit())
	defer leave.Stop()
	notes := j.conn.Notifications()
	for {
		select {
		case n, ok := <-notes:
			if !ok {
				flush()
				m.ended(j)
				return
			}
			completed, err := m.handle(j, n, &buf)
			if err != nil {
				flush()
				_ = m.stopCapture(j.key, err) // stopCapture returns no error today
				return
			}
			if completed {
				flush()
				m.trigger()
				return
			}
		case <-tick.C:
			flush()
		case <-leave.C:
			m.Log.Printf("codex-cloud: chat %s: disconnecting after %s; listening in again on a later run if it still runs",
				j.threadID, m.limit())
			m.leave(j, notes, &buf, flush)
			return
		case <-j.stop:
			m.leave(j, notes, &buf, flush)
			return
		}
	}
}

// leave saves what the join holds as it leaves, handling queued
// notifications the way the main loop would: a completed turn queues a
// sync, and one the monitor cannot read stops capture.
func (m *Monitor) leave(j *join, notes <-chan Notification, buf *[]mirror.Record, flush func()) {
	completed, err := m.keepQueued(j, notes, buf)
	flush()
	switch {
	case err != nil:
		_ = m.stopCapture(j.key, err) // stopCapture returns no error today
	case completed:
		m.trigger()
	}
}

// keepQueued records the notifications that already arrived when the join
// leaves. select picks at random among ready cases, so without this a stop
// can win over queued updates, and the last of them may be the reply's final
// token count. It stops at a completed turn or at the first notification it
// cannot record, and reports which.
func (m *Monitor) keepQueued(j *join, notes <-chan Notification, buf *[]mirror.Record) (completed bool, err error) {
	for {
		select {
		case n, ok := <-notes:
			if !ok {
				return false, nil
			}
			completed, err := m.handle(j, n, buf)
			if err != nil || completed {
				return completed, err
			}
		default:
			return false, nil
		}
	}
}

func (m *Monitor) every() time.Duration {
	if m.flushEvery > 0 {
		return m.flushEvery
	}
	return defaultFlushEvery
}

func (m *Monitor) limit() time.Duration {
	if m.joinLimit > 0 {
		return m.joinLimit
	}
	return defaultJoinLimit
}

// wireCounts is one token breakdown as the thread host sends it. A counter
// the host leaves out counts as zero.
type wireCounts struct {
	InputTokens           jsontext.Value `json:"inputTokens"`
	CachedInputTokens     jsontext.Value `json:"cachedInputTokens"`
	CacheWriteInputTokens jsontext.Value `json:"cacheWriteInputTokens"`
	OutputTokens          jsontext.Value `json:"outputTokens"`
	ReasoningOutputTokens jsontext.Value `json:"reasoningOutputTokens"`
	TotalTokens           jsontext.Value `json:"totalTokens"`
}

func (w wireCounts) counts() (mirror.TokenCounts, error) {
	var c mirror.TokenCounts
	fields := []struct {
		raw jsontext.Value
		dst *int64
	}{
		{w.InputTokens, &c.Input},
		{w.CachedInputTokens, &c.CachedInput},
		{w.CacheWriteInputTokens, &c.CacheWriteInput},
		{w.OutputTokens, &c.Output},
		{w.ReasoningOutputTokens, &c.ReasoningOutput},
		{w.TotalTokens, &c.Total},
	}
	for _, f := range fields {
		if len(f.raw) == 0 {
			continue
		}
		n, err := DecodeCount(f.raw)
		if err != nil {
			return c, fmt.Errorf("%w: %w", ErrProtocolChange, err)
		}
		*f.dst = n
	}
	return c, nil
}

type tokenUsageParams struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Total              wireCounts     `json:"total"`
		Last               wireCounts     `json:"last"`
		ModelContextWindow jsontext.Value `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// handle reads one notification into buf. It reports when the joined turn
// completed; an error is a protocol change that stops capture.
func (m *Monitor) handle(j *join, n Notification, buf *[]mirror.Record) (completed bool, err error) {
	now := m.Now()
	switch n.Method {
	case tokenUsageMethod:
		var p tokenUsageParams
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return false, fmt.Errorf("%w: unreadable %s: %w", ErrProtocolChange, n.Method, err)
		}
		if p.ThreadID != j.threadID {
			return false, nil
		}
		data := mirror.TokenUsageData{JoinID: j.id, TurnID: p.TurnID, Model: j.model, ObservedAt: now}
		if data.Total, err = p.TokenUsage.Total.counts(); err != nil {
			return false, err
		}
		if data.Last, err = p.TokenUsage.Last.counts(); err != nil {
			return false, err
		}
		if window := p.TokenUsage.ModelContextWindow; len(window) > 0 && window.Kind() != 'n' {
			if data.ModelContextWindow, err = DecodeCount(window); err != nil {
				return false, fmt.Errorf("%w: %w", ErrProtocolChange, err)
			}
		}
		return false, appendRecord(buf, mirror.KindTokenUsage, data, now)
	case modelReroutedMethod:
		var p struct {
			ThreadID string `json:"threadId"`
			ToModel  string `json:"toModel"`
		}
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return false, fmt.Errorf("%w: unreadable %s: %w", ErrProtocolChange, n.Method, err)
		}
		if p.ThreadID == j.threadID && p.ToModel != "" {
			j.model = p.ToModel
		}
		return false, nil
	case turnCompletedMethod:
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return false, fmt.Errorf("%w: unreadable %s: %w", ErrProtocolChange, n.Method, err)
		}
		if p.ThreadID != j.threadID {
			return false, nil
		}
		// Messages on one connection arrive in order, so every token update
		// for this turn has already arrived.
		j.handled = p.Turn.ID
		data := mirror.TurnCompletedData{JoinID: j.id, TurnID: p.Turn.ID, ObservedAt: now}
		return true, appendRecord(buf, mirror.KindTurnCompleted, data, now)
	default:
		return false, nil
	}
}

func appendRecord(buf *[]mirror.Record, kind mirror.Kind, data any, now time.Time) error {
	raw, err := json.Marshal(data, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("codex-cloud: encode %s record: %w", kind, err)
	}
	*buf = append(*buf, mirror.Record{Kind: kind, FetchedAt: now, Data: raw})
	return nil
}

// flush appends the chat's pending records, then buf, to its mirror file
// and records handled as its newest completed turn, under the account lock
// the fetcher also takes. An empty handled keeps the stored one. When the
// save fails, everything stays pending for the chat's next save, and the
// error is logged unless it repeats the last one logged for the chat.
func (m *Monitor) flush(key AccountKey, threadID string, buf []mirror.Record, handled string) error {
	unlock := m.Store.Lock(key)
	defer unlock()
	// Taking the batch under the account lock keeps the order: no other
	// save of this chat can write newer records first.
	id := threadKey{key, threadID}
	m.mu.Lock()
	m.init()
	batch, had := m.pending[id]
	delete(m.pending, id)
	m.mu.Unlock()
	batch.records = append(batch.records, buf...)
	if handled != "" {
		batch.handled = handled
	}
	if len(batch.records) == 0 && batch.handled == "" {
		return nil
	}
	err := m.save(key, threadID, batch)
	if err != nil {
		if len(batch.records) > pendingCompactAt {
			batch.records = compactPending(batch.records)
		}
		if msg := err.Error(); msg != batch.failure {
			batch.failure = msg
			m.Log.Printf("codex-cloud: chat %s: save token records: %v; retrying on later saves", threadID, err)
		}
		m.mu.Lock()
		m.pending[id] = batch
		m.mu.Unlock()
	}
	if err != nil || had {
		m.updateLive(key)
	}
	return err
}

func (m *Monitor) save(key AccountKey, threadID string, batch pendingBatch) error {
	st, err := m.Store.LoadThread(key, threadID)
	if err != nil {
		return err
	}
	if batch.handled != "" {
		// commitThread saves the state only after the records are durable.
		st.LastHandledTurnID = batch.handled
	}
	if _, _, err := commitThread(m.Store, key, threadID, st, batch.records, m.Now()); err != nil {
		return err
	}
	if len(batch.records) > 0 {
		m.flushes.Add(1)
	}
	return nil
}

// compactPending drops each token record that the next token record, of
// the same join and turn, supersedes. Totals are cumulative, so the parser
// counts the same tokens from the later record alone; it reads them as one
// event spanning several model calls instead of one event per call. A
// record whose total the next one falls below is kept: the counter reset,
// and the earlier total holds tokens the later one does not. A chat whose
// saves keep failing then holds about one token record per turn.
func compactPending(records []mirror.Record) []mirror.Record {
	tokens := make([]*mirror.TokenUsageData, len(records))
	for i, rec := range records {
		if rec.Kind != mirror.KindTokenUsage {
			continue
		}
		var data mirror.TokenUsageData
		if json.Unmarshal(rec.Data, &data) == nil {
			tokens[i] = &data
		}
	}
	kept := records[:0:0]
	var next *mirror.TokenUsageData
	for i := len(records) - 1; i >= 0; i-- {
		cur := tokens[i]
		superseded := cur != nil && next != nil && next.JoinID == cur.JoinID &&
			next.TurnID == cur.TurnID && next.Total.AtLeast(cur.Total)
		if !superseded {
			kept = append(kept, records[i])
		}
		if records[i].Kind == mirror.KindTokenUsage {
			next = cur
		}
	}
	slices.Reverse(kept)
	return kept
}

// flushPending retries every chat's pending batch. flush logs failures.
func (m *Monitor) flushPending() {
	m.mu.Lock()
	ids := make([]threadKey, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.flush(id.key, id.threadID, nil, "")
	}
}

// ended handles a connection the server or the network closed. A token
// refresh request makes the home wait and ends every join through it. A
// message the monitor cannot read stops capture, as unreadable
// notification params do; joining again would only repeat the resume.
func (m *Monitor) ended(j *join) {
	err := j.conn.Err()
	if errors.Is(err, ErrProtocolChange) {
		m.Log.Printf("codex-cloud: chat %s: a message AgentsView cannot read ended the connection", j.threadID)
		_ = m.stopCapture(j.key, Redact(err, j.login.Token)) // stopCapture returns no error today
		return
	}
	if !errors.Is(err, ErrRefreshRequested) {
		m.Log.Printf("codex-cloud: chat %s: connection ended: %v; listening in again on a later run if it still runs",
			j.threadID, Redact(err, j.login.Token))
		return
	}
	m.Log.Printf("codex-cloud: chat %s: OpenAI asked for a fresh login in %s; disconnecting every chat that uses it",
		j.threadID, j.login.Home)
	m.Waits.RecordHome(j.login.Home, FailRefreshRequested, j.login.AuthModTime)
	home := j.login.Home
	m.closeJoins(func(other *join) bool { return other.login.Home == home })
}

func (m *Monitor) trigger() {
	if m.Trigger == nil {
		return
	}
	go func() {
		if err := m.Trigger(); err != nil && !errors.Is(err, context.Canceled) {
			m.Log.Printf("codex-cloud: queue a sync after a turn completed: %v", err)
		}
	}()
}

// failure applies the error rules to a failed dial or call while joining.
// method names the call, or is empty for the dial.
func (m *Monitor) failure(ctx context.Context, key AccountKey, login Login, threadID, method string, err error) error {
	if ctx.Err() != nil {
		// Shutdown or a closed gate, not a failure to record.
		return ctx.Err()
	}
	if rpcErr, ok := errors.AsType[*RPCError](err); ok && method != "" {
		switch Classify(method, false, rpcErr) {
		case CatProtocolChange:
			return m.stopCapture(key, fmt.Errorf("%w: %s: %w", ErrProtocolChange, method, rpcErr))
		case CatAccountUnavailable, CatAccountServiceError:
			// An overloaded server, or any other error from thread/list:
			// the 5xx wait both jobs share.
			return m.recordAccount(key, FailServer, 0)
		default:
			// The thread's own error; the next run tries again if it still
			// runs.
			m.Log.Printf("codex-cloud: chat %s: %s failed: %v", threadID, method, rpcErr)
			return nil
		}
	}
	f := FailureFor(err, login)
	switch f {
	case FailProtocol:
		return m.stopCapture(key, Redact(err, login.Token))
	case FailSignedOut, FailKeyring, FailRefreshRequested:
		m.Log.Printf("codex-cloud: live tokens: %s: %v", login.Home, Redact(err, login.Token))
		m.Waits.RecordHome(login.Home, f, login.AuthModTime)
		return nil
	case FailTooLarge:
		m.Log.Printf("codex-cloud: chat %s: %v", threadID, err)
		return nil
	default:
		m.Log.Printf("codex-cloud: live tokens: %v", Redact(err, login.Token))
		var retryAfter time.Duration
		if httpErr, ok := errors.AsType[*HTTPError](err); ok {
			retryAfter = httpErr.RetryAfter
		}
		return m.recordAccount(key, f, retryAfter)
	}
}

// recordAccount sets the account wait both jobs share.
func (m *Monitor) recordAccount(key AccountKey, f Failure, retryAfter time.Duration) error {
	unlock := m.Store.Lock(key)
	defer unlock()
	acct, err := m.Store.LoadAccount(key)
	if err != nil {
		return err
	}
	if err := m.Waits.RecordAccount(&acct, f, retryAfter); err != nil {
		return err
	}
	return m.Store.SaveAccount(key, acct)
}

// stopCapture turns live capture off for the account until the daemon
// restarts or live_tokens changes, and closes its joins. Sync is not
// stopped: it finds a protocol change on its own next request.
func (m *Monitor) stopCapture(key AccountKey, err error) error {
	m.Log.Printf("codex-cloud: live tokens stopped for this account: %v", err)
	m.mu.Lock()
	m.init()
	m.stopped[key] = err
	m.mu.Unlock()
	m.closeJoins(func(j *join) bool { return j.key == key })
	m.updateLive(key)
	return nil
}

// updateLive sets the account's live token line.
func (m *Monitor) updateLive(key AccountKey) {
	m.mu.Lock()
	m.init()
	stopped := m.stopped[key]
	watching := 0
	for _, j := range m.joins[key] {
		if j != nil {
			watching++
		}
	}
	m.mu.Unlock()
	var text string
	switch {
	case stopped != nil:
		text = "Live tokens: stopped. " + Message(FailProtocol, time.Time{}) +
			" After updating AgentsView or fixing the endpoint, restart AgentsView, or set live_tokens = false" +
			" and back to true, to listen in again."
	case watching == 0:
		text = "Live tokens: on, no running chats to listen in on."
	default:
		text = fmt.Sprintf("Live tokens: listening in on %s.", chats(watching, "running chat"))
	}
	unsaved := m.unsaved(key)
	m.Status.SetLive(string(key), text+pendingNote(unsaved), stopped != nil || unsaved > 0)
}

// unsaved counts the account's chats whose token counts only memory
// holds, or every account's when key is empty.
func (m *Monitor) unsaved(key AccountKey) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id := range m.pending {
		if key == "" || id.key == key {
			n++
		}
	}
	return n
}

// pendingNote warns, after a live token line, about unsaved chats' token
// counts. It is empty when there are none.
func pendingNote(unsaved int) string {
	if unsaved == 0 {
		return ""
	}
	return fmt.Sprintf(" Token counts for %s are not saved yet. AgentsView keeps retrying;"+
		" restarting it before a save works loses them. The daemon log says why saving fails.",
		chats(unsaved, "chat"))
}

// chats counts n of a noun ending in "chat", in the words users see.
func chats(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// lastHandled reads the chat's newest handled turn, counting one still
// waiting to be saved.
func (m *Monitor) lastHandled(key AccountKey, threadID string) (string, error) {
	unlock := m.Store.Lock(key)
	defer unlock()
	m.mu.Lock()
	pending := m.pending[threadKey{key, threadID}].handled
	m.mu.Unlock()
	if pending != "" {
		return pending, nil
	}
	st, err := m.Store.LoadThread(key, threadID)
	return st.LastHandledTurnID, err
}

// writeResumeMetadata starts a mirror file the monitor reaches before sync
// does with the thread from the resume reply. The reply's updatedAt and
// recencyAt are the time of the resume, so they are dropped; times come
// only from listing records.
func (m *Monitor) writeResumeMetadata(key AccountKey, threadID string, thread jsontext.Value) error {
	unlock := m.Store.Lock(key)
	defer unlock()
	path, err := m.Store.MirrorPath(key, threadID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("codex-cloud: stat mirror: %w", err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(thread, &fields); err != nil {
		return fmt.Errorf("%w: unreadable thread/resume thread: %w", ErrProtocolChange, err)
	}
	delete(fields, "updatedAt")
	delete(fields, "recencyAt")
	data, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("codex-cloud: encode resume metadata: %w", err)
	}
	st, err := m.Store.LoadThread(key, threadID)
	if err != nil {
		return err
	}
	now := m.Now()
	record := mirror.Record{Kind: mirror.KindThreadResume, FetchedAt: now, Data: data}
	_, _, err = commitThread(m.Store, key, threadID, st, []mirror.Record{record}, now)
	return err
}

// newestTurn reads the newest turn's ID and whether it is in progress.
func newestTurn(ctx context.Context, c *Conn, threadID string) (id string, running bool, err error) {
	var page turnsPage
	params := turnsParams{ThreadID: threadID, Limit: 1, ItemsView: turnView}
	if err := c.Call(ctx, "thread/turns/list", params, &page); err != nil {
		return "", false, err
	}
	if len(page.Data) == 0 {
		return "", false, nil
	}
	var turn mirror.Turn
	if err := json.Unmarshal(page.Data[0], &turn); err != nil {
		return "", false, fmt.Errorf("%w: unreadable turn: %w", ErrProtocolChange, err)
	}
	return turn.ID, turn.Status == "inProgress", nil
}

func joinID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("codex-cloud: join ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
