# Live Refresh via SSE Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Usage, Analytics, and Sessions-list views refresh automatically within
seconds of the sync engine persisting new data, via a reusable SSE channel.

**Architecture:** A single `Broadcaster` type in the server package fans out
sync-engine `Emit` calls to connected SSE clients over `GET /api/v1/events`.
Three frontend stores subscribe through a shared `EventSource`-owning store and
refetch on a trailing-edge debounce, keeping existing timed polls as a safety
net.

**Tech Stack:** Go 1.x (stdlib `net/http`, `time`), Svelte 5 (`$state`,
`$effect`), Vitest, native browser `EventSource`.

**Reference spec:**
`docs/superpowers/specs/2026-04-17-live-refresh-sse-design.md`

______________________________________________________________________

## File Structure

**Create:**

- `internal/server/broadcaster.go` — thread-safe fan-out type; implements
  `sync.Emitter`.
- `internal/server/broadcaster_test.go` — unit tests for `Broadcaster`.
- `frontend/src/lib/stores/events.svelte.ts` — ref-counted `EventSource` store
  with debounced subscription API.
- `frontend/src/lib/stores/events.test.ts` — Vitest unit tests for the events
  store.

**Modify:**

- `internal/sync/engine.go` — add `Emitter` interface, `EngineConfig.Emitter`,
  emission points in `syncAllLocked`, `ResyncAll`, `SyncSingleSession`.
- `internal/sync/engine_test.go` — tests for emission on success / non-emission
  on empty sync.
- `internal/server/events.go` — add `handleEvents`.
- `internal/server/server.go` — register `/api/v1/events` route, add
  `WithBroadcaster` option, store broadcaster on `Server` struct.
- `internal/server/server_test.go` — update `setup` to wire a broadcaster into
  both engine and server; add handler + auth integration tests.
- `internal/server/auth.go` — extend the `?token=` allowlist at line 95 to also
  include `/api/v1/events`.
- `cmd/agentsview/main.go` — construct the broadcaster before the engine, pass
  it via `EngineConfig.Emitter`, and via `server.WithBroadcaster`.
- `frontend/src/lib/api/client.ts` — add `watchEvents(onEvent)` helper mirroring
  `watchSession`.
- `frontend/src/lib/components/usage/UsagePage.svelte` — subscribe in `onMount`,
  unsubscribe in `onDestroy`.
- `frontend/src/lib/components/analytics/AnalyticsPage.svelte` — same pattern as
  Usage.
- `frontend/src/lib/stores/sessions.svelte.ts` — subscribe in the store's
  lazy-init path; add 5-minute safety-net `setInterval`.

______________________________________________________________________

## Task 1: Broadcaster type + unit tests

**Files:**

- Create: `internal/server/broadcaster.go`

- Test: `internal/server/broadcaster_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/server/broadcaster_test.go`:

```go
package server

import (
	"sync"
	"testing"
	"time"
)

func TestBroadcaster_EmitFansOutToAllSubscribers(t *testing.T) {
	b := NewBroadcaster()
	sub1, unsub1 := b.Subscribe()
	defer unsub1()
	sub2, unsub2 := b.Subscribe()
	defer unsub2()

	b.Emit("messages")

	for i, sub := range []<-chan Event{sub1, sub2} {
		select {
		case ev := <-sub:
			if ev.Scope != "messages" {
				t.Errorf("sub %d: got scope %q, want %q", i, ev.Scope, "messages")
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timed out waiting for event", i)
		}
	}
}

func TestBroadcaster_EmitIsNonBlockingOnSlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
	slow, unsub := b.Subscribe()
	defer unsub()

	// Don't read from slow. Fill its buffer + one extra; Emit must not block.
	const extra = 5
	done := make(chan struct{})
	go func() {
		for i := 0; i < broadcasterBufferCap+extra; i++ {
			b.Emit("messages")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on slow subscriber")
	}

	// Drain what we can — drop count >= extra, exact count not guaranteed.
	drained := 0
	for {
		select {
		case <-slow:
			drained++
		case <-time.After(50 * time.Millisecond):
			if drained == 0 {
				t.Fatalf("slow subscriber received nothing")
			}
			return
		}
	}
}

func TestBroadcaster_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroadcaster()
	sub, unsub := b.Subscribe()
	unsub()

	b.Emit("messages")

	select {
	case ev, ok := <-sub:
		if ok {
			t.Fatalf("got event after unsubscribe: %v", ev)
		}
		// channel closed by unsubscribe — acceptable
	case <-time.After(100 * time.Millisecond):
		// no delivery — also acceptable
	}
}

func TestBroadcaster_ConcurrentSubscribeAndEmit(t *testing.T) {
	b := NewBroadcaster()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, unsub := b.Subscribe()
			defer unsub()
			b.Emit("sessions")
			select {
			case <-sub:
			case <-time.After(time.Second):
			}
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run TestBroadcaster -v`
Expected: compile error (`undefined: NewBroadcaster`, `undefined: Event`, etc.).

