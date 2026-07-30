# Codex Open-File Freshness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep Activity, session metadata, and daily usage current within two
minutes while Codex appends to an open rollout, including sessions resumed after
days of inactivity.

**Architecture:** Retain the filesystem watcher as the fast path. Add a
provider-declared Codex activity hint sourced from the bounded tail of
`history.jsonl`, then metadata-poll only the bounded set of rollouts named by
recent hints and submit changed paths through the existing exact-path sync
engine.

**Tech Stack:** Go, SQLite, existing parser/provider capability framework,
existing sync engine, `os.Stat`/bounded `io.Reader` operations, and
`github.com/stretchr/testify`.

## Global Constraints

- Use the isolated worktree on branch `fix/darwin-open-file-sync`.
- Do not call `codexSourceSet.FindSource` or any UUID lookup that walks the
  Codex archive.
- Do not run full provider reconciliation from the activity poller.
- Poll every 30 seconds; healthy local data must become visible within two
  minutes.
- Bootstrap at most 4 MiB from the end of each hint file and accept records no
  older than 24 hours.
- Read at most 4 MiB of new hint data per poll, retain at most a 1 MiB partial
  record, and decode at most 8,192 session IDs per poll.
- Retain at most 8,192 hot sources, 2 MiB of hot-source path bytes, and 8,192
  unresolved retries.
- Expire a hot source after 24 hours without a hint or observed file growth.
- Retry an unresolved indexed session row for two minutes, then discard it.
- Never retain, return, or log the `text` field from a Codex history record.
- Capability zero values remain unsupported; Codex is the only initial provider
  that opts in.
- Background work is `O(H + A + C)`, where `H` is configured hint files, `A` is
  hot sources, and `C` is changed hot sources. Total archive cardinality must
  not appear.
- The restart limitation is intentional: if the daemon restarts during an
  autonomous turn whose last prompt is older than 24 hours or outside the 4
  MiB bootstrap tail, freshness falls back to native watcher behavior until
  another prompt is persisted.
- When Codex sets `[history] persistence = "none"`, or a frontend does not write
  `history.jsonl`, freshness falls back to native watcher behavior.
- Derive a hint path as `<configured-session-root>/../history.jsonl`.
  Non-standard overrides may therefore produce a missing hint probe; do not
  search elsewhere.
- Go tests use testify, `t.TempDir()`, literal expected values, and observable
  effects.
- Run `go fmt ./...` and `go vet ./...` before every implementation commit.
- Invoke `kenn:commit` before every `git commit`; repository instructions
  prohibit generated-with and attribution blocks.

______________________________________________________________________

## File Map

- Modify `internal/parser/capabilities.go`: declare the activity-hint
  capability.
- Modify `internal/parser/provider.go`: define the provider-owned hint source
  and decode contract.
- Modify `internal/parser/codex_provider.go`: derive Codex hint paths and decode
  the minimal history schema.
- Modify `internal/parser/provider_capabilities_test.go`: pin capability gating
  across the registry.
- Modify `internal/parser/codex_provider_test.go`: pin path derivation,
  deduplication, validation, and prompt-data exclusion.
- Create `internal/sync/activity_hint_reader.go`: bounded append/tail reader and
  cursor lifecycle.
- Create `internal/sync/activity_hint_reader_test.go`: bootstrap, append,
  rotation, truncation, partial-line, overflow, and privacy tests.
- Create `internal/sync/live_activity.go`: bounded retry/hot-source scheduler
  and exact-path sync orchestration.
- Create `internal/sync/live_activity_test.go`: cold resume, continued open
  appends, caps, retries, expiration, errors, and cardinality tests.
- Create `internal/sync/live_activity_integration_test.go`: real Codex parser,
  SQLite, Activity, and Usage regression.
- Create `cmd/agentsview/live_activity.go`: configured-provider collection,
  indexed SQLite lookup, idle tracking, and goroutine lifecycle.
- Create `cmd/agentsview/live_activity_test.go`: daemon wiring and shutdown
  tests.
- Modify `cmd/agentsview/main.go`: start the poller after startup
  synchronization and stop it with the daemon.
- Modify `internal/sync/watcher_darwin_test.go`: stop masking the open-file
  delivery condition by closing immediately after append.
- Modify `docs/internal/session-format-sources.md`: record current producer
  evidence, persistence opt-out, and frontend scope.
