# Codex Cloud Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import every thread on OpenAI's hosted Codex app-server into
AgentsView as `codex-cloud` sessions, with live token counts for threads it sees
running and per-thread plan usage from `query_v2`.

**Architecture:** A fetcher (poller job) mirrors threads into per-thread
zstd-framed JSONL files under `{dataDir}/sources/codex-cloud/<accountKey>/`. A
new file-based provider parses those files through the normal sync engine. A
live token monitor (second poller job) joins running threads with
`thread/resume` and appends token records to the same files. A plan usage step
appends `query_v2` rows and outcome records. A new per-session table,
`session_plan_usage`, carries plan usage and capture coverage to every backend.

**Tech Stack:** Go (stdlib `encoding/json/v2`, `github.com/coder/websocket`,
`github.com/klauspost/compress/zstd`), SQLite, PostgreSQL/CockroachDB,
ClickHouse, DuckDB, Svelte 5.

**Spec:** `docs/superpowers/specs/2026-10-11-codex-cloud-source-design.md`.
Every task implements the spec section it names. When this plan and the spec
disagree, the spec wins; fix the plan.

## Global Constraints

- Agent ID `codex-cloud`; session ID `codex-cloud:<threadId>`; ID prefix
  `codex-cloud:`.
- Mirror root `{dataDir}/sources/codex-cloud`; files
  `<accountKey>/<threadId>.jsonl.zstd`; state files `*.state.json`,
  `*.descendants.json`, `*.waiting.json`, `account.json`.
- zstd only, every frame with a content checksum, no header frame.
- Websocket methods allowed: fetcher `initialize`, `initialized`, `thread/list`,
  `thread/items/list`, `thread/turns/list`; monitor adds `thread/resume` with
  exactly `threadId` and `excludeTurns: true`.
- Subprotocols in order: `codex-app-server`, `codex-client.agentsview`,
  `openai-bearer.<token>`. Headers: `X-OpenAI-Product-Sku: codex`,
  `X-OpenAI-Allow-Environmentless-Fallback: true`, `originator: agentsview`,
  `ChatGPT-Account-Id` when present, `User-Agent: agentsview/<version>`.
- Never answer a server-initiated request. Never refresh a token. Never follow a
  redirect. Never send `experimentalApi`.
- Endpoint defaults: `wss://codex-cloud-backend.chatgpt.com/` and
  `https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2`.
  Overrides: host `chatgpt.com`, `openai.com`, or a subdomain; `ws`/`http`
  only for loopback.
- Intervals: sync 5 minutes, live discovery 30 seconds, both with jitter.
- `NoSync` is never in `config.toml` (`toml:"-"`). Both jobs take it once from
  the daemon's startup config; only file-backed gates are re-read per run.
- Record body types that both the fetcher and the parser use live in
  `internal/codexcloud/mirror`. The parser must not import
  `internal/codexcloud`, which imports `config`, which imports `parser`.
- Numbers from the spec: save-lag margin 10 minutes (12 against server
  timestamps), clock allowance 2 minutes, stall rule 24 hours, retry cap 1
  hour, join cap 10 per account, join leave 6 hours, token buffer flush once a
  minute, plan usage budget 5 requests per pass, 100 groups and 1,000 thread
  IDs per request, settle after 7 days, usage backoff 5 minutes to 6 hours.
- The token never appears in logs, errors, files, the archive, fixtures, or
  status output. Fixtures are synthetic: no private names, paths, IDs, tokens.
- Tests use fakes only. Only the user runs anything against a real ChatGPT
  login. Use scratch data directories (`AGENTSVIEW_DATA_DIR`), never the live
  archive.
- After Go changes run `go fmt ./...` and `go vet ./...`. Tests use
  `CGO_ENABLED=1 go test -tags fts5`.
- Frontend: follow `frontend/AGENTS.md` and `DESIGN.md`; every string through
  `m.*()` in all locales.

## Review Focus

- A thread whose first fetch fails must still import with its metadata (the
  retry entry carries the listing record). Test in Task 14.
- A stuck `inProgress` turn must cause at most one cold resume, and a turn the
  monitor watched finish must not be rejoined. Test in Task 17.
- A token value must never reach a written file, log line, or status output,
  across sync, plan usage, joins, refresh requests, and 401/403/5xx. Canary
  test in Task 15.
- A page whose `updatedAt` increases row to row must fail the pass, never stop
  early. Test in Task 12.
- A reply larger than 32 KiB must decode (Task 9); a reply over the read limit
  must fail only that thread after one `limit: 1` retry on a fresh connection
  (Task 14).

______________________________________________________________________

## Gate before any run against the real host

The fakes cover everything up to here, but four values are unverified on the
hosted server: the turn view (`itemsView: notLoaded`), the listing order and
`modelProviders: []`, the `codex-client.agentsview` subprotocol, and which
listing returns sub-agent threads. Before the first run against a real login
(the milestone after Task 15), prepare a typed, read-only Python probe outside
the repository (`/tmp`, uv script, never committed) that answers Open questions
1, 2, 3, and 7 of the spec, and ask the user to run it. Record each answer in
`docs/internal/session-format-sources.md` (Task 29 owns the entry) and adjust
the constants in Tasks 9, 12, 13, and 17 to match. The gate probe is ready at
`/tmp/dot-probe/gate_probe.py`.

______________________________________________________________________

## Phase 1: Foundations

### Task 1: `poller.ErrTriggerPending`

**Files:**

- Modify: `internal/poller/poller.go:96-107`
- Test: `internal/poller/poller_test.go`

**Interfaces:**

- Produces:
  `var ErrTriggerPending = errors.New("poller: trigger already pending")`;
  `TriggerNow` returns `fmt.Errorf("%w: %q", ErrTriggerPending, name)` when a
  trigger is queued.

- [x] **Step 1: Write the failing test**

```go
func TestTriggerNowPendingIsSentinel(t *testing.T) {
	// Block the job inside Run, queue one trigger, then a second one.
	// require.ErrorIs(t, second, poller.ErrTriggerPending)
	// require.Contains(t, second.Error(), "my-job")
}
```