- [ ] **Step 3: Implement the broadcaster**

Create `internal/server/broadcaster.go`:

```go
package server

import gosync "sync"

// broadcasterBufferCap is the per-subscriber buffer size. A slow
// client can fall this many events behind before the broadcaster
// starts dropping events on its channel.
const broadcasterBufferCap = 8

// Event is a refresh signal sent by the sync engine after a pass
// that wrote data. Scope is advisory — subscribers may filter on
// it but are free to treat it as "refetch now".
type Event struct {
	Scope string
}

// Broadcaster fans out Event values from the sync engine to all
// connected SSE clients. It implements sync.Emitter.
type Broadcaster struct {
	mu   gosync.Mutex
	subs map[chan Event]struct{}
}

// NewBroadcaster creates an empty broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan Event]struct{})}
}

// Emit sends an event to every current subscriber. Delivery is
// non-blocking: if a subscriber's buffer is full, the event is
// dropped for that subscriber. The engine never blocks on slow
// clients.
func (b *Broadcaster) Emit(scope string) {
	ev := Event{Scope: scope}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a receive channel for events and an unsubscribe
// function. Calling unsubscribe closes the channel and removes the
// subscription. It is safe to call unsubscribe multiple times.
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, broadcasterBufferCap)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once gosync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, unsub
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run TestBroadcaster -v`
Expected: all four tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/broadcaster.go internal/server/broadcaster_test.go
git commit -m "feat(server): add Broadcaster type for SSE fan-out"
```

______________________________________________________________________

## Task 2: Emitter interface + engine emission

**Files:**

- Modify: `internal/sync/engine.go`

- Test: `internal/sync/engine_test.go`

- [ ] **Step 1: Write the failing tests**

`engine_test.go` uses `package sync`, so the test file cannot import `"sync"`
directly. Add an aliased import to the existing import block at the top of the
file if one is not already present:

```go
import (
	// ... existing imports ...
	gosync "sync"
)
```

Then append to `internal/sync/engine_test.go`:

```go
// fakeEmitter records scopes passed to Emit. Thread-safe so it
// can be called from engine goroutines under test.
type fakeEmitter struct {
	mu     gosync.Mutex
	scopes []string
}

func (f *fakeEmitter) Emit(scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)
}

func (f *fakeEmitter) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.scopes))
	copy(out, f.scopes)
	return out
}

func TestEngine_SyncAllEmitsWhenSessionsChange(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	stats := fx.engine.SyncAll(context.Background(), nil)
	if stats.Synced == 0 {
		t.Fatal("expected Synced > 0")
	}
	got := em.got()
	if len(got) != 1 {
		t.Fatalf("expected 1 emission, got %d: %v", len(got), got)
	}
}

func TestEngine_SyncAllDoesNotEmitOnEmptyRun(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	// No session files — sync finds nothing.
	stats := fx.engine.SyncAll(context.Background(), nil)
	if stats.Synced != 0 {
		t.Fatalf("expected Synced == 0, got %d", stats.Synced)
	}
	if got := em.got(); len(got) != 0 {
		t.Fatalf("expected no emissions, got %v", got)
	}
}

func TestEngine_SyncSingleSessionEmitsOnSuccess(t *testing.T) {
	fx := newEngineFixture(t)
	em := &fakeEmitter{}
	fx.engineWithEmitter(em)

	path := fx.writeClaudeSession(t, "proj", "s1.jsonl", "hello")
	// Seed DB first so SyncSingleSession has something to find.
	fx.engine.SyncPaths([]string{path})

	// Clear emissions from the seed, then append + SyncSingleSession.
	em.mu.Lock()
	em.scopes = em.scopes[:0]
	em.mu.Unlock()

	fx.appendClaudeMessage(t, path, "world")
	sessionID := fx.sessionIDFor(t, path)
	if err := fx.engine.SyncSingleSession(sessionID); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}
	if got := em.got(); len(got) != 1 {
		t.Fatalf("expected 1 emission, got %d: %v", len(got), got)
	}
}
```

NOTE: `newEngineFixture`, `writeClaudeSession`, `appendClaudeMessage`,
`sessionIDFor`, and the `engineWithEmitter` helper should follow whatever
patterns already exist in `engine_test.go`. If similar helpers exist under
different names, reuse them; if not, write small ones that construct an `Engine`
with `EngineConfig{Emitter: em, ...}`. See the existing test file for the
established shape.

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestEngine_Sync -v`
Expected: compile error (`EngineConfig has no field Emitter`, or similar).