- Modify `docs/configuration.md`: document hint derivation and degradation.
- Modify `docs/plans/2026-07-29-codex-open-file-freshness-design.md`: add the
  two accepted limitations and mark implementation status when complete.

______________________________________________________________________

### Task 1: Provider Activity-Hint Contract and Codex Decoder

**Files:**

- Modify: `internal/parser/capabilities.go`
- Modify: `internal/parser/provider.go`
- Modify: `internal/parser/codex_provider.go`
- Modify: `internal/parser/provider_capabilities_test.go`
- Modify: `internal/parser/codex_provider_test.go`

**Interfaces:**

- Produces:

    ```go
    type ActivityHintSource struct {
        Path string
    }

    type ActivityHint struct {
        RawSessionID string
        Timestamp    time.Time
    }

    type ActivityHintProvider interface {
        ActivityHintSources(context.Context) ([]ActivityHintSource, error)
        DecodeActivityHint([]byte) (ActivityHint, bool)
    }

    func ResolveActivityHintProvider(
        Provider,
    ) (ActivityHintProvider, bool, error)
    ```

- The sync tasks consume only this interface. They do not receive a provider
  `FindSource` function.

- [ ] **Step 1: Write failing registry and Codex provider tests**

    Add a registry-wide assertion that the zero value is unsupported, only Codex
    advertises support, and every advertising provider implements
    `ActivityHintProvider`. Add table-driven Codex tests with literal inputs:

    ```go
    func TestCodexActivityHintsUseConfiguredRootParent(t *testing.T) {
        base := t.TempDir()
        provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{
            filepath.Join(base, "sessions"),
            filepath.Join(base, "archived_sessions"),
        }})
        require.True(t, ok)

        hints := provider.(ActivityHintProvider)
        sources, err := hints.ActivityHintSources(t.Context())
        require.NoError(t, err)
        assert.Equal(t, []ActivityHintSource{{
            Path: filepath.Join(base, "history.jsonl"),
        }}, sources)
    }

    func TestCodexActivityHintDecodesIdentityWithoutPrompt(t *testing.T) {
        provider, ok := NewProvider(AgentCodex, ProviderConfig{
            Roots: []string{t.TempDir()},
        })
        require.True(t, ok)

        hint, accepted := provider.(ActivityHintProvider).DecodeActivityHint(
            []byte(`{"session_id":"019f0000-0000-7000-8000-000000000001",` +
                `"ts":1785376202,"text":"private prompt sentinel"}`),
        )

        assert.True(t, accepted)
        assert.Equal(t, ActivityHint{
            RawSessionID: "019f0000-0000-7000-8000-000000000001",
            Timestamp:    time.Unix(1785376202, 0).UTC(),
        }, hint)
    }
    ```

    Add rejected cases for an empty ID, invalid UUID, zero timestamp, malformed
    JSON, remote `s3://` roots, and duplicate configured parents.

- [ ] **Step 2: Run the focused tests and verify red**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/parser \
      -run 'TestProviderCapabilitiesActivityHints|TestCodexActivityHint' -count=1
    ```

    Expected: compilation fails because the capability and interfaces do not
    exist.

- [ ] **Step 3: Add the capability and provider contract**

    Add `ActivityHints CapabilitySupport` to `SourceCapabilities`, add
    `ProviderFeatureActivityHints`, and implement `ResolveActivityHintProvider`
    with these exact branches:

    ```go
    func ResolveActivityHintProvider(
        provider Provider,
    ) (ActivityHintProvider, bool, error) {
        if provider.Capabilities().Source.ActivityHints != CapabilitySupported {
            return nil, false, nil
        }
        hints, ok := provider.(ActivityHintProvider)
        if !ok {
            return nil, false, UnsupportedProviderFeatureError{
                Provider: provider.Definition().Type,
                Feature:  ProviderFeatureActivityHints,
            }
        }
        return hints, true, nil
    }
    ```

    Keep `ActivityHint` data-only: it must have no raw line, prompt, or generic
    payload field.

- [ ] **Step 4: Implement Codex path derivation and minimal decoding**

    Add `ActivityHints: CapabilitySupported` to `codexProviderCapabilities`.
    Implement the two methods on `codexProvider`. Path derivation cleans and
    deduplicates the parent of each local configured root. It ignores `s3://`
    roots and does not require the hint file to exist.

    Decode into a private minimal struct so unknown `text` data is discarded:

    ```go
    var row struct {
        SessionID string `json:"session_id"`
        Timestamp int64  `json:"ts"`
    }
    if json.Unmarshal(line, &row) != nil ||
        !IsValidSessionID(row.SessionID) ||
        row.Timestamp <= 0 {
        return ActivityHint{}, false
    }
    return ActivityHint{
        RawSessionID: row.SessionID,
        Timestamp:    time.Unix(row.Timestamp, 0).UTC(),
    }, true
    ```