- [x] **Step 2: Run it and see it fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/poller -run TestTriggerNowPendingIsSentinel`
Expected: FAIL (`ErrTriggerPending` undefined)

- [x] **Step 3: Add the sentinel and wrap it in `TriggerNow`**

- [x] **Step 4: Run `go test ./internal/poller` and see it pass**

- [x] **Step 5: Commit**

```bash
git commit -am "feat(poller): export ErrTriggerPending"
```

### Task 2: Shared zstd frame helpers

The DeepSeek harness reader already inspects frames and builds a bounded
decoder. Move those helpers to a package both readers can use, without changing
DeepSeek behavior.

**Files:**

- Create: `internal/zstdframe/zstdframe.go`,
  `internal/zstdframe/zstdframe_test.go`
- Modify: `internal/parser/deepseek_harness_format.go` (call the new package)

**Interfaces:**

- Produces:

    - `const MaxWindow = 8 << 20`, `const DecoderMemory = 64 << 20`
    - `type Layout struct { CompleteFrames int; FrameEnds []int64; Size int64; Torn bool }`
    - `func Inspect(r io.ReaderAt, size, from int64) (Layout, error)` inspects
      frames starting at offset `from`; `FrameEnds` lists the end offset of each
      complete frame; `Torn` reports an incomplete trailing frame.
    - `func NewDecoder(r io.Reader) (*zstd.Decoder, error)` with concurrency 1,
      low memory, `DecoderMemory`, `MaxWindow`.
    - `func EncodeFrame(src []byte) ([]byte, error)`: one frame with a content
      checksum.
    - `type FormatError struct { Offset int64; Reason string; Unsupported bool }`
      for bytes that are not a supported frame.

- [x] **Step 1: Write failing tests** `TestInspectCompleteFrames` (two frames:
  `CompleteFrames == 2`, `FrameEnds` match the written sizes,
  `Torn == false`), `TestInspectTornTail` (append half a frame:
  `Torn == true`, `CompleteFrames == 2`), `TestInspectFromOffset` (from the
  first frame's end: one frame).

- [x] **Step 2: Run** `go test ./internal/zstdframe`; expect FAIL.

- [x] **Step 3: Implement** by moving `inspectDeepSeekHarnessZstdFrames` and
  `newDeepSeekHarnessZstdDecoder` logic; DeepSeek keeps its header-frame check
  and line reader and calls `zstdframe.Inspect(f, 0)`.

- [x] **Step 4: Run**
  `go test ./internal/zstdframe ./internal/parser -run DeepSeek`; expect PASS.

- [x] **Step 5: Commit** `git commit -m "refactor: share zstd frame inspection"`

### Task 3: Mirror record format, writer, and reader

Spec: Mirror format.

**Files:**

- Create: `internal/codexcloud/mirror/record.go`, `writer.go`, `reader.go`,
  `mirror_test.go`

**Interfaces:**

- Produces:

    - `type Kind string` with `KindThread = "thread"`, `KindTurn = "turn"`,
      `KindItem = "item"`, `KindPlanUsageRow = "plan_usage_row"`,
      `KindPlanUsageOutcome = "plan_usage_outcome"`,
      `KindTokenUsage = "token_usage"`, `KindTurnCompleted = "turn_completed"`.
    - `type Record struct { Kind Kind \`json:"kind"\`; FetchedAt time.Time
      \`json:"fetched_at"\`; Data jsontext.Value \`json:"data"\` }\`
    - `type AppendResult struct { Offset int64; Recovered []Record; Truncated bool }`
    - `func Append(path string, storedOffset int64, records []Record) (AppendResult, error)`:
      inspects bytes after `storedOffset` (full scan when `storedOffset` is 0 or
      past EOF); keeps complete frames there and returns their records in
      `Recovered`; truncates a torn tail; writes one frame of JSONL; fsyncs the
      file, and the directory when the file is new. `records` may be empty
      (recovery only).
    - A torn walk from `storedOffset` is confirmed by a walk from the start before
      truncating, because an offset inside a frame looks torn.
    - `func ReadAll(ctx context.Context, path string) ([]Record, error)`: complete
      frames only; a torn tail contributes nothing.

- [x] **Step 1: Write failing tests**

    - `TestAppendThenReadAll`: two appends; `ReadAll` returns all records in
      order.
    - `TestAppendRecoversFramesPastStaleOffset`: append A, append B, call
      `Append(path, offsetAfterA, C)`; `Recovered` holds B's records; file has
      A, B, C.
    - `TestAppendTruncatesTornTail`: write half a frame after A; next append sets
      `Truncated`; `ReadAll` returns A then the new records.
    - `TestReadAllIgnoresTornTail`.
    - `TestAppendScansWhenOffsetPastEOF`.

- [x] **Step 2: Run** `go test ./internal/codexcloud/mirror`; FAIL.

- [x] **Step 3: Implement** with `zstdframe.Inspect` and `zstdframe.NewEncoder`.

- [x] **Step 4: Run**; PASS.

- [x] **Step 5: Commit**
  `git commit -m "feat(codexcloud): add zstd mirror format"`

### Task 4: Register the `codex-cloud` agent

Spec: Components (Provider), Configuration (`disabled_agents`, root, settings
list exclusion), Remote host sync flag.

**Files:**

- Modify: `internal/parser/types.go` (const, `AgentDef` fields, `Registry`,
  `RemoteSyncExcludedAgent`)
- Modify: `internal/config/config.go` (`finalize`: data-dir roots into
  `AgentDirs`; `resolveSessionSources`: reject `codex-cloud`).
  `internal/config/agent_directories.go` already skips the table (Task 5).
- Modify: `internal/remotesync/resolve.go:34,506` (use the helper)
- Modify: the settings provider list (exclude `codex-cloud`); find it with
  `grep -rn FileBased internal/server internal/service`
- Modify: `frontend/src/lib/utils/agents.ts`, `agents.test.ts`
- Test: `internal/parser/types_test.go`, `internal/config/config_test.go`,
  `internal/remotesync/resolve_test.go`, `internal/rawsync/manifest_test.go`

**Interfaces:**

- Produces:

    - `const AgentCodexCloud AgentType = "codex-cloud"`
    - `AgentDef.DataDirRoot string`: a root relative to the data directory
      (`"sources/codex-cloud"`). The engine reads `cfg.AgentDirs`, not
      `ResolveDirs`, so `finalize` (`internal/config/config.go`) sets
      `cfg.AgentDirs[def.Type] = []string{filepath.Join(cfg.DataDir, def.DataDirRoot)}`
      for every such entry, after `expandLocalPaths` and before
      `resolveSessionSources`. It overwrites and leaves `agentDirSource` at the
      default, so `IsUserConfigured` stays false. `Default()` is too early:
      `AGENTSVIEW_DATA_DIR` is applied later, in `loadEnv`.
    - `resolveSessionSources` rejects `[[session_sources]] agent = "codex-cloud"`:
      its root comes only from the data directory.
    - `AgentDef.RemoteHostSyncExcluded bool`; `RemoteSyncExcludedAgent` returns
      true when it or `RemoteSyncExcluded` is set. Raw ingest keeps reading only
      `RemoteSyncExcluded`.
    - Registry entry: `DisplayName "Codex Cloud"`, `IDPrefix "codex-cloud:"`,
      `FileBased true`, `DataDirRoot`, `RemoteHostSyncExcluded true`, no
      `EnvVar`, no `DefaultDirs`, no usage-capability flags.

- [ ] **Step 1: Write failing tests**

    - `TestCodexCloudRootUnderDataDir`: with `AGENTSVIEW_DATA_DIR=<tmp>`,
      `cfg.AgentDirs[AgentCodexCloud]` and `ResolveDirs(AgentCodexCloud)` both
      equal `[]string{<tmp>/sources/codex-cloud}` for `LoadMinimal`,
      `LoadReadOnly`, and `Load`, and `IsUserConfigured` is false.
    - `TestSessionSourcesRejectCodexCloud`.
    - `TestConfigLoadsWithCodexCloudTable`: a `config.toml` with
      `[agents.codex-cloud]\nenabled = true\ndirs = ["/x"]` loads without error
      and does not change `ResolveDirs`.
    - `TestDisabledAgentsAcceptsCodexCloud`.
    - `TestRemoteSyncExcludesCodexCloud`: `RemoteSyncExcludedAgent` is true,
      `ResolveTargets` forbids the root, `resolveAgentHasOnDiskSource` false.
    - `TestRawSyncAcceptsCodexCloud`: `validateProvider` accepts it.
    - frontend `agents.test.ts`: `KNOWN_AGENTS` list includes `codex-cloud` with
      label "Codex Cloud".

- [ ] **Step 2: Run** the four Go packages and `vp test agents`; FAIL.

- [ ] **Step 3: Implement.** Add the factory case in `providerFactoryForDef`
  pointing at a stub provider that discovers nothing (Task 6 fills it) and a
  `provider_migration.go` entry `ProviderMigrationProviderAuthoritative`.

- [ ] **Step 4: Run** again; PASS. Run `go vet ./...`.

- [ ] **Step 5: Commit** `git commit -m "feat: register codex-cloud agent"`

### Task 5: `[agents.codex-cloud]` settings

Spec: Configuration.

**Files:**

- Create: `internal/config/codex_cloud.go`,
  `internal/config/codex_cloud_test.go`
- Modify: `internal/config/config.go` (`applyConfigTOML` calls the parser;
  `Config` gains the field)

**Interfaces:**

- Produces:

    - `type CodexCloudConfig struct { Present bool; Enabled bool; Endpoint *url.URL; UsageEndpoint *url.URL; LiveTokens bool; Err *CodexCloudConfigError }`
    - `type CodexCloudConfigError struct { Path, Key, Reason string; Unknown bool }`
      with `Error()` rendering the two spec messages exactly.
    - `Config.CodexCloud CodexCloudConfig`
    - `func parseCodexCloudTable(cfgPath string, raw any) CodexCloudConfig` never
      returns an error to `applyConfigTOML`; problems land in `Err`.
    - Defaults: `Enabled false`, `LiveTokens true`, endpoints from Global
      Constraints.

- [x] **Step 1: Write failing tests** (table-driven `TestParseCodexCloudTable`):
  defaults; `enabled = true`; unknown key `dirs` yields `Err.Unknown` and the
  other agents still load; non-loopback `ws://` rejected; `wss://evil.example`
  rejected; `wss://127.0.0.1:9/` accepted; `https://api.openai.com/x`
  accepted; non-bool `live_tokens` rejected with `Key == "live_tokens"`.
  `TestCodexCloudErrorKeepsOtherAgents`.

- [x] **Step 2: Run** `go test ./internal/config -run CodexCloud`; FAIL.

- [x] **Step 3: Implement.**

- [x] **Step 4: Run**; PASS.

- [x] **Step 5: Commit**
  `git commit -m "feat(config): parse codex-cloud settings"`

## Phase 2: Parsing mirror files

### Task 6: Parse threads, turns, and items into sessions

Spec: Mapping to AgentsView sessions, Incremental parsing.

**Files:**

- Create: `internal/parser/codex_cloud.go`, `codex_cloud_items.go`,
  `codex_cloud_provider.go`, `codex_cloud_test.go`,
  `internal/parser/testdata/codex_cloud/` (synthetic `.jsonl.zstd` fixtures
  written by a test helper from JSON, not hand-made binaries)

**Interfaces:**

- Consumes: `mirror.ReadAll`, `mirror.Record`, `mirror.Kind*`.
- Produces in `internal/codexcloud/mirror` (`server.go`):
  `KindThreadResume = "thread_resume"` for the monitor's first metadata
  record, so listing records stay distinguishable; minimal decode types for
  server JSON used by both the fetcher and the parser: `Thread`,
  `Turn{ID, Status string; StartedAt, CompletedAt *int64}`,
  `Item{ID, Type, Status string; CompletedAtMs *int64}` plus
  `ItemRecord{TurnID string; Item jsontext.Value}` as the `item` record body.
- Produces:
    - `func ParseCodexCloudFile(ctx context.Context, path, machine string) (ParseResult, error)`
    - provider factory
      `newCodexCloudProviderFactory(def AgentDef) ProviderFactory` built on
      `NewJSONLSourceSet` with `WithRecursive`, `WithExtensions(".zstd")` (the
      default extension list is `.jsonl`, and `filepath.Ext` of a mirror file is
      `.zstd`), an include-path filter for `*.jsonl.zstd`,
      `WithSessionIDFromPath` (thread ID = file name without `.jsonl.zstd`),
      `WithParseFile`, `WithForceReplace`.
    - Content capabilities: `AggregateUsageEvents` supported.

Rules the tests pin (from the spec):

- newest record per `(turnId, item.id)` wins; newest thread record wins;

- title `name` else `preview`; times and cwd from `thread` (listing) records
  only, never from `thread_resume` records; a file with only a monitor
  metadata record ends at the newest `completedAtMs` or token record time;

- model and reasoning effort on every assistant and thinking message;

- `Entrypoint`: `dot` for `codex_work_cca` or `threadSource == "aeon_child"`,
  `codex-web` for `codex_work_web`, `codex-cloud` for `codex_cloud`, else the
  raw originator; `SessionKind` empty;

- cwd: first `environments[].cwd` of the newest listing record with non-empty
  `environments`, else `cwd`;

- resolved parent: `parentThreadId`, else
  `source.subAgent.thread_spawn.parent_thread_id`; `RelSubagent` wins over
  `forkedFromId` (`RelFork`);

- a file with no metadata record is skipped and logged.

- [ ] **Step 1: Write failing tests**

    - `TestCodexCloudMappingEveryItemType`: one fixture with every item type the
      spec's table lists plus `unknownType`; assert role, tool name, category,
      and that the unknown type is kept with its raw name.
    - `TestCodexCloudNewestRecordWins`: item appended twice with different output;
      one tool result with the newer output.
    - `TestCodexCloudSessionFields`: title fallback, times, entrypoint values, cwd
      fallback over a record with null `environments`.
    - `TestCodexCloudResolvedParent`: spawn-source parent and parent-plus-fork
      both yield `ParentSessionID "codex-cloud:<parent>"`, `RelSubagent`.
    - `TestCodexCloudSkipsFileWithoutMetadata`.
    - `BenchmarkCodexCloudParseLargeThread`: 900-item synthetic thread.

- [ ] **Step 2: Run** `go test ./internal/parser -run CodexCloud`; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run** tests and the benchmark once; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(parser): parse codex-cloud mirror files"`

### Task 7: Raw capture for mirror files

Spec: Components (Provider) raw capture paragraph.

**Files:**

- Modify: `internal/parser/codex_cloud_provider.go`
- Test: `internal/parser/codex_cloud_test.go`, `internal/rawcapture/` test for
  replace after truncation

**Interfaces:**

- Produces: capabilities
  `RawCapture{Support: Supported, Shape: Files, Append: RawCaptureAppendOne, Snapshot: None}`;
  `PlanRawCapture` returns one appendable entry for the thread's mirror file
  and never a state file; `DiscoverRawCaptureSourcesEach` (the
  `StreamingRawCaptureSourceProvider` interface) yields every mirror file and
  no state file. Raw discovery rejects the provider without it.

- [x] **Step 1: Write failing tests** `TestCodexCloudRawCapturePlanOneEntry`
  (entry path ends in `.jsonl.zstd`, `Appendable`; no `.state.json`), and
  `TestCodexCloudRawCaptureReplacesAfterTruncation` (capture, truncate the
  tail and append, capture again: the plan replaces the file), and
  `TestCodexCloudRawDiscovery` (`parser.DiscoverRawCaptureSources` returns the
  mirror files, no state files, and no unsupported-feature error).

- [x] **Step 2: Run**; FAIL.

- [x] **Step 3: Implement** following `codexProvider.PlanRawCapture`
  (`internal/parser/codex_provider.go:360`).

- [x] **Step 4: Run**; PASS.

- [x] **Step 5: Commit**
  `git commit -m "feat(parser): raw capture for codex-cloud"`

## Phase 3: The fetcher

All fetcher code lives in `internal/codexcloud`. Test doubles live in
`internal/codexcloud/codexcloudtest`.

### Task 8: Codex homes, logins, and account keys

Spec: Authentication.

**Files:**

- Create: `internal/codexcloud/auth.go`, `token.go`, `auth_test.go`

**Interfaces:**

- Produces:

    - `type Token struct{ v string }` with `String()`, `GoString()`,
      `MarshalJSON()` all returning `"[redacted]"`, and `Reveal() string` used
      only by transports.
    - `func Redact(err error, tokens ...Token) error` replaces each token's raw,
      URL-query-escaped, and path-escaped forms.
    - `type AccountKey string`;
      `func AccountKeyFor(accountID, userID string) AccountKey` (hex SHA-256 of
      `accountID + "\x00" + userID`, first 16 bytes).
    - `type HomeState int` with `HomeOK`, `HomeSignedOut`, `HomeKeyring`,
      `HomeEphemeral`.
    - `type Login struct { Home string; Token Token; AccountID, UserID string; Key AccountKey; Store string }`
    - `func DiscoverHomes(getenv func(string) string, userHome string, configured []string) []string`
      (daemon `CODEX_HOME`, else `~/.codex`, then configured homes, deduped).
    - `func ReadLogin(home string) (Login, HomeState, error)`: reads
      `cli_auth_credentials_store` from the home's `config.toml` (default
      `file`); `keyring`/`ephemeral` never read `auth.json`; `auto` with no
      token is keyring-backed; decodes the ID token payload in memory only
      (`chatgpt_user_id`, else `user_id`); ignores `chatgpt_account_user_id`.
    - `func GroupByKey(logins []Login) map[AccountKey][]Login` preserving home
      order.