- [ ] **Step 3: Add the Emitter interface and EngineConfig field**

In `internal/sync/engine.go`, near the top of the file (after the `EngineConfig`
type or alongside other interface declarations), add:

```go
// Emitter is notified after a sync pass writes data. Implementations
// must be thread-safe; Emit is called from whatever goroutine runs
// the sync pass (e.g., the file watcher, a periodic timer, or a
// handler goroutine triggered by POST /api/v1/sync).
//
// Emit must not block. A slow implementation can delay the sync
// pipeline; see server.Broadcaster for the production implementation,
// which drops events on full per-subscriber buffers.
type Emitter interface {
	Emit(scope string)
}
```

Extend the `EngineConfig` struct (at the top of `engine.go`) with:

```go
	// Emitter, when non-nil, is called once after each sync pass
	// that wrote data. Safe to leave nil (e.g., in PG serve mode
	// where the engine is not run).
	Emitter Emitter
```

- [ ] **Step 4: Store the emitter on the Engine struct**

In the `Engine` struct definition in `engine.go`, add:

```go
	emitter Emitter
```

In `NewEngine`, set it when returning the engine:

```go
	return &Engine{
		db:                      database,
		agentDirs:               dirs,
		machine:                 cfg.Machine,
		blockedResultCategories: blockedCategorySet(cfg.BlockedResultCategories),
		skipCache:               skipCache,
		ephemeral:               cfg.Ephemeral,
		idPrefix:                cfg.IDPrefix,
		pathRewriter:            cfg.PathRewriter,
		emitter:                 cfg.Emitter,
	}
```

- [ ] **Step 5: Add the emission helper and call it from sync paths**

Add a private helper near the bottom of `engine.go`:

```go
// emit fires a refresh event if an emitter is wired. Safe to call
// with a nil emitter.
func (e *Engine) emit(scope string) {
	if e.emitter != nil {
		e.emitter.Emit(scope)
	}
}
```

In `syncAllLocked` (around line 1057), at the end of the function just before
the final `return stats`, add:

```go
	if stats.Synced > 0 {
		e.emit("sessions")
	}
```

In `ResyncAll` (around line 750), at the end just before `return stats`:

```go
	if stats.Synced > 0 {
		e.emit("sync")
	}
```

`SyncSingleSession` (around line 3055) has several `return nil` success paths
(skip, zero results, incremental writes, full writes). Rather than editing each
return site, convert the signature to use a named return and emit once via
`defer`:

Change:

```go
func (e *Engine) SyncSingleSession(sessionID string) error {
```

to:

```go
func (e *Engine) SyncSingleSession(sessionID string) (err error) {
	defer func() {
		if err == nil {
			e.emit("messages")
		}
	}()
```

The session monitor only calls `SyncSingleSession` after already detecting a
file change, so emitting on every nil return — including the rare skip /
zero-results paths — is consistent with the spec's option of "emit
unconditionally on nil error" and keeps the emission point single-source.

- [ ] **Step 6: Run tests to verify they pass**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestEngine_Sync -v`
Expected: all three tests PASS.

- [ ] **Step 7: Run the full sync test suite to check for regressions**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sync/...` Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/sync/engine.go internal/sync/engine_test.go
git commit -m "feat(sync): add Emitter interface and emit on sync completion"
```

______________________________________________________________________

## Task 3: SSE handler + WithBroadcaster option

**Files:**

- Modify: `internal/server/server.go`

- Modify: `internal/server/events.go`

- Modify: `internal/server/server_test.go`

- [ ] **Step 1: Update the test harness to wire a broadcaster**

In `internal/server/server_test.go`, inside `setupWithServerOpts`, replace the
existing `engine := sync.NewEngine(...)` block (around line 114) with a version
that constructs a broadcaster first, wires it through `EngineConfig.Emitter`,
and prepends a `WithBroadcaster` option to the server options:

```go
	broadcaster := server.NewBroadcaster()
	engineCfg := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
			parser.AgentCodex:  {codexDir},
		},
		Machine: "test",
		Emitter: broadcaster,
	}
	engine := sync.NewEngine(database, engineCfg)

	// Prepend so caller-provided srvOpts can still override.
	srvOpts = append([]server.Option{server.WithBroadcaster(broadcaster)}, srvOpts...)
	srv := server.New(cfg, database, engine, srvOpts...)
```

Also add `broadcaster *server.Broadcaster` to the `testEnv` struct and populate
it in the return:

```go
	return &testEnv{
		srv:         srv,
		handler:     wrappedHandler,
		db:          database,
		engine:      engine,
		broadcaster: broadcaster,
		claudeDir:   claudeDir,
		dataDir:     dir,
	}