- [ ] **Step 5: Run parser tests, formatting, and vet**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/parser -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands pass.

- [ ] **Step 6: Commit the provider contract**

    Invoke `kenn:commit`, then commit only the five parser files:

    ```bash
    git add internal/parser/capabilities.go internal/parser/provider.go \
      internal/parser/codex_provider.go \
      internal/parser/provider_capabilities_test.go \
      internal/parser/codex_provider_test.go
    git commit -m "feat(parser): expose Codex activity hints"
    ```

______________________________________________________________________

### Task 2: Bounded Hint Tail Reader

**Files:**

- Create: `internal/sync/activity_hint_reader.go`
- Create: `internal/sync/activity_hint_reader_test.go`

**Interfaces:**

- Consumes: `parser.ActivityHintProvider`, `parser.ActivityHintSource`, and
  `parser.ActivityHint`.

- Produces:

    ```go
    const (
        activityHintBootstrapBytes    = 4 << 20
        activityHintBootstrapLookback = 24 * time.Hour
        activityHintMaxReadBytes      = 4 << 20
        activityHintMaxLineBytes      = 1 << 20
        activityHintMaxIDsPerPoll     = 8192
    )

    type activityHintCursor struct {
        info       os.FileInfo
        offset     int64
        partial    []byte
        initialized bool
        droppingLine bool
    }

    type activityHintReadResult struct {
        Hints     []parser.ActivityHint
        BytesRead int
        Overflow  bool
    }

    func readActivityHints(
        ctx context.Context,
        source parser.ActivityHintSource,
        decoder parser.ActivityHintProvider,
        cursor *activityHintCursor,
        now time.Time,
    ) (activityHintReadResult, error)
    ```

- [ ] **Step 1: Write failing behavioral tests**

    Use real files in `t.TempDir()` and a decoder spy with literal records. Cover:

    - initial tail accepts a record 23 hours old and rejects one 25 hours old;
    - initial read starts no earlier than the final 4 MiB;
    - an incomplete final line is retained and decoded after the next append;
    - a line over 1 MiB is discarded through its newline and the following valid
      record is decoded;
    - an append over 4 MiB sets `Overflow` and prioritizes the newest bounded
      tail;
    - replacement, truncation, and `os.SameFile` identity changes reset the
      cursor;
    - a missing hint file returns no hints and becomes readable after it is
      created;
    - duplicate IDs are emitted once per read;
    - cancellation aborts before another read;
    - errors contain the hint path but never the private prompt sentinel.

    Assertions must check decoded IDs, timestamps, exact `BytesRead` bounds,
    cursor offsets, and error strings. Do not assert private field names or
    implementation source text.