- [x] **Step 1: Write failing tests**

    - `TestReadLoginModes`: file, auto-with-token, auto-without (keyring), keyring
      with leftover `auth.json` (never read), ephemeral.
    - `TestAccountKeyIgnoresAccountUserClaim`: two ID tokens for the same user,
      one with `chatgpt_account_user_id`; same key.
    - `TestTwoUsersOneWorkspaceSeparate`.
    - `TestTokenNeverFormats`: `fmt.Sprintf("%v %+v %#v", tok, login, login)` and
      `json.Marshal(login)` contain no raw token.
    - `TestRedactEscapedForms`.

- [x] **Step 2: Run**
  `go test ./internal/codexcloud -run 'Login|Key|Token|Redact'`; FAIL.

- [x] **Step 3: Implement.** Fixtures build fake JWTs in the test; no real
  tokens.

- [x] **Step 4: Run**; PASS.

- [x] **Step 5: Commit**
  `git commit -m "feat(codexcloud): read Codex homes and logins"`

### Task 9: App-server websocket client and fake host

Spec: Components (Fetcher, Live token monitor), Configuration (redirects),
Authentication (identity, redaction).

**Files:**

- Modify: `go.mod` (make `github.com/coder/websocket` direct)
- Create: `internal/codexcloud/client.go`, `rpc.go`, `client_test.go`
- Create: `internal/codexcloud/codexcloudtest/host.go` (fake durable host)

**Interfaces:**

- Consumes: `Login`, `Token`, `RedactString` (Task 8).