```

- [ ] **Step 2: Write the failing integration tests**

Append to `internal/server/server_test.go`:

```go
func TestEvents_StreamsDataChangedAfterSync(t *testing.T) {
	te := setup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(w, req)
		close(done)
	}()

	// Give the handler time to subscribe.
	time.Sleep(100 * time.Millisecond)

	// Emit directly via the broadcaster to isolate the handler
	// from sync engine timing.
	te.broadcaster.Emit("messages")

	te.waitForSSEEvent(t, w, "data_changed", 3*time.Second)
	cancel()
	<-done
}

func TestEvents_ReturnsServiceUnavailableInPGMode(t *testing.T) {
	// A server with engine == nil (PG serve mode) must not stream.
	te := setupPGMode(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "300" {
		t.Errorf("got Retry-After %q, want 300", got)
	}
}
```

`setupPGMode(t)` is a tiny helper alongside `setup`; if one doesn't already
exist, add it in the test file: constructs
`server.New(cfg, database, nil /*engine*/)` without a broadcaster and wraps the
handler the same way `setup` does.

- [ ] **Step 3: Run tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run "TestEvents_|TestBroadcaster" -v`
Expected: compile errors (`undefined: server.WithBroadcaster`, route not
registered, handler missing).

- [ ] **Step 4: Add WithBroadcaster option to Server**

In `internal/server/server.go`, add a `broadcaster *Broadcaster` field to the
`Server` struct (near the `engine` field, around line 37) and a new Option below
the existing ones (near line 126):

```go
// WithBroadcaster wires an event broadcaster into the server so the
// /api/v1/events handler has something to subscribe to. Required for
// live-refresh SSE; absent in PG serve mode where the engine is nil.
func WithBroadcaster(b *Broadcaster) Option {
	return func(s *Server) { s.broadcaster = b }
}
```

- [ ] **Step 5: Register the /api/v1/events route**

In `internal/server/server.go`, in the `routes()` method, add the registration
next to the existing session-watch SSE route (around line 171):

```go
	// SSE: Do not use timeout, as this is a long-lived connection.
	s.mux.HandleFunc(
		"GET /api/v1/events", s.handleEvents,
	)
```

- [ ] **Step 6: Implement handleEvents**

In `internal/server/events.go`, append:

```go
func (s *Server) handleEvents(
	w http.ResponseWriter, r *http.Request,
) {
	if s.engine == nil || s.broadcaster == nil {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusServiceUnavailable,
			"events not available in this mode")
		return
	}

	stream, err := NewSSEStream(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"streaming not supported")
		return
	}

	sub, unsub := s.broadcaster.Subscribe()
	defer unsub()

	heartbeat := time.NewTicker(pollInterval * heartbeatTicks)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			stream.SendJSON("data_changed",
				map[string]string{"scope": ev.Scope})
		case <-heartbeat.C:
			stream.Send("heartbeat",
				time.Now().Format(time.RFC3339))
		}
	}
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run "TestEvents_|TestBroadcaster" -v`
Expected: all tests PASS.

- [ ] **Step 8: Run full server + sync test suites to check for regressions**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/... ./internal/sync/...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/server/events.go internal/server/server.go internal/server/server_test.go
git commit -m "feat(server): add /api/v1/events SSE handler and WithBroadcaster option"
```

______________________________________________________________________

## Task 4: Extend authMiddleware for /api/v1/events

**Files:**

- Modify: `internal/server/auth.go`

- Modify: `internal/server/server_test.go`

- [ ] **Step 1: Write the failing auth tests**

Append to `internal/server/server_test.go`:

```go
func withAuth(token string) setupOption {
	return func(c *config.Config) {
		c.RequireAuth = true
		c.AuthToken = token
	}
}

func TestEvents_AuthViaQueryTokenSucceeds(t *testing.T) {
	te := setup(t, withAuth("secret"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/events?token=secret", nil).WithContext(ctx)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	te.broadcaster.Emit("messages")
	te.waitForSSEEvent(t, w, "data_changed", 2*time.Second)

	cancel()
	<-done
}

func TestEvents_AuthViaBearerHeaderSucceeds(t *testing.T) {
	te := setup(t, withAuth("secret"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	te.broadcaster.Emit("messages")
	te.waitForSSEEvent(t, w, "data_changed", 2*time.Second)

	cancel()
	<-done
}

func TestEvents_AuthMissingTokenReturns401(t *testing.T) {
	te := setup(t, withAuth("secret"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", w.Code)
	}
}

func TestEvents_AuthInvalidTokenReturns401(t *testing.T) {
	te := setup(t, withAuth("secret"))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/events?token=wrong", nil)
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify the query-token tests fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run "TestEvents_Auth" -v`
Expected: `TestEvents_AuthViaQueryTokenSucceeds` FAILS (401 because `auth.go:95`
rejects `?token=` on non-`/watch` paths).
`TestEvents_AuthViaBearerHeaderSucceeds`,
`TestEvents_AuthMissingTokenReturns401`, and
`TestEvents_AuthInvalidTokenReturns401` should already PASS.

- [ ] **Step 3: Extend the authMiddleware query-param allowlist**

In `internal/server/auth.go` at line 95, replace:

```go
		} else if qt := r.URL.Query().Get("token"); qt != "" && strings.HasSuffix(r.URL.Path, "/watch") {
			provided = qt
```

with:

```go
		} else if qt := r.URL.Query().Get("token"); qt != "" && isSSEPath(r.URL.Path) {
			provided = qt
```

Add the helper at the bottom of `auth.go`:

```go
// isSSEPath reports whether the given path is a server-sent events
// endpoint that accepts a ?token= query parameter in place of the
// Authorization header. The query-param fallback exists because
// browser EventSource cannot set headers.
func isSSEPath(path string) bool {
	return strings.HasSuffix(path, "/watch") || path == "/api/v1/events"
}
```

(With a base-path deployment, `http.StripPrefix` runs before `authMiddleware` —
so `r.URL.Path == "/api/v1/events"` is the correct comparison regardless of
prefix.)

- [ ] **Step 4: Run tests to verify they pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run "TestEvents_Auth" -v`
Expected: all four tests PASS.

- [ ] **Step 5: Run full server test suite**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/server/...` Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/auth.go internal/server/server_test.go
git commit -m "feat(server): allow ?token= query auth on /api/v1/events"
```

______________________________________________________________________

## Task 5: Wire broadcaster in main.go

**Files:**

- Modify: `cmd/agentsview/main.go`

- [ ] **Step 1: Read main.go to locate the engine + server construction**

Run: `grep -n "sync.NewEngine\|server.New\b" cmd/agentsview/main.go` Expected:
lines reporting where the engine is built and where `server.New` is called.

- [ ] **Step 2: Construct the broadcaster and wire it**

Just before the `sync.NewEngine(...)` call in `main.go` (around line 103), add:

```go
	broadcaster := server.NewBroadcaster()
```

Modify the `sync.EngineConfig` literal to set `Emitter`:

```go
	engine = sync.NewEngine(database, sync.EngineConfig{
		AgentDirs:               cfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
		Emitter:                 broadcaster,
	})
```

Modify the `server.New(...)` call (around line 192) to include the option. Add
it alongside the existing options:

```go
	srv := server.New(cfg, database, engine,
		server.WithVersion(server.VersionInfo{...}),
		server.WithDataDir(cfg.DataDir),
		server.WithBaseContext(ctx),
		server.WithBroadcaster(broadcaster),
	)
```

(Keep the exact argument list consistent with whatever is already there. If the
existing call uses different options, preserve them and append
`server.WithBroadcaster(broadcaster)` at the end.)

- [ ] **Step 3: Verify PG serve mode (`pg serve` subcommand) is not affected**

Run: `grep -n "server.New" cmd/agentsview/pg.go` Expected: one or more
`server.New` calls in the `pg serve` path. Do NOT pass `WithBroadcaster` there —
PG serve mode should leave the broadcaster nil so `/api/v1/events` returns 503.

- [ ] **Step 4: Build to verify nothing broke**

Run: `CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/` Expected: successful
build, no output.

- [ ] **Step 5: Run vet and the full test suite**

Run:
`CGO_ENABLED=1 go vet -tags fts5 ./... && CGO_ENABLED=1 go test -tags fts5 ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/agentsview/main.go
git commit -m "feat(cmd): wire broadcaster into engine and server"
```

______________________________________________________________________

## Task 6: Frontend `watchEvents` helper

**Files:**

- Modify: `frontend/src/lib/api/client.ts`

- [ ] **Step 1: Locate the existing watchSession export**

Run:
`grep -n "watchSession\|getAuthToken\|getBase" frontend/src/lib/api/client.ts`
Expected: lines including `export function watchSession` (around 413),
`export function getAuthToken` (around 80), and `getBase()`.

- [ ] **Step 2: Add a type for the event payload**

Near other type declarations in `frontend/src/lib/api/client.ts`, add:

```ts
/** Event payload for /api/v1/events data_changed frames. */
export interface DataChangedEvent {
  scope: "messages" | "sessions" | "sync";
}
```

- [ ] **Step 3: Implement watchEvents**

After the existing `watchSession` export (just before `getExportUrl` around line
441), add:

```ts
/** Watch the global sync event stream via SSE.
 *
 * Returns the underlying EventSource so callers can close() it
 * when done. The browser's native EventSource auto-reconnects
 * on transient errors; in PG serve mode the endpoint returns
 * 503 and the browser will retry at its default interval.
 *
 * SECURITY NOTE: Same as watchSession — EventSource cannot set
 * headers, so the auth token is passed as a query parameter
 * for remote connections. This may leak the token into browser
 * history / access logs; accepted per the project threat model.
 */
export function watchEvents(
  onEvent: (e: DataChangedEvent) => void,
): EventSource {
  const url = `${getBase()}/events`;
  const token = getAuthToken();
  const fullUrl = token
    ? `${url}?token=${encodeURIComponent(token)}`
    : url;
  const es = new EventSource(fullUrl);

  es.addEventListener("data_changed", (msg) => {
    try {
      const data = JSON.parse(
        (msg as MessageEvent).data,
      ) as DataChangedEvent;
      onEvent(data);
    } catch {
      // Ignore malformed payloads — treat as a refresh signal.
      onEvent({ scope: "sync" });
    }
  });

  es.onerror = () => {
    // Connection will auto-retry via EventSource spec.
  };

  return es;
}
```

- [ ] **Step 4: Run the type check**

Run: `cd frontend && npx tsc --noEmit` Expected: no type errors.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/api/client.ts
git commit -m "feat(frontend): add watchEvents helper for /api/v1/events"
```

______________________________________________________________________

## Task 7: Frontend events store

**Files:**

- Create: `frontend/src/lib/stores/events.svelte.ts`

- Create: `frontend/src/lib/stores/events.test.ts`

- [ ] **Step 1: Write the failing tests**

Create `frontend/src/lib/stores/events.test.ts`:

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Minimal EventSource stub. Tests control when events fire and
// assert on the number of instances created.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  public url: string;
  public readyState = 1;
  private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {};
  public onerror: ((ev: Event) => void) | null = null;
  public closed = false;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(name: string, cb: (ev: MessageEvent) => void) {
    (this.listeners[name] ||= []).push(cb);
  }

  close() {
    this.closed = true;
  }

  fire(name: string, data: unknown) {
    const payload = { data: JSON.stringify(data) } as MessageEvent;
    (this.listeners[name] || []).forEach((cb) => cb(payload));
  }

  static reset() {
    FakeEventSource.instances = [];
  }
}

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("events store", () => {
  it("opens a single EventSource on first subscribe", async () => {
    const { events } = await import("./events.svelte.js");
    const unsub1 = events.subscribe(() => {});
    const unsub2 = events.subscribe(() => {});
    expect(FakeEventSource.instances).toHaveLength(1);
    unsub1();
    unsub2();
  });

  it("closes the EventSource when the last subscriber leaves", async () => {
    const { events } = await import("./events.svelte.js");
    const unsub = events.subscribe(() => {});
    const es = FakeEventSource.instances[0]!;
    expect(es.closed).toBe(false);
    unsub();
    expect(es.closed).toBe(true);
  });

  it("delivers events to every subscriber", async () => {
    const { events } = await import("./events.svelte.js");
    const received: string[] = [];
    const unsub1 = events.subscribe((e) => received.push(`a:${e.scope}`));
    const unsub2 = events.subscribe((e) => received.push(`b:${e.scope}`));
    FakeEventSource.instances[0]!.fire("data_changed", { scope: "messages" });
    expect(received).toEqual(["a:messages", "b:messages"]);
    unsub1();
    unsub2();
  });

  it("debounces rapid events into one callback per debounce window", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const received: string[] = [];
    const unsub = events.subscribeDebounced(
      (e) => received.push(e.scope),
      100,
    );
    const es = FakeEventSource.instances[0]!;
    es.fire("data_changed", { scope: "messages" });
    es.fire("data_changed", { scope: "messages" });
    es.fire("data_changed", { scope: "sessions" });
    expect(received).toEqual([]);
    vi.advanceTimersByTime(100);
    expect(received).toEqual(["sessions"]); // last-write-wins
    unsub();
    vi.useRealTimers();
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd frontend && npx vitest run src/lib/stores/events.test.ts` Expected:
import errors (`Cannot find module './events.svelte.js'`).

- [ ] **Step 3: Implement the events store**

Create `frontend/src/lib/stores/events.svelte.ts`:

```ts
import { watchEvents, type DataChangedEvent } from "../api/client.js";

type Listener = (e: DataChangedEvent) => void;

class EventsStore {
  private es: EventSource | null = null;
  private listeners = new Set<Listener>();

  /** Subscribe to every event. Returns unsubscribe. */
  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    this.ensureOpen();
    return () => {
      this.listeners.delete(fn);
      if (this.listeners.size === 0) {
        this.close();
      }
    };
  }

  /** Subscribe with a trailing-edge debounce. The callback fires
   * once, `delayMs` after the last event in a burst, with the
   * most recent event's payload. Returns unsubscribe. */
  subscribeDebounced(
    fn: Listener,
    delayMs = 300,
  ): () => void {
    let timer: ReturnType<typeof setTimeout> | null = null;
    let latest: DataChangedEvent | null = null;

    const wrapped: Listener = (e) => {
      latest = e;
      if (timer !== null) clearTimeout(timer);
      timer = setTimeout(() => {
        timer = null;
        if (latest) fn(latest);
        latest = null;
      }, delayMs);
    };

    const unsub = this.subscribe(wrapped);
    return () => {
      unsub();
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    };
  }

  private ensureOpen() {
    if (this.es !== null) return;
    this.es = watchEvents((e) => {
      for (const fn of this.listeners) fn(e);
    });
  }

  private close() {
    if (this.es === null) return;
    this.es.close();
    this.es = null;
  }
}

export const events = new EventsStore();
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd frontend && npx vitest run src/lib/stores/events.test.ts` Expected: all
four tests PASS.

- [ ] **Step 5: Run the full frontend test suite**

Run: `cd frontend && npm test` Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/stores/events.svelte.ts frontend/src/lib/stores/events.test.ts
git commit -m "feat(frontend): add events store with ref-counted EventSource"
```

______________________________________________________________________

## Task 8: Subscribe Usage page to events

**Files:**

- Modify: `frontend/src/lib/components/usage/UsagePage.svelte`

- [ ] **Step 1: Read the current onMount/onDestroy block**

Run:
`sed -n '1,10p;159,175p' frontend/src/lib/components/usage/UsagePage.svelte`
Expected: imports at the top and the `onMount`/`onDestroy` block around lines
159-171.

- [ ] **Step 2: Import the events store**

In the `<script lang="ts">` imports block at the top of `UsagePage.svelte`, add:

```ts
  import { events } from "../../stores/events.svelte.js";
```

- [ ] **Step 3: Wire the subscription in onMount/onDestroy**

Replace the existing `onMount`/`onDestroy` blocks (around lines 159-171) with:

```ts
  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let unsubEvents: (() => void) | undefined;

  onMount(() => {
    usage.fetchAll();
    refreshTimer = setInterval(
      () => usage.fetchAll(),
      REFRESH_MS,
    );
    unsubEvents = events.subscribeDebounced(
      () => usage.fetchAll(),
    );
  });

  onDestroy(() => {
    if (refreshTimer !== undefined) {
      clearInterval(refreshTimer);
    }
    unsubEvents?.();
  });
```

- [ ] **Step 4: Type-check the component**

Run: `cd frontend && npx tsc --noEmit` Expected: no errors.

- [ ] **Step 5: Run frontend tests**

Run: `cd frontend && npm test` Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/usage/UsagePage.svelte
git commit -m "feat(usage): refetch on live event in addition to 5-min poll"
```

______________________________________________________________________

## Task 9: Subscribe Analytics page to events

**Files:**

- Modify: `frontend/src/lib/components/analytics/AnalyticsPage.svelte`

- [ ] **Step 1: Read the current refresh wiring**

Run:
`grep -n "onMount\|onDestroy\|REFRESH\|setInterval" frontend/src/lib/components/analytics/AnalyticsPage.svelte`
Expected: the refresh block around lines 44-48.

- [ ] **Step 2: Import the events store**

In `AnalyticsPage.svelte`'s `<script>` imports, add:

```ts
  import { events } from "../../stores/events.svelte.js";
```

- [ ] **Step 3: Wire the subscription**

Mirror Task 8's pattern — add `unsubEvents` alongside the existing
`refreshTimer`, subscribe in `onMount` with
`events.subscribeDebounced(() => analytics.fetchAll())`, and call
`unsubEvents?.()` in `onDestroy`.

- [ ] **Step 4: Type-check**

Run: `cd frontend && npx tsc --noEmit` Expected: no errors.

- [ ] **Step 5: Run frontend tests**

Run: `cd frontend && npm test` Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/components/analytics/AnalyticsPage.svelte
git commit -m "feat(analytics): refetch on live event in addition to 5-min poll"
```

______________________________________________________________________

## Task 10: Subscribe Sessions list to events + safety-net poll

**Files:**

- Modify: `frontend/src/lib/stores/sessions.svelte.ts`

- [ ] **Step 1: Locate the initial fetch path in the sessions store**

Run:
`grep -n "fetchAll\|async load\|class\|export const sessions" frontend/src/lib/stores/sessions.svelte.ts | head -20`
Expected: the main fetch entry point (likely `fetchAll`, `load`, or similar).

- [ ] **Step 2: Wire the subscription once on first fetch**

Add imports at the top of `sessions.svelte.ts`:

```ts
import { events } from "./events.svelte.js";
```

Add private state on the sessions store class (alongside existing fields):

```ts
  private liveRefreshStarted = false;
  private unsubEvents: (() => void) | null = null;
  private safetyNetTimer: ReturnType<typeof setInterval> | null = null;
```

Add a method that starts live refresh exactly once:

```ts
  private startLiveRefresh() {
    if (this.liveRefreshStarted) return;
    this.liveRefreshStarted = true;
    this.unsubEvents = events.subscribeDebounced(
      () => { this.fetchAll(); },
    );
    this.safetyNetTimer = setInterval(
      () => { this.fetchAll(); },
      5 * 60 * 1000,
    );
  }
```

At the end of the existing `fetchAll()` method (or equivalent initial load),
call `this.startLiveRefresh();` so the first fetch ensures live refresh is
active. Keep the call idempotent.

Add a `dispose()` method for symmetry (used only if the store is torn down —
Svelte store singletons typically live for the app lifetime, but exposing it
keeps tests clean):

```ts
  dispose() {
    if (this.unsubEvents) {
      this.unsubEvents();
      this.unsubEvents = null;
    }
    if (this.safetyNetTimer !== null) {
      clearInterval(this.safetyNetTimer);
      this.safetyNetTimer = null;
    }
    this.liveRefreshStarted = false;
  }
```

- [ ] **Step 3: Type-check**

Run: `cd frontend && npx tsc --noEmit` Expected: no errors.

- [ ] **Step 4: Run frontend tests**

Run: `cd frontend && npm test` Expected: PASS.

- [ ] **Step 5: Manual verification**

Run: `make dev` in one terminal and `make frontend-dev` in another, then:

1. Open the browser to the dev server.
1. Open DevTools → Network → filter on `events`. You should see one open
   EventSource connection to `/api/v1/events`.
1. Run an agent that writes a new session file (e.g., `claude` in another
   directory).
1. Within a few seconds, the sessions list in the sidebar should update without
   clicking refresh.
1. Open the Usage tab and the Analytics tab. Repeat — numbers should update
   within seconds of the agent writing to disk.
1. Toggle `require_auth: true` in config, restart, and confirm the EventSource
   connects with `?token=<value>` appended. Confirm data still arrives.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/stores/sessions.svelte.ts
git commit -m "feat(sessions): live-refresh sessions list via events store"
```

______________________________________________________________________

## Self-Review

**Spec coverage check** — every spec section maps to at least one task:

- Architecture (broadcaster, handler, store, three subscribers) → Tasks 1, 3, 7,
  8, 9, 10
- Event contract (`data_changed`, `scope`, heartbeat) → Task 3
- Authentication (`?token=` extension, middleware change) → Task 4
- Backend: engine `Emitter` + emission points → Task 2
- Backend: events handler + SSE framing → Task 3
- Backend: `server.go` wiring + route registration → Tasks 3, 5
- Broadcaster (thread-safe, non-blocking; shutdown is request-context-driven, no
  dedicated Close API) → Task 1 (broadcaster), Task 3 (handler cleanup via defer
  unsub on `r.Context().Done()`)
- Frontend: `watchEvents` → Task 6
- Frontend: events store (ref-counted, debounce) → Task 7
- Frontend: Usage / Analytics / Sessions subscribers → Tasks 8, 9, 10
- Safety-net polling (keep existing on Usage/Analytics, add for Sessions) →
  Tasks 8, 9 (kept), 10 (added)
- PG serve mode (503 fallback) → Task 3 (test), Task 5 (do not pass option)
- Testing: broadcaster / emission / handler / auth / frontend store → Tasks 1,
  2, 3, 4, 7
- Manual verification → Task 10

**Type consistency check:**

- `Emitter` interface defined in `sync` (Task 2), implemented by `Broadcaster`
  in `server` (Task 1). Go's structural typing resolves this at compile time.
- `DataChangedEvent` in `client.ts` (Task 6) used by `events` store (Task 7) and
  all subscribers (Tasks 8-10).
- `events.subscribeDebounced(fn, delayMs?)` signature defined in Task 7, called
  the same way in Tasks 8, 9, 10.
- `Broadcaster.Subscribe()` returns `(<-chan Event, func())` in Task 1; consumed
  by `handleEvents` the same way in Task 3.
- `Broadcaster.Emit(scope string)` signature matches `sync.Emitter.Emit`
  interface in Task 2.

**Placeholder scan:** No "TBD" / "TODO" / "appropriate handling" — every step
has concrete code or concrete commands.

**Frequent commits:** Each task ends with one commit. No cross-task commit
orchestration.