- [ ] **Step 2: Run the reader tests and verify red**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^TestReadActivityHints' -count=1
    ```

    Expected: compilation fails because `readActivityHints` is undefined.

- [ ] **Step 3: Implement bootstrap and incremental reading**

    Use `os.Stat`, `os.Open`, `Seek`, and `io.LimitReader`; never `ReadFile`. On
    bootstrap, choose:

    ```go
    start := max(int64(0), info.Size()-activityHintBootstrapBytes)
    ```

    When `start > 0`, discard through the first newline before decoding so a
    partial JSON suffix cannot be accepted. Apply the literal cutoff:

    ```go
    cutoff := now.Add(-activityHintBootstrapLookback)
    if hint.Timestamp.Before(cutoff) || hint.Timestamp.After(now.Add(time.Minute)) {
        continue
    }
    ```

    On incremental overflow, move to the newest 4 MiB, discard the first partial
    line, set `Overflow`, and advance the cursor to the bytes actually consumed.
    Preserve a bounded final partial line; once it exceeds 1 MiB, set
    `droppingLine` and discard bytes through the next newline.

- [ ] **Step 4: Run focused and package tests**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^TestReadActivityHints' -count=1
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync -short -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands pass.

- [ ] **Step 5: Commit the bounded reader**

    Invoke `kenn:commit`, then:

    ```bash
    git add internal/sync/activity_hint_reader.go \
      internal/sync/activity_hint_reader_test.go
    git commit -m "feat(sync): read activity hints with bounded cursors"
    ```

______________________________________________________________________

### Task 3: Hot-Source Poller and Cardinality Bounds

**Files:**

- Create: `internal/sync/live_activity.go`
- Create: `internal/sync/live_activity_test.go`

**Interfaces:**

- Consumes: Task 1 provider interfaces and Task 2 `readActivityHints`.

- Produces:

    ```go
    const (
        liveActivityPollInterval = 30 * time.Second
        liveActivityHotTTL       = 24 * time.Hour
        liveActivityRetryTTL     = 2 * time.Minute
        liveActivityLogInterval  = 5 * time.Minute
        liveActivityMaxEntries   = 8192
        liveActivityMaxPathBytes = 2 << 20
    )

    type LiveActivitySource struct {
        Path           string
        StoredSize     int64
        StoredMTimeNS  int64
        HasStoredStat  bool
    }

    type LiveActivityLookup func(
        context.Context, string,
    ) (LiveActivitySource, bool, error)

    type LiveActivitySync func(context.Context, []string) error

    type LiveActivityTarget struct {
        Provider parser.Provider
        Hints    parser.ActivityHintProvider
        Sources  []parser.ActivityHintSource
    }

    type LiveActivityPollStats struct {
        HintFiles      int
        HintBytes      int
        SessionLookups int
        SourceStats    int
        SyncPaths      int
    }

    func NewLiveActivityPoller(
        targets []LiveActivityTarget,
        lookup LiveActivityLookup,
        syncPaths LiveActivitySync,
        logf func(string, ...any),
    ) *LiveActivityPoller

    func (p *LiveActivityPoller) PollOnce(
        ctx context.Context, now time.Time,
    ) (LiveActivityPollStats, error)

    func (p *LiveActivityPoller) Run(ctx context.Context)
    ```

- [ ] **Step 1: Write failing cold-resume and ongoing-append tests**

    Build targets with a real temporary hint file, a fake provider that decodes
    literal records, a lookup spy keyed by exact full session ID, and a sync spy
    that rejects any path other than the expected rollout.

    Assert this sequence:

    1. A history record for raw ID `cold-id` causes lookup of `codex:cold-id`.
    1. A rollout whose stat differs from stored SQLite metadata is submitted
       immediately.
    1. A second unchanged poll submits nothing.
    1. Appending to the still-open rollout causes the next poll to submit exactly
       that path even with no second history record.
    1. A failed sync leaves the successful-stat baseline unchanged so the next
       poll retries.

    These tests catch missing cold-resume activation, wrong ID prefixing, sync
    suppression, and accidental baseline acknowledgement on failure.

- [ ] **Step 2: Write failing bounds and lifecycle tests**

    Add literal tests for:

    - 8,193 hint IDs retain no more than 8,192 hot/retry entries;
    - path-byte overflow evicts least-recently-hinted quiescent entries first;
    - retries happen for two minutes and never invoke provider `FindSource`;
    - a missing hot path is removed without a tombstone or reconciliation call;
    - 24-hour expiration removes quiescent entries, while observed growth
      refreshes retention;
    - a later hint re-queries the indexed row and moves a hot entry to a newly
      selected canonical path;
    - duplicate hints and overlapping hint sources produce one lookup/sync;
    - cancellation stops `Run` without another tick;
    - repeated overflow/errors are logged at most once per five minutes, and logs
      contain counts and paths but not record contents.

- [ ] **Step 3: Write the archive-cardinality regression**

    Run one case with 10 unrelated rollout files and one with 20,000 unrelated
    rollout files. Both cases contain the same one hint and one changed hot
    rollout. Assert identical literal stats:

    ```go
    assert.Equal(t, LiveActivityPollStats{
        HintFiles:      1,
        SessionLookups: 1,
        SourceStats:    1,
        SyncPaths:      1,
    }, withoutByteCount(small))
    assert.Equal(t, withoutByteCount(small), withoutByteCount(large))
    assert.Zero(t, provider.findSourceCalls)
    ```

    The sync spy must receive exactly one path in both cases. The large archive is
    real filesystem state, not a generated expectation, so an accidental archive
    walk materially slows or fails the test and a `FindSource` fallback fails
    the explicit spy assertion.

- [ ] **Step 4: Run the poller tests and verify red**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^TestLiveActivity' -count=1
    ```

    Expected: compilation fails because the poller types do not exist.