- Produces:

    - `type Mode int` with `ModeFetch`, `ModeMonitor`.
    - `type DialOptions struct { Endpoint *url.URL; Login Login; Mode Mode; Version string; ReadLimit int64; HTTPClient *http.Client }`
    - `const DefaultReadLimit = 64 << 20`
    - `const ClientSubprotocol = "codex-client.agentsview"` (the gate may change
      it only with the user's consent).
    - `func Dial(ctx context.Context, o DialOptions) (*Conn, error)`: subprotocols
      and headers from Global Constraints, an `http.Client` whose
      `CheckRedirect` refuses, `SetReadLimit(o.ReadLimit)`, then `initialize`
      with `clientInfo{name:"agentsview", version}` and `initialized`. Every
      error it returns is redacted.
    - `func (c *Conn) Call(ctx context.Context, method string, params, result any) error`:
      the allowlist check runs before sending; `thread/resume` accepts only
      `ResumeParams{ThreadID string; ExcludeTurns bool}` with `ExcludeTurns`
      true.
    - `func (c *Conn) Notifications() <-chan Notification` (monitor mode only).
    - `func (c *Conn) Done() <-chan struct{}` and `func (c *Conn) Err() error`:
      closed and set when the connection ends, so a monitor reading only
      notifications learns about a refresh request or a read-limit overflow.
    - `type Notification struct { Method string; Params jsontext.Value }`
    - `var ErrRefreshRequested`, `ErrNotAllowed`, `ErrReplyTooLarge`,
      `ErrProtocolChange` (the server selected a subprotocol other than
      `codex-app-server` or none; the gate may pin this further), `ErrClosed`. A
      server request for `account/chatgptAuthTokens/refresh` closes the
      connection and sets `Err` to `ErrRefreshRequested`; every other server
      request is ignored, never answered. A message over the read limit closes
      the connection with `ErrReplyTooLarge`.
    - `type RPCError struct { Code int; Message string; Data jsontext.Value }`
    - `type HTTPError struct { Status int; RetryAfter time.Duration }` for a
      refused handshake (401, 403, 429, 5xx, any 3xx).
    - `func DecodeCount(v jsontext.Value) (int64, error)` accepts `4603049.0`,
      rejects `1.5`.
    - `codexcloudtest.Host`: `Handle(method, Handler)`, `Notify`, `ServerRequest`,
      `Requests`, `Methods`, `Handshakes`, `SelectSubprotocol`, `Redirect`,
      `RefuseHandshake(status, header, body)`, `EchoSubprotocols`. Later tasks
      script threads, turns, and items through `Handle`; an oversized reply is a
      handler returning a large result.

- [x] **Step 1: Write failing tests**

    - `TestDialHandshake`: subprotocols in order; headers; token only in the
      subprotocol; `initialize` then `initialized`.
    - `TestDialRefusesRedirect`: fake host answers 302; one request recorded;
      error is an `HTTPError` with status 302.
    - `TestHandshakeErrorRedacted`: fake host fails the upgrade echoing the
      subprotocol; error text has no token.
    - `TestAllowlist`: `thread/read` in fetch mode and `thread/resume` with an
      extra field fail with `ErrNotAllowed` and nothing is sent.
    - `TestServerRequestsNeverAnswered`: an approval request gets no reply; a
      refresh request closes the connection, `Done` closes, `Err` and the
      pending `Call` return `ErrRefreshRequested`.
    - `TestReadLimit`: a 40 KiB reply decodes; a reply over a 64 KiB test limit
      returns `ErrReplyTooLarge`.
    - `TestDecodeCount`.

- [x] **Step 2: Run** `go test ./internal/codexcloud/...`; FAIL.

- [x] **Step 3: Implement.**

- [x] **Step 4: Run**; PASS. `go mod tidy` leaves `coder/websocket` direct.

- [x] **Step 5: Commit**
  `git commit -m "feat(codexcloud): add app-server client"`

### Task 10: Fetcher state store

Spec: Fetcher state, Failure timing (persisted account wait).

**Files:**

- Create: `internal/codexcloud/state.go`, `state_test.go`

**Interfaces:**

- Produces:

    - `type Store struct{...}`; `func NewStore(root string) *Store` (root is
      `{dataDir}/sources/codex-cloud`).
    - `func (s *Store) Lock(key AccountKey) (unlock func())` (one mutex per
      account, shared by the sync job and the monitor).
    - `func (s *Store) MirrorPath(key AccountKey, threadID string) (string, error)`:
      thread IDs come from the server, so every path helper rejects an ID that
      is not a plain file name.
    - `type TurnState struct { ID, Status string; FirstSeen, EndedFirstSeen time.Time; CompletedAt, StartedAt *time.Time; Digest string }`
    - `type ThreadState struct { UpdatedAt int64; ResumeTurnID, ResumeItemID string; Offset int64; ResolvedParent string; Turns []TurnState; LastProgress time.Time; MetaDigest string; ItemDigests map[string]string; CreatedAt int64; LastPlanRecordDigest, LastPlanRecordKind string; CountedUnder string; LastHandledTurnID string; IsSubAgent bool; ArchivedFirstSeen time.Time }`
    - `type WaitState struct { Failure string; Attempts int; Until time.Time }`
    - `type UsagePauseState struct { Until time.Time; Reason string }`
    - `type PendingEntry struct { Since, NextAttempt time.Time; Attempts int }`
    - `type RefreshEntry struct { DataAsOf, LastAttempt time.Time; Settled bool }`
    - `type AccountState struct { Watermarks map[ListingID]int64; Wait WaitState; UsagePause UsagePauseState; Retry map[string]RetryEntry; CheckAgain map[string]bool; Unfinished map[string]UnfinishedEntry; UsagePending map[string]PendingEntry; Refresh map[string]RefreshEntry; AccountID, UserID string; Backfilled map[ListingID]bool }`
    - `type RetryEntry struct { Attempts int; NextAttempt, FirstFailure time.Time; Message string; Listing jsontext.Value }`
    - `type UnfinishedEntry struct { InProgress, Stalled, SubAgent bool }`
    - `type ListingID string` with `ListDefaultActive`, `ListDefaultArchived`,
      `ListSubAgentActive`, `ListSubAgentArchived`.
    - `LoadThread`/`SaveThread`, `LoadAccount`/`SaveAccount` (every set allocated
      on load), `LoadDescendants`/`SaveDescendants` and
      `LoadWaiting`/`SaveWaiting` (`[]string`; an empty list removes the file);
      atomic write through `go.kenn.io/kit/atomicfile`; missing file loads as
      zero value; files `0o600`, directories `0o700`.

- [x] **Step 1: Write failing tests** `TestStateRoundTrip`,
  `TestStateAtomicWrite` (a leftover temp file never replaces the old file),
  `TestLockSerializesWriters` (two goroutines updating different fields under
  the lock lose neither change), `TestLostThreadStateLoadsZero` (offset 0, so
  the next append recovers every frame).

- [x] **Step 2: Run**; FAIL.

- [x] **Step 3: Implement.**

- [x] **Step 4: Run**; PASS.

- [x] **Step 5: Commit**
  `git commit -m "feat(codexcloud): persist fetcher state"`

### Task 11: Error classification, waits, and the status board

Spec: JSON-RPC errors, Failure timing, Status and errors (messages).

**Files:**

- Create: `internal/codexcloud/classify.go`, `waits.go`, `status.go`,
  `classify_test.go`, `waits_test.go`, `status_test.go`

**Interfaces:**

- Consumes: `AccountState.Wait` (Task 10), `HTTPError`, `RPCError` (Task 9).

- Produces:

    - `type Category int` with `CatThreadError`, `CatAccountUnavailable`
      (overloaded or shutting down), `CatProtocolChange`, `CatAnchoredRead`,
      `CatAccountServiceError`.
    - `func Classify(method string, anchored bool, e *RPCError) Category` applying
      the spec's ordered rules (environment ended first; `-32001`;
      `serverShuttingDown`; `-32601` with `unsupported app-server method`;
      `-32600` with `Invalid request:` or `experimentalApi`; `-32602` anchored
      vs elsewhere; other replies by method).
    - `type Failure int` with `FailSignedOut`, `FailKeyring`,
      `FailRefreshRequested`, `FailRefused`, `FailRateLimited`, `FailNetwork`,
      `FailServer`, `FailProtocol`, `FailTooLarge`.
    - `func FailureFor(err error, login Login) Failure`: a 401 in `auto` mode is
      `FailKeyring`, not `FailSignedOut`.
    - `type Waits struct{...}` with an injected `now func() time.Time`:
      `RecordAccount(st *AccountState, f Failure, retryAfter time.Duration)`,
      `AccountReady(st *AccountState) bool`, `ResetAccount(st *AccountState)`
      (the caller saves the state);
      `RecordHome(home string, f Failure, authMTime time.Time)`,
      `HomeReady(home string, authMTime time.Time) bool` (memory only).
    - Durations: refused 1h doubling to 24h; 429 `Retry-After` else 5m doubling to
      1h; network/5xx 1m doubling to 1h; signed out and refresh requested until
      `auth.json` mtime changes or 1h; protocol change as refused.
    - `type StatusBoard struct{...}` (safe for concurrent use) with
      `Set(row HomeStatus)`, `Snapshot() []HomeStatus`.
    - `type HomeStatus struct { Homes []string; State string; Message string; LastSuccess *time.Time; Imported, Total, Found int; RetryCount int; OldestFailure *time.Time; Live string; PlanUsage string }`
      with states `off_disabled_agents`, `off_disabled`, `off_no_sync`,
      `config_error`, `waiting`, `importing`, `up_to_date`, `retrying`, `error`.
    - `func Message(f Failure, retryAt time.Time) string`: the spec's table, word
      for word.

