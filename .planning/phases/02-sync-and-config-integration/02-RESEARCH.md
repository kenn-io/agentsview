# Phase 2: Sync and Config Integration - Research

**Researched:** 2026-02-27
**Domain:** Go config layering, sync engine wiring, discovery patterns
**Confidence:** HIGH

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CONF-01 | `PiDir` (single) and `PiDirs` (multi) fields added to `Config` struct following `GeminiDir`/`GeminiDirs` pattern | Config struct + loadEnv + loadFile patterns fully documented below |
| CONF-02 | `PI_DIR` env var overrides `PiDir` (same as `GEMINI_DIR` pattern) | `loadEnv()` pattern is verbatim copy with name substitution |
| CONF-03 | Default `PiDir` is `~/.pi/agent/sessions/` | `Default()` function pattern documented |
| CONF-04 | `ResolvePiDirs()` method returns effective directory list (env var > config file > default) | `resolveDirs()` helper already exists; one-liner method |
| CONF-05 | Pi session files discovered by scanning encoded-cwd subdirectories for `*.jsonl`, validated by reading session header (type="session") | Discovery validation pattern (read first line, check `type` field) fully documented |
| SYNC-01 | Sync engine dispatches pi through all 4 wiring points: `processFile` switch, `classifyOnePath`, `syncAllLocked` discovery, `piDirs` field | All 4 points mapped with exact code locations and change required |
| SYNC-02 | Pi sessions appear in DB after startup sync and 15-min periodic sync | Wiring to existing `SyncAll`/`SyncPaths` paths handles this automatically |
| TEST-02 | Integration test seeding a temp pi session directory and verifying sessions appear in DB via sync engine | Integration test pattern from existing `engine_integration_test.go` fully documented |
</phase_requirements>

## Summary

Phase 2 wires pi-agent sessions through the three layers that control discovery and persistence: `internal/config/config.go`, `internal/sync/discovery.go`, and `internal/sync/engine.go` (plus the startup call-site in `cmd/agentsview/main.go`). All three layers have already-established patterns for other agents; adding pi is a structured extension of those patterns.

The config layer uses a layered approach: `Default()` sets a home-relative path, `loadEnv()` reads `PI_DIR` and populates both the single-dir and multi-dir fields, `loadFile()` reads `pi_dirs` from `~/.agentsview/config.json`, and `ResolvePiDirs()` calls the existing `resolveDirs()` helper. Every other multi-dir agent (Gemini, Codex, Copilot) follows the same pattern verbatim.

The sync engine has four precise wiring points. Wiring pi into all four correctly is the entire goal of SYNC-01. The `processFile` switch currently logs "unknown agent type" for AgentPi — adding a `case parser.AgentPi` that calls a new `processPi()` method eliminates that. The other three points each need one block of code modeled on the Gemini or Codex pattern.

Discovery for pi uses content-validation (read first line, confirm `type == "session"`) rather than directory-name pattern matching. This is already the decision captured in STATE.md and is the correct approach because pi directory encoding format is ambiguous between installations.

**Primary recommendation:** Implement the four wiring points in strict order — (1) config fields+env, (2) discovery function, (3) engine struct+NewEngine+syncAllLocked+classifyOnePath+processFile, (4) main.go call-site — then write the integration test.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `os` stdlib | Go stdlib | File system traversal, `ReadDir`, `Stat`, `Open` | All existing discovery uses this directly |
| `path/filepath` stdlib | Go stdlib | Path joining, `Base`, `Ext`, `Clean` | All path manipulation uses this |
| `github.com/tidwall/gjson` | already in go.mod | JSON field extraction without full unmarshal | Parser uses this; discovery reads first line |
| `github.com/wesm/agentsview/internal/parser` | internal | `ParsePiSession`, `AgentPi` constant | Already implemented in Phase 1 |
| `github.com/wesm/agentsview/internal/dbtest` | internal | `OpenTestDB(t)`, `WriteTestFile(t, path, data)` | Used by all sync integration tests |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `sort` stdlib | Go stdlib | Sort discovered files by path | All discovery functions sort before returning |
| `strings` stdlib | Go stdlib | `HasSuffix`, `Split`, `TrimSuffix` | Path classification in `classifyOnePath` |
| `github.com/wesm/agentsview/internal/testjsonl` | internal | JSONL fixture builders | Integration tests that need inline session data |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| gjson for header validation | `encoding/json` full unmarshal | gjson is already the project standard for all parsers; use it |
| content validation | directory name pattern | Rejected — encoding format ambiguous; content validation is locked decision |