- [ ] **Step 5: Implement retry and hot-source state**

    Key hot entries by canonical full session ID, but deduplicate submitted work
    by cleaned source path. A newly resolved row begins with the stored
    size/mtime baseline. Compare that baseline with `os.Stat`; acknowledge the
    observed stat only after `syncPaths` succeeds.

    Do not expose any provider source-resolution method inside the poller. Missing
    indexed rows enter the bounded retry map with the original target and
    first-seen time.

- [ ] **Step 6: Implement deterministic eviction and polling**

    Sort candidates by last hint/growth time and then full session ID before
    eviction. Track path bytes on insertion, path change, removal, and
    expiration. `PollOnce` returns work counters for diagnostics and cardinality
    tests, not internal map sizes.

    `Run` uses a single `time.Ticker` at `liveActivityPollInterval`, performs one
    startup `PollOnce`, and exits on `ctx.Done()`. Log aggregate errors and
    overflow metadata only.

- [ ] **Step 7: Run focused tests, short sync tests, formatting, and vet**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run '^(TestReadActivityHints|TestLiveActivity)' -count=1
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync -short -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands pass.

- [ ] **Step 8: Commit the poller**

    Invoke `kenn:commit`, then:

    ```bash
    git add internal/sync/live_activity.go internal/sync/live_activity_test.go
    git commit -m "feat(sync): poll bounded hot session sources"
    ```

______________________________________________________________________

### Task 4: Daemon Wiring and Indexed Path Lookup

**Files:**

- Create: `cmd/agentsview/live_activity.go`
- Create: `cmd/agentsview/live_activity_test.go`
- Modify: `cmd/agentsview/main.go`

**Interfaces:**

- Consumes: `sync.NewLiveActivityPoller`, `parser.ResolveActivityHintProvider`,
  `db.GetSessionFull`, `sync.Engine.SyncPathsContext`, and
  `server.IdleTracker`.

- Produces:

    ```go
    func collectLiveActivityTargets(
        ctx context.Context,
        cfg config.Config,
    ) ([]sync.LiveActivityTarget, error)

    func startLiveActivityPoller(
        ctx context.Context,
        cfg config.Config,
        database *db.DB,
        engine *sync.Engine,
        idleTracker *server.IdleTracker,
    ) func()
    ```

- [ ] **Step 1: Write failing target-collection tests**

    Construct config maps with Codex, an unsupported provider, duplicated
    `sessions`/`archived_sessions` parents, an `s3://` root, and a non-standard
    override. Assert:

    - only capability-supported local providers produce targets;
    - standard roots deduplicate to one parent `history.jsonl`;
    - an override `/data/custom/sessions` probes `/data/custom/history.jsonl`
      exactly;
    - collection does not stat or discover rollout sources.

- [ ] **Step 2: Write failing indexed-lookup and lifecycle tests**

    Use `testDB(t)` with an exact `codex:<uuid>` row and file metadata. Verify the
    lookup returns `file_path`, stored size, and stored mtime from
    `GetSessionFull`. Verify a missing row returns `found=false` without calling
    a provider.

    Start the poller with a cancellable context and a sync spy. Assert the
    returned stop function cancels and waits for the goroutine. Assert each sync
    pass is bracketed by `IdleTracker.BeginWork`, and shutdown does not start
    new work.