- [ ] **Step 1: Write failing tests** table-driven `TestClassify` (every rule
  and the spec's examples, including "no rollout found" and "not supported
  yet"), `TestWaitsDurations` on an injected clock,
  `TestHomeWaitEndsOnAuthChange`, `TestAccountsIndependent`,
  `TestAutoMode401IsKeyring`, `TestMessages` (each row of the spec table).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): classify errors and time waits"`

### Task 12: Listings and selection

Spec: Incremental sync (listings, steps 1 to 5, watermarks).

**Files:**

- Create: `internal/codexcloud/listing.go`, `selection.go`, `listing_test.go`

**Interfaces:**

- Consumes: `Conn.Call`, `Store`, `Classify`.

- Produces:

    - `type ListedThread struct { ID string; UpdatedAt int64; Status string; Raw jsontext.Value }`
    - `func listChanged(ctx context.Context, c *Conn, id ListingID, watermark int64, stored func(threadID string) (int64, bool)) (changed []ListedThread, newest int64, err error)`:
      `sortKey: "updated_at"`, `sortDirection: "desc"`, `modelProviders: []`,
      `sourceKinds` only for sub-agent listings, `archived` per listing; stops
      after the first page holding a thread strictly older than the watermark; a
      page whose `updatedAt` increases between rows returns a
      `CatProtocolChange` error.
    - `func buildSelection(now time.Time, acct *AccountState, changed []ListedThread, active map[string]bool) []string`
      unions changed threads, unfinished entries not stalled, `active` threads
      not stalled, check-again flags, due retry entries; removes retry entries
      not yet due; each thread once.

- [ ] **Step 1: Write failing tests**

    - `TestListingParams`: default listings send no `sourceKinds`; sub-agent
      listings send exactly the five sub-agent kinds; every call sends
      `modelProviders: []`.
    - `TestListingStopsAfterOlderPage`, `TestListingSharedSecondAcrossPages`.
    - `TestListingOutOfOrderIsProtocolChange` (Review Focus).
    - `TestUnchangedAccountOneCallPerListing`.
    - `TestActiveUpdateFoundWhileArchivedIsNewer` and the reverse.
    - `TestSelectionUnionOnce` (a thread in two listings and the unfinished set is
      selected once), `TestSelectionBackoff`, `TestStallOverridesActive`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): list and select changed threads"`

### Task 13: Fetch a thread, resume point, durable append

Spec: Incremental sync (finished rules, resume point, steps 6 and 7), Mirror
format (dedup).

**Files:**

- Create: `internal/codexcloud/fetch.go`, `resume.go`, `fetch_test.go`

**Interfaces:**

- Consumes: `mirror.Append`, `mirror.Turn`, `mirror.Item`, `Store`, `Conn.Call`.

- Produces:

    - `type TurnView string` with `TurnViewNotLoaded = "notLoaded"`,
      `TurnViewSummary = "summary"`, `TurnViewFull = "full"`; package variable
      `turnView` set from the gate's answer (default `notLoaded`).
    - `func fetchThread(ctx context.Context, c *Conn, st ThreadState, listing jsontext.Value, now time.Time) (records []mirror.Record, next ThreadState, err error)`:
      turn statuses first (newest first, stop at the resume turn, always with
      `itemsView`), then items from the resume anchor (`cursor` = item anchor,
      `turnId`), discarding the anchor item, then each later turn in full.
      `ErrReplyTooLarge` returns to the caller, which owns the redial.
    - `func itemFinished(it mirror.Item, turn TurnState, later *TurnState, archivedFirstSeen time.Time, now time.Time) bool`:
      12 minutes against server timestamps, 10 minutes after first sight
      otherwise; an archived thread counts only 10 minutes after
      `archivedFirstSeen`.
    - `func advanceResume(st ThreadState, items []mirror.Item, now time.Time) (turnID, itemID string)`:
      stops at the first unfinished item, and never leaves a turn until the
      save-lag margin has passed for that turn's end or for the start of a later
      turn, even when every saved item is finished.
    - `func dedup(st ThreadState, recs []mirror.Record) []mirror.Record` (by
      digest per key).
    - `func commitThread(store *Store, key AccountKey, threadID string, st ThreadState, recs []mirror.Record) (ThreadState, error)`:
      `mirror.Append(path, st.Offset, nil)` to recover, rebuild digests and the
      last plan record from `Recovered`, then `dedup`, then append, then save
      state. Dedup never runs against digests a crash left stale.

- [ ] **Step 1: Write failing tests**

    - `TestFirstImportFinishedThreadOneFetch` (`completedAt` 13 minutes old).
    - `TestRecentEndRefetchUntilMargin` (11 minutes: fetched until 12).
    - `TestArchivedDuringSaveLag` (archived seconds after the turn ends: the
      unsaved command stays after the resume point until 10 minutes later).
    - `TestResumeStaysInTurnUntilMargin` (every saved item finished, turn ended 1
      minute ago: an item saved later is still fetched).
    - `TestItemSavedAfterNextTurnStarts` (an item saved mid-flight in turn N after
      turn N+1 starts is fetched).
    - `TestParallelToolCallsFinishOutOfOrder`.
    - `TestSilentCommandStillFinal`.
    - `TestLaterTurnFetchedInFull`.
    - `TestLongThreadReadsOnlyNewTurns` (turn listing stops at the resume turn).
    - `TestTurnEndingBetweenCallsNotFinished`.
    - `TestUnchangedRefetchByteIdentical` (file bytes equal, no parse trigger).
    - `TestCrashBetweenAppendAndSave` (kill before save; next run: no gap, no
      duplicate item after parse).
    - `TestEveryTurnsListCarriesItemsView`.
    - `TestAnchoredReadErrorRereadsTurn`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): fetch threads incrementally"`

### Task 14: The sync pass: retries, check-again, refresh, oversized replies

Spec: Incremental sync (steps 3, 8 to 10), Components (refresh request,
oversized replies), Failure timing.

**Files:**

- Create: `internal/codexcloud/gates.go`, `pass.go`, `pass_test.go`

**Interfaces:**

- Produces:

    - `type Gates struct { CodexCloud config.CodexCloudConfig; Disabled bool; Homes []string; ReadErr error }`
      and `type GateSource func() Gates`: re-reads `config.toml` with
      `config.LoadReadOnly`, keeps the last good `Gates` on a read error and
      sets `ReadErr`. Both jobs use it. `NoSync` is passed separately (Global
      Constraints).
    - `type Syncer struct{ Store *Store; Waits *Waits; Status *StatusBoard; Now func() time.Time; Dial func(context.Context, DialOptions) (*Conn, error); Gates GateSource; NoSync bool; ReadLogin func(home string) (Login, HomeState, error); Version string; Log *log.Logger }`
    - `func (s *Syncer) Run(ctx context.Context) error` (the poller job's `Run`;
      returns nil for per-account failures).
    - `func (s *Syncer) runAccount(ctx context.Context, key AccountKey, logins []Login) error`:
      the thread step runs only when the account wait allows it; the plan usage
      step (Task 20) runs either way.
    - Check-again: set when the fetch started less than 2 minutes after the
      thread's `updatedAt`; cleared per the spec rule.
    - Retry entry stores the listing record; a retry appends it first when it
      differs from the stored metadata.
    - Oversized reply: redial, retry the same `thread/items/list` page once with
      `limit: 1`; a second overflow puts the thread in the retry set with the
      "reply too large" message and the pass continues on a fresh connection.
      Under the `full` turn view an oversized turn listing fails the thread at
      once.
    - `ErrRefreshRequested` ends the pass: durable work stays, no retry entries,
      no watermark advance, no plan usage;
      `Waits.RecordHome(home, FailRefreshRequested, ...)`; the next pass uses
      the next home on the key.
    - Status rows: "Importing, found N threads so far" until all four listings
      have finished a first pass, then "Importing N of M threads".