## Architecture Patterns

### Config Layering Pattern (verbatim from existing agents)

Every multi-dir agent follows: struct fields → `Default()` → `loadEnv()` → `loadFile()` → `ResolveDirs()`.

**Struct fields** (add after `GeminiDir`/`GeminiDirs`):
```go
// In Config struct
PiDir  string   `json:"pi_dir"`
PiDirs []string `json:"pi_dirs,omitempty"`
```

**Default()** (add after GeminiDir line):
```go
PiDir: filepath.Join(home, ".pi", "agent", "sessions"),
```

**loadEnv()** (add at end of method, same pattern as GEMINI_DIR):
```go
if v := os.Getenv("PI_DIR"); v != "" {
    c.PiDir = v
    c.PiDirs = []string{v}
}
```

**loadFile()** — add `PiDirs []string \`json:"pi_dirs"\`` to the local `file` struct, then:
```go
if len(file.PiDirs) > 0 && c.PiDirs == nil {
    c.PiDirs = file.PiDirs
}
```

**ResolvePiDirs()** (one-liner method):
```go
func (c *Config) ResolvePiDirs() []string {
    return c.resolveDirs(c.PiDirs, c.PiDir)
}
```

### Discovery Pattern for Pi

Pi sessions live in `<piDir>/<encoded-cwd>/<session-id>.jsonl`. The encoded-cwd subdirectory name format is ambiguous (`--path--` vs `-path/` depending on pi version), so discovery validates by file content, not directory name.

```go
// DiscoverPiSessions finds JSONL files under piDir that are
// valid pi sessions (header type="session").
func DiscoverPiSessions(piDir string) []DiscoveredFile {
    if piDir == "" {
        return nil
    }
    // Top-level entries are encoded-cwd subdirectories
    entries, err := os.ReadDir(piDir)
    if err != nil {
        return nil
    }
    var files []DiscoveredFile
    for _, entry := range entries {
        if !isDirOrSymlink(entry, piDir) {
            continue
        }
        cwdDir := filepath.Join(piDir, entry.Name())
        // project name derived from cwd encoding
        project := parser.ExtractProjectFromCwd(
            decodePiCwdDir(entry.Name()),
        )
        sessionFiles, err := os.ReadDir(cwdDir)
        if err != nil {
            continue
        }
        for _, sf := range sessionFiles {
            if sf.IsDir() {
                continue
            }
            if !strings.HasSuffix(sf.Name(), ".jsonl") {
                continue
            }
            path := filepath.Join(cwdDir, sf.Name())
            if !isPiSessionFile(path) {
                continue
            }
            files = append(files, DiscoveredFile{
                Path:    path,
                Project: project,
                Agent:   parser.AgentPi,
            })
        }
    }
    sort.Slice(files, func(i, j int) bool {
        return files[i].Path < files[j].Path
    })
    return files
}

// isPiSessionFile reads the first line and checks type == "session".
func isPiSessionFile(path string) bool {
    f, err := os.Open(path)
    if err != nil {
        return false
    }
    defer f.Close()
    // Read just enough for the first JSON line
    buf := make([]byte, 512)
    n, _ := f.Read(buf)
    line := string(buf[:n])
    // Trim to first newline
    if i := strings.IndexByte(line, '\n'); i >= 0 {
        line = line[:i]
    }
    return gjson.Get(line, "type").Str == "session"
}
```