- [ ] **Step 3: Run the focused command tests and verify red**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
      -run '^TestCollectLiveActivity|^TestStartLiveActivity' -count=1
    ```

    Expected: compilation fails because the wiring functions do not exist.

- [ ] **Step 4: Implement collection and the exact indexed lookup**

    Iterate configured provider factories in registry order. Construct providers
    with only their configured roots and local machine. Resolve the hint
    capability, call `ActivityHintSources`, and skip unsupported or empty
    providers. Return healthy targets together with any joined provider errors;
    startup logs those errors and still runs the healthy targets.

    Implement the lookup with `database.GetSessionFull(ctx, fullID)`. Return no
    source when the row or `FilePath` is absent. When size or mtime is absent,
    return the path with `HasStoredStat=false` so the first hot poll schedules
    an exact-path sync instead of losing an older incomplete baseline. Never
    call `provider.FindSource`.

- [ ] **Step 5: Wire poller startup after initial synchronization**

    In the existing `!cfg.NoSync` block, start the activity poller after the
    initial sync/resync/worker gap reconciliation has been scheduled or
    completed, and before `startPeriodicSync`. Use a child context and wait
    group so the returned stop function cancels and joins it.

    The first poll performs the bounded 24-hour/4 MiB bootstrap. This ordering
    intentionally handles writes during startup without needing an unbounded
    history replay.

- [ ] **Step 6: Run command tests, formatting, and vet**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview \
      -run 'LiveActivity' -count=1
    CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview -short -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: all commands pass.

- [ ] **Step 7: Commit daemon wiring**

    Invoke `kenn:commit`, then:

    ```bash
    git add cmd/agentsview/live_activity.go \
      cmd/agentsview/live_activity_test.go cmd/agentsview/main.go
    git commit -m "feat(cli): keep live Codex sessions synchronized"
    ```

______________________________________________________________________

### Task 5: End-to-End Activity and Usage Regression

**Files:**

- Create: `internal/sync/live_activity_integration_test.go`
- Modify: `internal/sync/watcher_darwin_test.go`

**Interfaces:**

- Consumes the real Codex provider, sync engine, SQLite archive,
  `db.GetActivityReport`, and `db.GetDailyUsage`.

- Produces no new production interface.

- [ ] **Step 1: Write the real cold-resume integration test**

    Create a dated Codex rollout under `t.TempDir()` with `internal/testjsonl`
    helpers:

    ```go
    initial := testjsonl.JoinJSONL(
        testjsonl.CodexSessionMetaJSON(uuid, "/workspace", "user", firstTS),
        testjsonl.CodexTurnContextJSON("gpt-5.4", firstTS),
        testjsonl.CodexMsgJSON("user", "first", firstTS),
        testjsonl.CodexMsgJSON("assistant", "answer", firstTS),
        testjsonl.CodexTokenCountJSON(firstTS, 1000, 100, 400),
    )
    ```

    Perform the initial exact-path sync, then open the rollout once with
    `os.O_APPEND|os.O_WRONLY` and keep that descriptor open through all
    assertions. Append a current history record and a second user/assistant turn
    with a literal token count.

    Run `PollOnce` at the fixed current time. Assert:

    - `GetSessionFull` file size, file mtime, message count, and `EndedAt`
      advance;
    - `GetDailyUsage(...).Totals.OutputTokens` grows from 100 to the literal
      combined total;
    - `GetActivityReport` contains the canonical session ID in an interval
      overlapping the second turn's five-minute bucket; and
    - the rollout descriptor remains open until after every database assertion.

    This test must fail if activity hints are ignored, indexed lookup is wrong,
    open-file growth is not polled, sync is acknowledged before success, or
    Activity/Usage bypass the refreshed archive.

- [ ] **Step 2: Add the monotonic append sequence**

    Append a third assistant/token event through the same open descriptor, run a
    second poll, and assert the new daily output-token total is strictly greater
    than the second-turn total. Use literal expected totals rather than deriving
    them with parser normalization logic.

- [ ] **Step 3: Correct the Darwin watcher characterization**

    In the existing recursive-watch append case, write while the descriptor is
    open and keep it open for a bounded observation window before closing.
    Record whether an FSEvents batch arrived before close; after close, require
    that the changed path has been observed either before or after close.

    Do not require FSEvents to suppress the event: that is upstream,
    version-dependent behavior. The production regression is the integration
    test above, which proves Agentsview refreshes before close regardless of
    native delivery.

- [ ] **Step 4: Run focused integration and Darwin tests**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run 'TestLiveActivityPollerRefreshesColdResumedOpenCodexRollout' -count=1
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run 'TestDarwin.*Recursive' -count=1
    ```

    Expected: both pass on macOS; the Darwin test is excluded by build tags on
    other platforms.