- [ ] **Step 1: Write failing tests**

    - `TestFailedFirstImportKeepsMetadata` (Review Focus).
    - `TestFailingThreadBacksOffAndDoesNotHoldWatermark`.
    - `TestCheckAgainSameSecond` and `TestCheckAgainNotSetForOldThread` with a
      local clock 90 seconds ahead.
    - `TestOversizedReplyRetriedOnceThenRetrySet` (Review Focus) and
      `TestOversizedTurnListingUnderFullFailsAtOnce`.
    - `TestRefreshRequestEndsPassEarly`.
    - `TestSecondHomeAfter401`.
    - `TestInterruptedBackfillResumes` and `TestFoundSoFarUntilAllListings`.
    - `TestFullResyncKeepsStateAndFetchesNothing` and
      `TestLostStateRefetchesWithoutDuplicates`.
    - `TestCardinalityScaling` (small vs large archive with deletions, tombstones,
      and a persistent archive: unchanged-pass calls and state files read stay
      constant).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): run sync passes with retries"`

### Task 15: Sync job in the daemon, gates, and status

Spec: Scheduler, Configuration (gates, re-read), Status and errors.

**Files:**

- Create: `cmd/agentsview/codex_cloud_jobs.go`,
  `cmd/agentsview/codex_cloud_jobs_test.go`,
  `internal/server/huma_routes_codex_cloud.go`
- Modify: `cmd/agentsview/main.go:503` (pass the jobs to `poller.Start`),
  `cmd/agentsview/serve_lifecycle.go:200-218` (status lines)

**Interfaces:**

- Consumes: `Syncer`, `GateSource`, `StatusBoard`.

- Produces:

    - `func codexCloudSyncJob(sync *codexcloud.Syncer) poller.Job` (name
      `codex-cloud-sync`, 5 minutes, jitter, `RunAtStart: true`).
    - Gate order each run: `NoSync` from the startup config, then
      `disabled_agents`, then `Present`/`Enabled`, then `CodexCloud.Err`; a
      `config.toml` read error keeps the last good settings and shows "<config
      path> could not be read: <reason>. Cloud import keeps its last good
      settings."
    - `GET /api/v1/codex-cloud/status` returning `[]HomeStatus`, always
      registered, empty when the source is not configured (configuration can
      change without a restart).
    - `serve status` prints one line per row from that endpoint.

- [ ] **Step 1: Write failing tests**

    - `TestCodexCloudJobOffStates` (each gate, including `--no-sync` and an
      archive-only daemon, yields its Off state and sends no request to the fake
      host).
    - `TestCodexCloudHandEditTakesEffect`.
    - `TestCodexCloudUnreadableConfigKeepsLastGood`.
    - `TestStatusMessages` (401 shows `codex login`; 403 does not; `auto` mode 401
      shows the keyring message; retrying shows count and message; never "Up to
      date" while retrying).
    - `TestTokenCanary` (Review Focus): unique fake token and ID token through
      sync, a refresh request, 401/403/5xx; scan every file under the data dir,
      the captured `log` output, the status JSON, and `serve status` output.
      Tasks 17 and 20 extend it.
    - `TestUnconfiguredSourceHasNoStatusRow` (route answers an empty list).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat: run codex-cloud sync in the daemon"`

**Milestone: working locally.** After the gate's probe answers are in, ask the
user to run `AGENTSVIEW_DATA_DIR=<scratch> agentsview serve` with
`[agents.codex-cloud] enabled = true` and `live_tokens = false`, and to check
that their threads appear as sessions. Do not run it yourself: it uses their
login.

## Phase 4: Live token capture

### Task 16: Monitor joins and token records

Spec: Live token capture (Joining, While joined, What gets stored).

**Files:**

- Create: `internal/codexcloud/monitor.go`, `monitor_test.go`,
  `internal/codexcloud/mirror/live.go` (record bodies the parser also reads)

**Interfaces:**

- Produces:

    - `type Monitor struct{ Store *Store; Waits *Waits; Status *StatusBoard; Now func() time.Time; Dial func(context.Context, DialOptions) (*Conn, error); Gates GateSource; NoSync bool; ReadLogin func(home string) (Login, HomeState, error); Trigger func() error; Log *log.Logger }`
    - `func (m *Monitor) Join(ctx context.Context, key AccountKey, login Login, threadID string) error`
      (re-check newest turn, `thread/resume`, branch on `resumeKind`).
    - `func (m *Monitor) CloseAll()` (flush and close; used by gates and
      shutdown).
    - Records in `internal/codexcloud/mirror`:
      `TokenUsageData{JoinID, TurnID string; Total, Last TokenCounts; ModelContextWindow int64; Model string; ObservedAt time.Time}`,
      `TurnCompletedData{JoinID, TurnID string; ObservedAt time.Time}`,
      `TokenCounts{Input, CachedInput, CacheWriteInput, Output, ReasoningOutput, Total int64}`.
    - A connection that ends (`Conn.Done`) with `ErrRefreshRequested` flushes the
      buffer, records the home wait, and closes every join for that home.
    - Buffer flush at most once a minute, on `turn/completed`, on leave; at most
      10 joins per account; leave after 6 hours.
    - Cold or completed: store `LastHandledTurnID`; never rejoin until a newer
      turn ID appears.
    - First metadata record only after `warm`, without `updatedAt` and
      `recencyAt`.
    - After `turn/completed`: call `Trigger` (wraps `TriggerNow`, treats
      `ErrTriggerPending` as success) in its own goroutine.

- [ ] **Step 1: Write failing tests** following the spec's Live token capture
  test list: warm stays until `turn/completed`; cold closes and stuck turn
  causes one cold resume (Review Focus); just-completed turn not rejoined
  (Review Focus); unknown or missing `resumeKind` stops capture; refresh
  request flushes, closes, starts the home wait; `model/rerouted` changes the
  model; buffer coalescing; join cap; 6-hour leave; `environment_terminated`
  retried; `1.5` counter stops capture; several joined turns completing
  together queue at most one extra sync run and `ErrTriggerPending` is not an
  error (`TestTriggerCoalesces`).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): join running threads for token counts"`

### Task 17: Live discovery job

Spec: Live token capture (Finding running threads), gates.

**Files:**

- Create: `internal/codexcloud/discovery.go`, `discovery_test.go`
- Modify: `cmd/agentsview/codex_cloud_jobs.go`

**Interfaces:**

- Produces:

    - `func (m *Monitor) Discover(ctx context.Context) error` (job `Run`).
    - `func codexCloudLiveJob(m *codexcloud.Monitor) poller.Job` (name
      `codex-cloud-live`, 30 seconds, jitter).
    - Discovery: non-archived default listing pages while a page holds `active` or
      recently updated threads; newest-turn check per candidate; unfinished set
      entries with `InProgress` and not `Stalled` and not `SubAgent`; skip
      joined threads and sub-agent threads.

- [ ] **Step 1: Write failing tests** `TestDiscoveryFindsIdleListedRunning`,
  `TestDiscoverySecondPage`, `TestDiscoverySkipsSubAgents`,
  `TestDiscoveryGatesCloseJoins`, `TestSync429HoldsDiscovery`,
  `TestDiscovery5xxDelaysSync`, `TestDiscoveryProtocolChangeStopsCaptureOnly`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS. Extend `TestTokenCanary` with a warm join.

- [ ] **Step 5: Commit**
  `git commit -m "feat: discover running codex-cloud threads"`

### Task 18: Token records to usage events and coverage

Spec: What gets stored, How it shows up (coverage).

**Files:**

- Create: `internal/parser/codex_cloud_usage.go`, `codex_cloud_usage_test.go`
- Modify: `internal/usagefacts/fact.go:295` (new request-scoped source),
  `internal/parser/types.go` (coverage type on `ParsedSession`)

**Interfaces:**