**Note on project extraction:** The cwd-encoded directory name needs decoding to a filesystem path before calling `parser.ExtractProjectFromCwd`. The encoding used by pi is not standardized across versions. The simplest approach that covers both formats (`--path--` replacing `/` with `--` and `-path/` replacing `/` with `-`) is to try the known Claude format (`parser.DecodeCursorProjectDir` won't help here) or simply use a best-effort path reconstruction. Given the ambiguity, `decodePiCwdDir` can be a stub that tries both formats and falls back to the directory name itself. If the derived project is empty, use `"unknown"`.

**Alternative simpler approach:** Pass `project=""` to `ParsePiSession` and let it derive the project from the `cwd` field in the session header. This is already supported — `ParsePiSession` already has: `if project == "" && cwd != "" { project = ExtractProjectFromCwd(cwd) }`. Using `project=""` in discovery and letting the parser derive it from the header `cwd` field is cleaner and more accurate than trying to decode the directory name.

### Four Engine Wiring Points

These are the exact locations that need changes. Confirmed by reading `internal/sync/engine.go`:

**Point 1 — Engine struct field** (line ~26–45 in engine.go):
```go
type Engine struct {
    // ... existing fields ...
    piDirs    []string  // ADD THIS
    // ...
}
```

**Point 2 — NewEngine constructor** (line ~51–75):
```go
func NewEngine(
    database *db.DB,
    claudeDirs, codexDirs, copilotDirs,
    geminiDirs, opencodeDirs []string,
    cursorDir, machine string,
    piDirs []string,  // ADD as new parameter
) *Engine {
    // ...
    return &Engine{
        // ... existing fields ...
        piDirs: piDirs,  // ADD
    }
}
```

**IMPORTANT:** Adding `piDirs` as a parameter to `NewEngine` changes the call signature. Both callers must be updated:
1. `cmd/agentsview/main.go` — `sync.NewEngine(...)` call
2. `internal/sync/engine_integration_test.go` — `sync.NewEngine(...)` call in `setupTestEnv`

**Point 3 — classifyOnePath** (line ~162–340, after Cursor block):
```go
// Pi: <piDir>/<encoded-cwd>/<session>.jsonl
for _, piDir := range e.piDirs {
    if piDir == "" {
        continue
    }
    if rel, ok := isUnder(piDir, path); ok {
        parts := strings.Split(rel, sep)
        if len(parts) != 2 {
            continue
        }
        if !strings.HasSuffix(parts[1], ".jsonl") {
            continue
        }
        return DiscoveredFile{
            Path:  path,
            Agent: parser.AgentPi,
            // project left empty; parser derives from header cwd
        }, true
    }
}
```

**Point 4 — processFile switch** (line ~845–864):
```go
case parser.AgentPi:
    res = e.processPi(file, info)
```

And the corresponding `processPi` method (model on `processGemini` or `processCopilot` pattern):
```go
func (e *Engine) processPi(
    file DiscoveredFile, info os.FileInfo,
) processResult {
    stored, storedMtime, ok := e.db.GetFileInfoByPath(file.Path)
    _ = stored
    if ok && storedMtime == info.ModTime().UnixNano() {
        return processResult{skip: true}
    }
    sess, msgs, err := parser.ParsePiSession(
        file.Path, file.Project, e.machine,
    )
    if err != nil {
        return processResult{err: err}
    }
    if sess == nil {
        return processResult{}
    }
    return processResult{
        results: []parser.ParseResult{{
            Session:  *sess,
            Messages: msgs,
        }},
    }
}
```

**Point 5 — syncAllLocked discovery call** (line ~538–575):
```go
var pi []DiscoveredFile
for _, d := range e.piDirs {
    pi = append(pi, DiscoverPiSessions(d)...)
}
// Then include pi in all/log/onProgress
```

Also update the `all` slice construction and log message to include pi count.

**Point 6 — main.go call-site** — Add pi wiring in `runServe`:
```go
warnMissingDirs(cfg.ResolvePiDirs(), "pi")
// Pass to NewEngine: add cfg.ResolvePiDirs() parameter
// Add pi watcher registration in startFileWatcher
```

### FindPiSourceFile Pattern

For the server's session re-sync path, `FindPiSourceFile` needs to search all pi directories for a session by ID. Since pi session files are named `<session-id>.jsonl` (V2) or arbitrary (V1 derives ID from filename), the search is:

```go
func FindPiSourceFile(piDir, sessionID string) string {
    if piDir == "" || !isValidSessionID(sessionID) {
        return ""
    }
    entries, err := os.ReadDir(piDir)
    if err != nil {
        return ""
    }
    target := sessionID + ".jsonl"
    for _, entry := range entries {
        if !isDirOrSymlink(entry, piDir) {
            continue
        }
        candidate := filepath.Join(piDir, entry.Name(), target)
        if _, err := os.Stat(candidate); err == nil {
            return candidate
        }
    }
    return ""
}
```

### Integration Test Pattern (TEST-02)

Model on `TestSyncEngineIntegration` in `engine_integration_test.go`. The test must:
1. Create a temp pi session directory with the expected sub-structure
2. Write a valid pi session JSONL file (can reuse or inline the `testdata/pi/session.jsonl` content)
3. Create a `sync.Engine` with the temp dir wired as `piDirs`
4. Run `SyncAll` and verify the session appears in the DB

The `setupTestEnv` function will need a `WithPiDirs` option or the test can construct `sync.NewEngine` directly with the pi dir. Since `NewEngine` signature is changing, tests that call it directly are simplest.

```go
func TestPiSessionIntegration(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    piDir := t.TempDir()
    // Pi sessions live in piDir/<encoded-cwd>/<session-id>.jsonl
    cwdSubdir := filepath.Join(piDir, "--Users-alice-code-my-project")
    if err := os.MkdirAll(cwdSubdir, 0o755); err != nil {
        t.Fatal(err)
    }
    sessionFile := filepath.Join(cwdSubdir, "pi-test-session-uuid.jsonl")
    // Write the session fixture (from testdata or inline)
    content := // ... pi session JSONL content ...
    if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
        t.Fatal(err)
    }

    database := dbtest.OpenTestDB(t)
    engine := sync.NewEngine(
        database,
        nil, nil, nil, nil, nil, // claude, codex, copilot, gemini, opencode
        "",                      // cursorDir
        "local",                 // machine
        []string{piDir},         // piDirs
    )

    stats := engine.SyncAll(nil)
    if stats.Synced != 1 {
        t.Fatalf("expected 1 synced, got %d", stats.Synced)
    }

    assertSessionState(t, database, "pi-test-session-uuid", func(sess *db.Session) {
        if sess.Agent != "pi" {
            t.Errorf("agent = %q, want %q", sess.Agent, "pi")
        }
    })
}
```

### Anti-Patterns to Avoid

- **Positional parameter trap:** `NewEngine` takes many slices in a fixed order. Adding `piDirs` must go in a consistent position and all callers must be updated. Recommend adding at the end of the existing slice parameters (before `cursorDir`).
- **Skip non-JSONL in discovery:** `DiscoverPiSessions` must skip files that don't have `.jsonl` extension before doing the header read — avoids I/O on unrelated files.
- **Empty piDir guard:** Every function that receives a piDir must check `piDir == ""` and return nil/empty immediately — matches all other agents.
- **Project empty vs "unknown":** Pass `project=""` to `ParsePiSession` and let the parser derive it from the header `cwd` field. Do not set `project="unknown"` in discovery — the parser has better information.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Multi-dir resolution precedence | Custom env/config/default logic | `c.resolveDirs(c.PiDirs, c.PiDir)` | Already exists; handles all precedence cases |
| Path containment check | Manual string prefix | `isUnder(dir, path)` in engine.go | Already exists with proper edge cases |
| JSONL first-line read | Custom buffered reader | `os.Open` + `f.Read(buf)` + `IndexByte('\n')` | Discovery only needs the first line; full `lineReader` is overkill here |
| Session file validation | Directory name pattern matching | Header `type == "session"` content check | Directory name format is ambiguous between pi versions |
| Test DB setup | Custom SQLite init | `dbtest.OpenTestDB(t)` | Project standard; handles FTS5 tags and cleanup |

## Common Pitfalls

### Pitfall 1: NewEngine Caller Count
**What goes wrong:** Adding `piDirs` to `NewEngine` signature breaks `engine_integration_test.go`'s `setupTestEnv` which calls `sync.NewEngine(...)` directly. This will be a compile error that only surfaces at test time if not caught during implementation.
**Why it happens:** There are two callers: `main.go` and `engine_integration_test.go`. Both must be updated.
**How to avoid:** Search for all `sync.NewEngine(` calls before submitting. Add `nil` as the `piDirs` argument to `setupTestEnv`'s call so existing tests still compile and pass.
**Warning signs:** Compile error `too few arguments in call to sync.NewEngine`.

### Pitfall 2: Discovery Returns Sessions Without Project
**What goes wrong:** If `project=""` is passed from discovery and the session header has no `cwd`, the parser falls back to `""` project, which can cause DB constraint violations or sessions that appear with no project label.
**Why it happens:** V1 pi sessions may lack `cwd` in the header.
**How to avoid:** In `ParsePiSession`, if both the passed-in project and the header cwd are empty, use `filepath.Base(filepath.Dir(path))` as a last-resort project name (the encoded-cwd directory name).
**Warning signs:** Sessions in DB with empty project field.

### Pitfall 3: Content Validation Read Size
**What goes wrong:** Reading only 512 bytes may truncate a session header line that is very long (session headers can include long `cwd` paths or extra fields).
**Why it happens:** Pi session headers can be long JSONL lines.
**How to avoid:** Read at least 4096 bytes, or read until the first `\n`. The simplest approach is `bufio.Scanner` with `ScanLines` and `scanner.Scan()` once to get the first line.
**Warning signs:** `isPiSessionFile` returns false for valid files with long headers.

### Pitfall 4: Watcher Registration Missing for Pi
**What goes wrong:** `startFileWatcher` in `main.go` doesn't register pi directories, so file-change events for pi sessions are not delivered. Sessions only appear on startup sync and 15-min periodic sync, but NOT immediately after writing.
**Why it happens:** SYNC-01 requires all four engine wiring points, but the watcher registration in `main.go` is a fifth touch-point that must also be updated.
**How to avoid:** Add pi dirs to the watcher loop in `startFileWatcher`, following the same pattern as Claude dirs (pi sessions are directly in the directory, no subdirectory like `session-state/` or `tmp/`).
**Warning signs:** Watcher test shows pi sessions only appearing after 15-min poll, not on file change.

### Pitfall 5: processFile switch default branch logs "unknown agent type"
**What goes wrong:** If pi files reach `processFile` without the `case parser.AgentPi` arm, the default branch produces log noise: `"unknown agent type: pi"`. This is the stated success criterion for SYNC-01.
**Why it happens:** The switch has no pi case until it is added.
**How to avoid:** Add `case parser.AgentPi: res = e.processPi(file, info)` to the switch. Confirmed: `AgentPi` constant exists in `internal/parser/types.go` from Phase 1.
**Warning signs:** Log output contains "unknown agent type: pi" after any sync.

## Code Examples

### processPi implementation (modeled on processCopilot pattern)

```go
// Source: modeled on processCopilot in internal/sync/engine.go
func (e *Engine) processPi(
    file DiscoveredFile, info os.FileInfo,
) processResult {
    mtime := info.ModTime().UnixNano()

    // Check skip cache (handled by processFile before calling us)
    // Check if stored mtime matches
    _, storedMtime, ok := e.db.GetFileInfoByPath(file.Path)
    if ok && storedMtime == mtime {
        return processResult{skip: true, mtime: mtime}
    }

    sess, msgs, err := parser.ParsePiSession(
        file.Path, file.Project, e.machine,
    )
    if err != nil {
        return processResult{err: err, mtime: mtime}
    }
    if sess == nil {
        return processResult{mtime: mtime}
    }
    return processResult{
        results: []parser.ParseResult{{
            Session:  *sess,
            Messages: msgs,
        }},
        mtime: mtime,
    }
}
```

**IMPORTANT correction:** Looking at how other `processX` methods work — `processFile` captures `mtime` once from stat and sets `res.mtime = mtime` at the end. The individual `processX` methods do NOT set mtime themselves — they only return results/err/skip. The `mtime` is assigned in `processFile` after the switch. So `processPi` should NOT set `mtime` — match the pattern of other agents.

The actual pattern (from processFile):
```go
res.mtime = mtime  // set after switch, not inside processPi
```

### Config test pattern (for config_test.go or manual verification)

```go
// Set PI_DIR env var, load config, verify ResolvePiDirs
os.Setenv("PI_DIR", "/custom/pi/dir")
defer os.Unsetenv("PI_DIR")
cfg, _ := config.LoadMinimal()
dirs := cfg.ResolvePiDirs()
// dirs == []string{"/custom/pi/dir"}
```

### Discovery helper: read first line safely

```go
// Source: project pattern; bufio.Scanner for first-line read
func isPiSessionFile(path string) bool {
    f, err := os.Open(path)
    if err != nil {
        return false
    }
    defer f.Close()
    s := bufio.NewScanner(f)
    s.Buffer(make([]byte, 8192), 8192)
    if !s.Scan() {
        return false
    }
    line := s.Text()
    return gjson.Get(line, "type").Str == "session"
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Add new agents to engine as special cases | Follow the established "add one block per wiring point" pattern | Established when Gemini/Cursor were added | All agents are structurally identical in the engine |
| Directory name pattern for validation | File content validation (header `type` field) | Decision for pi (STATE.md) | No dependency on directory encoding format |

**No deprecated patterns** — all existing agent patterns are current.

## Open Questions

1. **processPi: does it need `GetFileInfoByPath` check or does processFile's skip cache handle it?**
   - What we know: `processFile` handles skip cache (mtime unchanged = skip). But the skip cache only covers files that previously returned errors or nil results. Files that ARE in the DB with matching mtime are handled by... checking inside each processX method.
   - What's unclear: Looking at `processClaude` — it does NOT check `GetFileInfoByPath`; instead the DB's upsert-with-mtime-check is what prevents redundant writes. The skip cache is only for files that should be permanently skipped. The mtime check happens via `db.UpsertSession`/`writeBatch`.
   - Recommendation: Do NOT add a `GetFileInfoByPath` check in `processPi`. Follow the `processClaude` pattern exactly — just call `ParsePiSession` and return results. Let `writeBatch` handle deduplication.

2. **Should `FindPiSourceFile` be implemented in phase 2?**
   - What we know: `FindClaudeSourceFile`, `FindCodexSourceFile`, etc. are used by the server's `SyncSingleSession` path when a user requests a session re-sync from the UI.
   - What's unclear: SYNC-01 requirements list only the 4 engine wiring points + discovery. The roadmap says "02-02: Implement `DiscoverPiSessions()` and `FindPiSourceFile()`".
   - Recommendation: Yes, implement `FindPiSourceFile` in plan 02-02 alongside `DiscoverPiSessions`. The pattern is simple and complete without it the server's re-sync path will produce errors for pi sessions.

3. **Does `SyncSingleSession` also need pi wiring?**
   - What we know: `SyncSingleSession` (referenced in test_helpers_test.go) exists on the engine.
   - What's unclear: Whether it has its own agent dispatch that needs a pi case.
   - Recommendation: Search for `SyncSingleSession` in engine.go and check for any agent-specific dispatch. If it calls `classifyOnePath` or `processFile`, it is automatically handled by the wiring in SYNC-01. Flag as check item in plan 02-03.

## Validation Architecture

> `workflow.nyquist_validation` is not present in `.planning/config.json` — treating as not enabled. Skipping this section.

## Sources

### Primary (HIGH confidence)
- `/Users/carze/Documents/personal/misc/agentsview/internal/config/config.go` — complete config layer; all patterns for struct fields, loadEnv, loadFile, resolveDirs
- `/Users/carze/Documents/personal/misc/agentsview/internal/sync/engine.go` — all four wiring points confirmed by direct reading; processFile switch at line 845; classifyOnePath at line 162; syncAllLocked at line 538; Engine struct at line 26; NewEngine at line 51
- `/Users/carze/Documents/personal/misc/agentsview/internal/sync/discovery.go` — DiscoverGeminiSessions, DiscoverCopilotSessions as pattern models; isDirOrSymlink, isValidSessionID, sort.Slice pattern
- `/Users/carze/Documents/personal/misc/agentsview/internal/sync/engine_integration_test.go` — setupTestEnv, writeSession, TestSyncEngineIntegration patterns
- `/Users/carze/Documents/personal/misc/agentsview/internal/sync/test_helpers_test.go` — assertSessionState, assertSessionMessageCount, runSyncAndAssert helpers
- `/Users/carze/Documents/personal/misc/agentsview/internal/parser/pi.go` — ParsePiSession signature; project="" fallback via header cwd confirmed
- `/Users/carze/Documents/personal/misc/agentsview/internal/parser/testdata/pi/session.jsonl` — actual pi session format; valid header confirmed (type="session", id, cwd, branchedFrom fields)
- `/Users/carze/Documents/personal/misc/agentsview/cmd/agentsview/main.go` — runServe call-site for NewEngine; startFileWatcher pattern; warnMissingDirs call pattern
- `/Users/carze/Documents/personal/misc/agentsview/.planning/STATE.md` — locked decision: content validation (not directory name pattern) for pi discovery

### Secondary (MEDIUM confidence)
- None needed — all findings verified against source code directly.

### Tertiary (LOW confidence)
- None.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH — verified by reading source code directly
- Architecture: HIGH — all four wiring points confirmed by code; patterns copied verbatim from existing agents
- Pitfalls: HIGH — confirmed by reading actual code paths (NewEngine callers, processFile switch default branch, watcher registration)

**Research date:** 2026-02-27
**Valid until:** 2026-03-29 (stable internal codebase; 30-day window)