- [ ] **Step 5: Run formatting, vet, and commit the regressions**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    ```

    Invoke `kenn:commit`, then:

    ```bash
    git add internal/sync/live_activity_integration_test.go \
      internal/sync/watcher_darwin_test.go
    git commit -m "test(sync): cover open Codex rollout freshness"
    ```

______________________________________________________________________

### Task 6: Provenance and Documented Degradation

**Files:**

- Modify: `docs/internal/session-format-sources.md`
- Modify: `docs/configuration.md`
- Modify: `docs/plans/2026-07-29-codex-open-file-freshness-design.md`

**Interfaces:**

- Documents the shipped behavior; no code consumers.

- [ ] **Step 1: Update the Codex provenance entry**

    Reverify against OpenAI Codex commit
    `406dc9239492aff6d295cca5eebe2a548548d42f` and link directly to:

    - `codex-rs/message-history/src/lib.rs` for the `session_id`/`ts`/`text`
      schema, append-only file, and `HistoryPersistence::None` behavior;
    - `codex-rs/tui/src/app/thread_routing.rs` and TUI input submission for the
      submitted-prompt append call; and
    - `codex-rs/core/config.schema.json` for `save-all` versus `none`.

    State the verified frontend boundary precisely: the pinned TUI submits
    message-history entries; no `append_entry` producer call exists under the
    pinned `app-server` or `exec` trees. Do not claim IDE/desktop coverage
    beyond producer evidence. Record that locally observed Codex app builds may
    write the same schema, but this remains observational rather than a public
    compatibility guarantee.

- [ ] **Step 2: Document operational limitations**

    In `docs/configuration.md`, explain:

    - activity hints are a freshness backstop, not a session source;
    - `[history] persistence = "none"` and frontends without history writes retain
      native-watcher freshness;
    - custom session roots probe their cleaned parent for `history.jsonl` and do
      not trigger a search; and
    - after restart, a turn whose last prompt is outside the 24-hour or 4 MiB
      bootstrap window is not hot until another persisted prompt.

    Add the same two limitations under a `Known Limitations` section in the design
    spec and change its status to `implemented` only after Tasks 1–5 pass.

- [ ] **Step 3: Format and inspect documentation**

    Run:

    ```bash
    mdformat --wrap 80 docs/internal/session-format-sources.md \
      docs/configuration.md \
      docs/plans/2026-07-29-codex-open-file-freshness-design.md
    git diff --check
    ```

    Expected: both commands pass and the diff contains no private paths, session
    IDs, prompt text, or personal identifiers.

- [ ] **Step 4: Commit documentation**

    Invoke `kenn:commit`, then:

    ```bash
    git add docs/internal/session-format-sources.md docs/configuration.md \
      docs/plans/2026-07-29-codex-open-file-freshness-design.md
    git commit -m "docs: record Codex live-sync boundaries"
    ```

______________________________________________________________________

### Task 7: Full Verification and Branch Handoff

**Files:**

- No expected file changes.

**Interfaces:**

- Verifies the complete feature branch.

- [ ] **Step 1: Run focused packages**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/sync \
      ./cmd/agentsview -count=1
    ```

    Expected: all packages pass.

- [ ] **Step 2: Run repository-required checks**

    Run:

    ```bash
    go fmt ./...
    go vet ./...
    make test-short
    ```

    Expected: all commands pass and `git status --short` remains empty.

- [ ] **Step 3: Inspect bounded-work evidence**

    Re-run the cardinality and real open-rollout tests verbosely:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run 'TestLiveActivity.*Cardinality|TestLiveActivityPollerRefreshesColdResumedOpenCodexRollout' \
      -count=1 -v
    ```

    Expected: both small and large archive cases report identical work counts, and
    the open-rollout test advances session metadata, Activity, and Usage before
    the descriptor closes.

- [ ] **Step 4: Run the private-data scrub**

    Inspect the complete branch diff:

    ```bash
    git diff main...HEAD --check
    git diff main...HEAD -- \
      docs internal/parser internal/sync cmd/agentsview
    ```

    Expected: no private absolute paths, real session IDs, prompt content,
    credentials, hostnames, attribution blocks, or unrelated changes.

- [ ] **Step 5: Use the branch-finishing workflow**

    Invoke `superpowers:verification-before-completion`, then
    `superpowers:finishing-a-development-branch`. Follow repository policy:
    delivery is through a feature-branch pull request, but do not push or open
    the PR without explicit user authorization, and never merge it.