- Produces:

    - `const CodexCloudRequestSource = "codex-cloud-request"`, added to
      `SourceIsRequestScoped`.
    - `type ParsedCaptureCoverage struct { LastCaptureAt *time.Time; UncapturedTurns int }`
      and `ParsedSession.CaptureCoverage *ParsedCaptureCoverage`.
    - `func codexCloudUsageEvents(threadID string, recs []mirror.TokenUsageData, fallbackModel string) (events []ParsedUsageEvent, totalOutput int64, peakContext int64)`:
      increases per field; a decrease starts a baseline; increase equal to
      `last` becomes a `CodexCloudRequestSource` event, otherwise `session`;
      `DedupKey = threadID + ":" + baseline + ":" + observedTotal`; field
      mapping as `applyCodexTokenUsage`; `MessageOrdinal` nil; model fallback to
      the listing model, else `unknown`.
    - `func codexCloudCoverage(turns []mirror.Turn, tokens []mirror.TokenUsageData, completions []mirror.TurnCompletedData) ParsedCaptureCoverage`
      (later turn in same baseline, or same-join token plus completion).

- [ ] **Step 1: Write failing tests**

    - `TestUsageIncrementsAndBaselines` (exact sum across a reset; repeated total
      adds nothing; recurring total after reset does not collide).
    - `TestRequestScopedWhenIncreaseEqualsLast` and
      `TestPriceBandAppliesAbove272k` (through `export` pricing with a GPT-6
      test catalog).
    - `TestCoverageRules` (spec cases: before first record; after last; join IDs;
      reset gap; 6-hour leave).
    - `TestSessionTotalsCountEachIncreaseOnce`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.** `TotalOutputTokens` and `PeakContextTokens` set by
  the provider with `Has*` flags; check that `prepare.go` does not count
  request-scoped events a second time.

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(parser): turn codex-cloud token records into usage"`

## Phase 5: Plan usage

### Task 19: `query_v2` client and usage roots

Spec: Plan usage (Usage roots, request shape), Fetcher state (descendants,
waiting files).

**Files:**

- Create: `internal/codexcloud/usage_client.go`, `roots.go`,
  `usage_client_test.go`, `roots_test.go`,
  `internal/codexcloud/codexcloudtest/usage.go` (fake `query_v2`),
  `internal/codexcloud/mirror/plan.go` (row and outcome bodies)

**Interfaces:**

- Produces:

    - `type UsageGroup struct { ThreadID string; CreatedAt time.Time; Descendants []string }`
    - `type UsageResponse struct { DataAsOf time.Time; Threads []mirror.UsageRow }`;
      in `mirror`:
      `UsageRow{ThreadID, DataStatus, UsageSource string; WeeklyPercent, FiveHourPercent *float64; Credits *string; Groups []UsageRowGroup}`
      (credits stay decimal strings, exponent form accepted).
    - `func QueryUsage(ctx context.Context, hc *http.Client, endpoint *url.URL, login Login, version string, groups []UsageGroup) (UsageResponse, error)`
      (bearer header, conditional account header, no redirects, ISO 8601 UTC
      `created_at`).
    - `func (s *Store) LinkParent(key AccountKey, child, parent string) (root string, moved []string, err error)`
      (descendants file, waiting file, late-parent move, "counted under" records
      via the caller).

- [ ] **Step 1: Write failing tests** `TestQueryUsageRequestShape`,
  `TestQueryUsageNoRedirect`, `TestQueryUsageCreditsExponent`,
  `TestNestedDescendantsGroupedUnderRoot`,
  `TestLateParentMovesChildAndGrandchildren`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): query plan usage per root"`

### Task 20: Plan usage scheduling, settling, and outcome records

Spec: Plan usage (Fetching, Settling, outcome records).

**Files:**

- Create: `internal/codexcloud/usage_step.go`, `usage_step_test.go`
- Modify: `internal/codexcloud/pass.go` (run the step after the thread step, and
  whenever the thread host is waiting), `fetch.go` (the commit path links
  parents and marks usage roots pending)

**Interfaces:**

- Produces:

    - `func (s *Syncer) runUsage(ctx context.Context, key AccountKey, logins []Login, now time.Time) error`
    - Records in `mirror`:
      `PlanUsageRowData{Row UsageRow; DataAsOf time.Time; Final bool}`,
      `PlanUsageOutcomeData{Kind string; At time.Time; Root string}` with kinds
      `left_out`, `settled_no_row`, `account_unavailable`, `counted_under`,
      `too_many_descendants`.
    - Pending marks, re-entry wait (5 minutes doubling to 6 hours, reset when
      nothing runs or after 24 quiet hours), per-root backoff, refresh set
      (later of one day after `data_as_of` and one day after last attempt),
      budget of 5 requests with pending roots first, 100 groups and 1,000 IDs,
      settling after 7 days, 429 pause, 401 next home, 403/404 24-hour
      unavailable.
    - Commit-path hook: after each thread commit, compute `ResolvedParent` and
      `IsSubAgent`, call `Store.LinkParent`, set the unfinished entry's
      `SubAgent` flag, and mark the usage root pending.
    - Waits: plan usage runs while the thread host waits; it skips homes in a
      signed-out or refresh-requested wait; a `query_v2` 401 is remembered per
      home in memory and pauses plan usage until any home on the key changes its
      `auth.json`, or one hour passes.
    - Dedup of plan records runs after `commitThread`'s recovery step.

- [ ] **Step 1: Write failing tests** for every Plan usage case in the spec's
  Testing list (budget, settling, refresh timing including `data_as_of`
  earlier than the last attempt, left-out, stale outcome, 401 home fallback,
  403/404, 429, pending re-entry, oversized group, linking and pending marks
  during the thread step, plan usage while the thread host waits, a crash
  after a row append with a newer outcome recovered).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS. Extend `TestTokenCanary` with plan usage.

- [ ] **Step 5: Commit**
  `git commit -m "feat(codexcloud): schedule and record plan usage"`

### Task 21: Parse plan usage records

Spec: Plan usage (Storage, parse-time ordering).

**Files:**

- Create: `internal/parser/codex_cloud_plan_usage.go`, test (reads the `mirror`
  row and outcome bodies)
- Modify: `internal/parser/types.go`

**Interfaces:**

- Produces:

    - `type ParsedPlanUsage struct { DataAsOf *time.Time; DataStatus, UsageSource string; WeeklyPercent, FiveHourPercent *float64; Credits *string; Groups []ParsedPlanUsageGroup; Final bool; OutcomeKind string; OutcomeAt *time.Time; CountedUnder string }`
    - `ParsedSession.PlanUsage *ParsedPlanUsage`
    - Ordering by record position: newest row and newest outcome kept separately;
      an outcome stored only when newer than the row; a `counted_under` newer
      than the row clears plan values and sets
      `CountedUnder = "codex-cloud:<root>"`.

- [ ] **Step 1: Write failing tests** `TestPlanUsageNewestRowWins`,
  `TestStaleOutcomeAfterRow`, `TestCountedUnderClearsValues`,
  `TestOutcomesSurviveReparse`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(parser): parse codex-cloud plan usage"`

## Phase 6: Storage and replicas

Read `docs/agents/storage.md` before each task here. Follow `usage_events` as
the pattern at every site the storage survey lists.

### Task 22: SQLite `session_plan_usage`

**Files:**

- Modify: `internal/db/schema.sql`, `internal/db/db.go` (migration,
  `readOnlyRequiredTables`), create `internal/db/plan_usage.go`
- Modify: `internal/ingest/prepare.go`, `internal/ingest/convert.go`
  (`PreparedSession.PlanUsage`), `internal/db/session_batch.go`,
  `internal/sync/engine.go` (batch and direct writes),
  `internal/db/orphaned.go` (guarded copy), `internal/db/session_replace.go`
- Test: `internal/db/plan_usage_test.go`, `internal/db/orphaned_test.go`

**Interfaces:**

- Produces:

    - Table
      `session_plan_usage(session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE, data_as_of TEXT, data_status TEXT, usage_source TEXT, weekly_percent REAL, five_hour_percent REAL, credits TEXT, groups_json TEXT NOT NULL DEFAULT '[]', is_final INTEGER NOT NULL DEFAULT 0, outcome_kind TEXT NOT NULL DEFAULT '', outcome_at TEXT, counted_under TEXT NOT NULL DEFAULT '', has_plan_usage INTEGER NOT NULL DEFAULT 0, last_capture_at TEXT, uncaptured_turns INTEGER NOT NULL DEFAULT 0, has_coverage INTEGER NOT NULL DEFAULT 0)`
    - `type SessionPlanUsage struct{...}` mirroring the columns.
    - `func replaceSessionPlanUsageTx(ctx, tx, sessionID string, p *SessionPlanUsage) error`
      (bumps `local_modified_at`),
      `func (d *DB) GetSessionPlanUsage(ctx, id) (*SessionPlanUsage, error)`,
      `func (d *DB) PlanUsageFingerprints(ctx, ids []string) (map[string]string, error)`.

- [ ] **Step 1: Write failing tests** `TestPlanUsageReplaceAndRead`,
  `TestPlanUsageBumpsLocalModified`,
  `TestFullResyncKeepsOrphanAndTrashPlanUsage`,
  `TestReadOnlyOpenRequiresPlanUsageTable`, ingest test that a parsed
  `PlanUsage`/`CaptureCoverage` reaches the table.

- [ ] **Step 2: Run**
  `go test -tags fts5 ./internal/db ./internal/ingest ./internal/sync -run PlanUsage`;
  FAIL.

- [ ] **Step 3: Implement.** Bump `dataVersion` only if parsed output for
  existing agents changes (it should not).

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(db): store plan usage and capture coverage"`

### Task 23: PostgreSQL, hosted tenant, and raw projection

**Files:**

- Modify: `internal/postgres/schema.go`, `schema_columns.go`,
  `hosted_tenant_schema.go:30-39`, `raw_runtime_privileges.go:21`, `push.go`
  (replace alongside `replaceUsageEvents`), `push_fingerprint.go`,
  `raw_projection_rows.go:129-161`, `raw_projection_links.go` (new link kind
  `usage_root`), `hosted_session_ids.go`, `hosted_reports.go`
- Test: `internal/postgres/*_pgtest_test.go`,
  `cmd/agentsview/pg_raw_parity_pgtest_test.go`

**Interfaces:**

- Produces: PostgreSQL `session_plan_usage` matching SQLite; push fingerprint
  field `PlanUsageFP`; `writeRawLinks` adds
  `add("usage_root", -1, -1, -1, counted_under)`; hosted reads map
  `counted_under` through `mapIDs`.

- [ ] **Step 1: Write failing tests** (tag `pgtest`):
  `TestPushPlanUsageRoundTrip`, `TestPushPlanUsageChangeRepushes`,
  `TestHostedProjectionWritesPlanUsage`, `TestHostedGrandchildUsageRootMapped`.

- [ ] **Step 2: Run** `make test-postgres` scoped with `-run PlanUsage`; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit** `git commit -m "feat(postgres): replicate plan usage"`

### Task 24: ClickHouse

**Files:**

- Modify: `internal/clickhouse/schema.go`, `push.go` (`dependentTables`,
  `insertDependents`), `push_fingerprint.go`, `internal/db` snapshot loader
  (`LoadSessionMirrorSnapshot` adds plan usage)

- [ ] **Step 1: Write failing tests** (tag `chtest`):
  `TestClickHousePlanUsageRoundTrip`, `TestClickHouseDeleteClearsPlanUsage`,
  `TestClickHouseSameSessionFromTwoMachines` (spec: Several machines; record
  the observed behavior for Task 29).

- [ ] **Step 2: Run** `make test-clickhouse` scoped; FAIL.

- [ ] **Step 3: Implement** as `ReplacingMergeTree(push_version)`.

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(clickhouse): replicate plan usage"`

### Task 25: DuckDB mirror

**Files:**

- Modify: `internal/duckdb/schema.go` (`SchemaVersion = 18`, table), `push.go`
  (`pushSession`, `deleteMirrorSession`), `sync.go` (fingerprints)

- [ ] **Step 1: Write failing tests** `TestDuckDBPlanUsageRoundTrip`,
  `TestDuckDBSchemaVersionRebuild`.

- [ ] **Step 2: Run** `go test -tags fts5 ./internal/duckdb -run PlanUsage`;
  FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit** `git commit -m "feat(duckdb): mirror plan usage"`

## Phase 7: Surfaces

### Task 26: Usage totals leave out plan-only sessions

Spec: Replicas and mirror (left-out and partly-captured counts), Display.

**Files:**

- Modify: `internal/db/usage.go`, `usage_cache_consumers.go`,
  `internal/postgres/usage.go`, `internal/duckdb/analytics_usage.go`,
  `internal/clickhouse/usage.go`, `internal/service/usage.go`,
  `internal/service/direct.go`, `internal/server/usage.go:66-104`
- Test: per-backend usage tests plus a new parity test

**Interfaces:**

- Produces:

    - `UsageSummaryResult.PlanOnlySessions int` and
      `UsageSummaryResult.PartlyCapturedSessions int` (and the server mirror),
      computed at query time per backend with the spec's filter rules
      (`usageMatchingMessageEligibility` for plan-only; usage-event matching for
      partly captured).
    - `codex-cloud` never sets `NoPerMessageTokenData`.
    - A per-model plan-only marker in the model totals, so per-model token and
      cost columns can show "Plan usage".

- [ ] **Step 1: Write failing tests** `TestPlanOnlySessionsLeftOutOfTotals`,
  `TestPlanOnlyCountFilters` (agent, project, machine, model include and
  exclude, date window, timezone, session with no assistant message),
  `TestPartlyCapturedCount`, and a cross-backend `TestPlanUsageCountsParity`
  (tags `pgtest || chtest`).

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat: leave plan-only sessions out of token totals"`

### Task 27: API, CLI, and exports

**Files:**

- Modify: `internal/service/service.go:314-375` (both alias structs),
  `internal/db/usage.go` (`SessionUsage.PlanUsage`),
  `internal/server/huma_routes_sessions.go:624-737`, `cmd/agentsview/usage.go`
  (left-out note), `cmd/agentsview/session_usage.go` (plan usage block),
  `internal/db/session_export.go`, `cmd/agentsview/export.go`; regenerate
  `internal/apiclient` and the frontend client (`npm run generate:api`).

**Interfaces:**

- Produces: `SessionDetail.PlanUsage *PlanUsageView` and
  `SessionDetail.CaptureCoverage *CaptureCoverageView`; session usage response
  `plan_usage`; presence flags false for plan-only sessions; export rows carry
  plan usage fields with empty token and cost values.

- [ ] **Step 1: Write failing tests** `TestSessionDetailIncludesPlanUsage`,
  `TestSessionUsagePlanOnlyFlagsFalse`, `TestUsageCLILeftOutNote`,
  `TestExportPlanOnlyEmptyCost`.

- [ ] **Step 2: Run**; FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run**; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat: expose plan usage in API, CLI, and exports"`

### Task 28: Frontend

**Files:**

- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.svelte` (Plan
  usage section, coverage line, origin badge), `usage/UsagePage.svelte`,
  `usage/UsageSummaryCards.svelte`, `stores/usage.svelte.ts` (carry the new
  counts through `summaryForDateRange`), `usage/TopSessionsTable.svelte`,
  analytics `TopSessions.svelte`, and the model totals in
  `AttributionPanel.svelte` ("Plan usage" cells), `layout/StatusBar.svelte`
  (codex-cloud status line), `messages/*.json` (all locales)

- [ ] **Step 1: Write failing tests** (Vitest): breadcrumb renders each Plan
  usage state from the spec's list; credits note; "N turns not captured" with
  and without the plan usage clause; usage page shows the left-out and
  partly-captured counts with plural forms; every session list and per-model
  column shows "Plan usage" for plan-only rows; status bar shows the source
  line only when configured.

- [ ] **Step 2: Run** `vp test`; FAIL.

- [ ] **Step 3: Implement** with existing shared components only.

- [ ] **Step 4: Run** `vp check`, `vp test`, `npm run i18n:compile`; PASS.

- [ ] **Step 5: Commit**
  `git commit -m "feat(frontend): show codex-cloud plan usage and status"`

### Task 29: Documentation

**Files:**

- Modify: `docs/internal/session-format-sources.md` (evidence entry with the
  gate's probe answers), `docs/configuration.md`, `docs/commands.md`,
  `README.md` (agent list), `docs/changelog.md`

- [ ] **Step 1: Write** the sections the spec's Documentation list names,
  including every listed limit, the one-machine recommendation, and how
  ClickHouse and a hosted hub handle one session pushed from two machines
  (from Task 24's test and the hosted projection). `docs/configuration.md`,
  `docs/commands.md`, and `docs/changelog.md` are excluded from the mdformat
  hook (`prek.toml`); do not reformat them.

- [ ] **Step 2: Run** the mdformat hook on the other changed files and the
  private-data scrub.

- [ ] **Step 3: Commit** `git commit -m "docs: document the codex-cloud source"`
