# Architecture Patterns

**Domain:** Adding pi-agent support to agentsview
**Researched:** 2026-02-27
**Dimension:** Architecture — end-to-end integration from parser through sync, config, and frontend

---

## How an Existing Agent Integrates End-to-End

Using Gemini as the reference because its directory layout (encoded-path subdirs, JSONL files) most closely mirrors pi-agent.

### Integration Pipeline

```
PI_DIR env var / config.json
        |
        v
internal/config/config.go          ← PiDir field, ResolvePiDirs(), loadEnv(), loadFile()
        |
        v
cmd/agentsview/main.go             ← warnMissingDirs(), sync.NewEngine(..., piDirs), startFileWatcher()
        |
        v
internal/sync/engine.go            ← Engine.piDirs field, syncAllLocked(), classifyOnePath(), processFile()
        |
        v
internal/sync/discovery.go         ← DiscoverPiSessions(), FindPiSourceFile()
        |
        v
internal/parser/types.go           ← AgentPi constant
        |
        v
internal/parser/pi.go              ← ParsePiSession(), extractPiContent(), formatPiToolCall()
        |
        v
internal/sync/engine.go            ← processPi(), FindSourceFile() pi: prefix case
        |
        v
internal/db/*                      ← No changes needed (agent stored as string column)
        |
        v
frontend/src/lib/utils/agents.ts   ← KNOWN_AGENTS entry for "pi"
        |
        v
frontend/src/App.svelte            ← agent-pi CSS class for badge color
```

### Reference: Gemini Integration Points

Tracing every file Gemini touches reveals the complete surface area:

| Layer | File | What Gemini Does There |
|-------|------|------------------------|
| Parser | `internal/parser/gemini.go` | `ParseGeminiSession()`, `extractGeminiContent()`, `formatGeminiToolCall()`, `GeminiSessionID()` |
| Parser types | `internal/parser/types.go` | `AgentGemini AgentType = "gemini"` constant |
| Taxonomy | `internal/parser/taxonomy.go` | Already handles Gemini tool names; no Gemini-specific additions needed (confirmed: pi tool names already present) |
| Discovery | `internal/sync/discovery.go` | `DiscoverGeminiSessions()`, `FindGeminiSourceFile()`, `confirmGeminiSessionID()`, `buildGeminiProjectMap()`, `resolveGeminiProject()` |
| Engine struct | `internal/sync/engine.go` | `geminiDirs []string` field |
| Engine constructor | `internal/sync/engine.go` | `geminiDirs` parameter to `NewEngine()` |
| Engine sync | `internal/sync/engine.go` | `syncAllLocked()` — discovery loop over geminiDirs, append to `all` |
| Engine classify | `internal/sync/engine.go` | `classifyOnePath()` — path routing for geminiDirs |
| Engine process | `internal/sync/engine.go` | `processGemini()` method, `processFile()` switch case |
| Engine source | `internal/sync/engine.go` | `FindSourceFile()` — "gemini:" prefix case |
| Engine single | `internal/sync/engine.go` | `SyncSingleSession()` — "gemini:" prefix case |
| Config struct | `internal/config/config.go` | `GeminiDir string`, `GeminiDirs []string` fields |
| Config default | `internal/config/config.go` | `Default()` — `GeminiDir: filepath.Join(home, ".gemini")` |
| Config env | `internal/config/config.go` | `loadEnv()` — `GEMINI_DIR` env var |
| Config file | `internal/config/config.go` | `loadFile()` — `gemini_dirs` JSON key |
| Config resolve | `internal/config/config.go` | `ResolveGeminiDirs()` method |
| Main wiring | `cmd/agentsview/main.go` | `warnMissingDirs(cfg.ResolveGeminiDirs(), "gemini")`, engine constructor arg |
| Main watcher | `cmd/agentsview/main.go` | `startFileWatcher()` — watches `gemini/tmp` subdir |
| Frontend agents | `frontend/src/lib/utils/agents.ts` | `{ name: "gemini", color: "var(--accent-rose)" }` in `KNOWN_AGENTS` |
| Frontend badge | `frontend/src/App.svelte` | `class:agent-gemini={session.agent === "gemini"}`, `.agent-gemini { background: var(--accent-rose); }` |
| Frontend filter | `frontend/src/lib/components/sidebar/SessionList.svelte` | Automatic — iterates `KNOWN_AGENTS` array |

---

## Recommended Architecture for Pi-Agent

Pi-agent follows Claude's directory layout (encoded-cwd subdirectories, JSONL files per session), not Gemini's. This simplifies discovery: `DiscoverPiSessions` is structurally identical to `DiscoverClaudeProjects` but without subagent subdirectory recursion and without the `agent-` prefix exclusion.

### Component Boundaries

| Component | Responsibility | Communicates With |
|-----------|---------------|-------------------|
| `internal/parser/pi.go` | Parse JSONL lines into `ParsedSession` + `[]ParsedMessage`; extract text, thinking blocks, tool calls from content arrays | `types.go` (types), `taxonomy.go` (NormalizeToolCategory) |
| `internal/parser/types.go` | Define `AgentPi` constant | Used by parser/pi.go, sync engine, discovery |
| `internal/sync/discovery.go` | `DiscoverPiSessions(dir)` walks encoded-cwd subdirs; `FindPiSourceFile(dir, id)` resolves session ID to path | `parser.AgentPi` |
| `internal/sync/engine.go` | Hold `piDirs []string`, route discovered files to `processPi()`, handle "pi:" session ID prefix | `discovery.go`, `parser/pi.go`, `db` |
| `internal/config/config.go` | `PiDir`/`PiDirs` fields, `PI_DIR` env var, `ResolvePiDirs()`, default path, JSON config key | Main entrypoint |
| `cmd/agentsview/main.go` | Wire config to engine, add watcher, add missing-dir warning | `sync.NewEngine`, `config` |
| `frontend/src/lib/utils/agents.ts` | Add `{ name: "pi", color: "var(--accent-teal)" }` to `KNOWN_AGENTS` | SessionList (filter buttons), App.svelte (badge) |
| `frontend/src/App.svelte` | Add `class:agent-pi` CSS class for session badge coloring | CSS variables from app.css |

### Data Flow

```
~/.pi/agent/sessions/
  -Documents-personal-misc-agentsview/
    2026-02-14T19-40-45-439Z_{id}.jsonl
          |
          | DiscoverPiSessions()
          v
    DiscoveredFile{Path, Project, Agent: AgentPi}
          |
          | Engine.processFile() -> processPi()
          v
    ParsePiSession(path, project, machine)
          |
          | returns ParsedSession + []ParsedMessage
          v
    writeBatch() -> db.UpsertSession() + db.InsertMessages()
          |
          | REST API /api/v1/sessions?agent=pi
          v
    Frontend filter UI (SessionList agent buttons)
          |
          | session.agent === "pi" badge CSS class
          v
    App.svelte session list badge
```

---

## Files to Create

| File | Purpose |
|------|---------|
| `internal/parser/pi.go` | Full pi-agent JSONL parser: session header, message types (text, thinking, toolCall, toolResult), model_change/thinking_level_change/compaction event handling, `PiSessionID()` helper |
| `internal/parser/pi_test.go` | Table-driven unit tests covering: session header parsing, user/assistant text messages, thinking blocks, tool calls in content arrays, tool results in user messages, compaction entries, model_change events, invalid/empty files |

---

## Files to Modify

| File | What Changes | Scope |
|------|-------------|-------|
| `internal/parser/types.go` | Add `AgentPi AgentType = "pi"` constant | 1 line |
| `internal/parser/taxonomy.go` | Verify pi tool names are covered. `read`, `write`, `edit`, `bash`, `grep`, `glob` are already present in `NormalizeToolCategory`. Add `find` -> `"Read"` if not present. | 1-2 lines |
| `internal/sync/discovery.go` | Add `DiscoverPiSessions(sessionsDir string) []DiscoveredFile` and `FindPiSourceFile(sessionsDir, sessionID string) string`. Pi layout mirrors Claude's encoded-cwd pattern: enumerate subdirectories, within each look for `*.jsonl` files matching `YYYY-MM-DDT..._{id}.jsonl` | ~60 lines |
| `internal/sync/engine.go` | (1) Add `piDirs []string` field to `Engine` struct; (2) add `piDirs []string` param to `NewEngine()`; (3) add pi discovery loop in `syncAllLocked()`; (4) add pi classification branch in `classifyOnePath()`; (5) add `case parser.AgentPi:` in `processFile()` switch; (6) add `processPi()` method; (7) add `"pi:"` prefix handling in `FindSourceFile()`; (8) add `"pi:"` prefix case in `SyncSingleSession()` | ~80 lines across existing functions |
| `internal/config/config.go` | (1) Add `PiDir string` and `PiDirs []string` fields to `Config` struct; (2) add default `~/.pi/agent/sessions` in `Default()`; (3) add `PI_DIR` env var in `loadEnv()`; (4) add `pi_dirs` JSON key in `loadFile()`; (5) add `ResolvePiDirs()` method | ~20 lines |
| `cmd/agentsview/main.go` | (1) Add `warnMissingDirs(cfg.ResolvePiDirs(), "pi")` in `runServe()`; (2) add `cfg.ResolvePiDirs()` to `sync.NewEngine(...)` call; (3) add pi dir to `startFileWatcher()` watch list; (4) update help text with `PI_DIR` env var and `pi_dirs` config key | ~15 lines |
| `frontend/src/lib/utils/agents.ts` | Add `{ name: "pi", color: "var(--accent-teal)" }` to `KNOWN_AGENTS` array. Note: `--accent-teal` does not exist yet — either add it to `app.css` or use `#0d9488` (teal-600) as an inline value. All existing accent slots are taken: blue=claude, green=codex, amber=copilot, rose=gemini, purple=opencode, black=cursor | 1 line (+ possible CSS var addition) |
| `frontend/src/App.svelte` | Add `class:agent-pi={session.agent === "pi"}` in the badge class list; add `.agent-pi { background: [teal value]; }` CSS rule | ~3 lines |
| `frontend/src/app.css` | Add `--accent-teal: #0d9488;` in `:root` block alongside the other accent variables | 1 line |

---

## Build Order and Dependencies

Dependencies flow strictly top-down. Each layer depends only on layers above it.

```
Step 1: internal/parser/types.go
        Add AgentPi constant.
        No dependencies.

Step 2: internal/parser/taxonomy.go
        Verify/add "find" -> "Read" mapping.
        Depends on: nothing (standalone).

Step 3: internal/parser/pi.go  +  internal/parser/pi_test.go
        Implement ParsePiSession, extractPiContent, formatPiToolCall, PiSessionID.
        Depends on: AgentPi (step 1), NormalizeToolCategory (step 2).

Step 4: internal/sync/discovery.go
        Implement DiscoverPiSessions, FindPiSourceFile.
        Depends on: AgentPi (step 1).

Step 5: internal/config/config.go
        Add PiDir/PiDirs fields, Default(), loadEnv(), loadFile(), ResolvePiDirs().
        Depends on: nothing (standalone config change).

Step 6: internal/sync/engine.go
        Add piDirs field, NewEngine param, syncAllLocked loop, classifyOnePath branch,
        processFile switch case, processPi method, FindSourceFile case, SyncSingleSession case.
        Depends on: AgentPi (step 1), ParsePiSession (step 3), DiscoverPiSessions (step 4).

Step 7: cmd/agentsview/main.go
        Wire ResolvePiDirs into engine constructor and file watcher.
        Depends on: ResolvePiDirs (step 5), NewEngine updated signature (step 6).

Step 8: frontend/src/app.css
        Add --accent-teal CSS variable.
        No backend dependencies.

Step 9: frontend/src/lib/utils/agents.ts
        Add pi entry to KNOWN_AGENTS.
        Depends on: --accent-teal (step 8).

Step 10: frontend/src/App.svelte
        Add agent-pi badge class and CSS rule.
        Depends on: --accent-teal (step 8).
```

---

## Patterns to Follow

### Pattern 1: JSONL Line-by-Line Parser with Typed Dispatch

All JSONL parsers (Claude, Codex) read line by line and dispatch on a `type` field. Pi follows the same pattern.

```go
// internal/parser/pi.go — sketch
func ParsePiSession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
    f, err := openNoFollow(path)
    // ...
    scanner := bufio.NewScanner(f)
    var header piSessionHeader
    // line 0: session header
    if scanner.Scan() {
        json.Unmarshal(scanner.Bytes(), &header)
    }
    // subsequent lines: typed events
    for scanner.Scan() {
        line := scanner.Bytes()
        eventType := gjson.GetBytes(line, "type").Str
        switch eventType {
        case "message":
            // parse role, content array
        case "model_change", "thinking_level_change", "compaction", "branch_summary":
            // skip or record metadata
        }
    }
    // build ParsedSession with ID "pi:"+header.ID
}
```

### Pattern 2: Encoded-CWD Directory Discovery

Pi mirrors Claude's layout. The discovery function enumerates subdirectories (encoded cwd paths), then JSONL files within. Filename pattern: `YYYY-MM-DDTHH-MM-SS-mmmZ_{uuid}.jsonl`.

```go
// internal/sync/discovery.go — sketch
func DiscoverPiSessions(sessionsDir string) []DiscoveredFile {
    entries, _ := os.ReadDir(sessionsDir)
    var files []DiscoveredFile
    for _, entry := range entries {
        if !isDirOrSymlink(entry, sessionsDir) { continue }
        projDir := filepath.Join(sessionsDir, entry.Name())
        project := parser.GetProjectName(entry.Name()) // reuse Claude's decoder
        sessionFiles, _ := os.ReadDir(projDir)
        for _, sf := range sessionFiles {
            if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".jsonl") { continue }
            files = append(files, DiscoveredFile{
                Path:    filepath.Join(projDir, sf.Name()),
                Project: project,
                Agent:   parser.AgentPi,
            })
        }
    }
    sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
    return files
}
```

### Pattern 3: Config Multi-Dir Resolution

Every agent follows the same single-field / multi-field / env-var pattern:

```go
// Config struct
PiDir  string   `json:"pi_dir"`
PiDirs []string `json:"pi_dirs,omitempty"`

// Default()
PiDir: filepath.Join(home, ".pi", "agent", "sessions"),

// loadEnv()
if v := os.Getenv("PI_DIR"); v != "" {
    c.PiDir = v
    c.PiDirs = []string{v}
}

// loadFile() — add to the file struct and copy block
PiDirs []string `json:"pi_dirs"`
// ...
if len(file.PiDirs) > 0 && c.PiDirs == nil {
    c.PiDirs = file.PiDirs
}

// Resolve method
func (c *Config) ResolvePiDirs() []string {
    return c.resolveDirs(c.PiDirs, c.PiDir)
}
```

### Pattern 4: Frontend Agent Registration

Adding an agent to `KNOWN_AGENTS` is the only change needed for filter button support. The `SessionList` component iterates the array automatically.

```typescript
// frontend/src/lib/utils/agents.ts
export const KNOWN_AGENTS: readonly AgentMeta[] = [
  // ... existing entries ...
  { name: "pi", color: "var(--accent-teal)" },
];
```

The badge in `App.svelte` requires explicit CSS class binding because it uses `class:` directives rather than dynamic styling:

```svelte
<!-- App.svelte — in the badge class list -->
class:agent-pi={session.agent === "pi"}
```

```css
/* App.svelte <style> */
.agent-pi {
  background: var(--accent-teal);
}
```

---

## Anti-Patterns to Avoid

### Anti-Pattern 1: Hardcoded Session ID Prefix Omission

**What goes wrong:** Adding pi discovery and parser but forgetting the `"pi:"` prefix cases in `FindSourceFile()` and `SyncSingleSession()` in engine.go.
**Consequence:** Session detail view fails to load, manual re-sync via API returns "source file not found".
**Prevention:** Search for every `"gemini:"` prefix occurrence in engine.go and replicate each one for `"pi:"`.

### Anti-Pattern 2: Skipping File Watcher Registration

**What goes wrong:** Sessions are discovered on startup sync but not watched for live changes.
**Consequence:** New pi sessions don't appear until the 15-minute periodic sync.
**Prevention:** Add pi dirs to the `roots` loop in `startFileWatcher()` in main.go. Pi files are directly in the encoded-cwd subdirectory — no subdirectory like Gemini's `tmp` is needed.

### Anti-Pattern 3: Missing CSS Variable

**What goes wrong:** Adding `"var(--accent-teal)"` to agents.ts without defining `--accent-teal` in app.css.
**Consequence:** Buttons and badges render with no color (transparent/inherit).
**Prevention:** Define `--accent-teal` in `:root` of `app.css` (both light and dark theme if applicable) before wiring the frontend.

### Anti-Pattern 4: Reusing an Occupied Accent Color

**What goes wrong:** Using an existing accent variable (e.g. `--accent-blue`) for pi because "there's a free one".
**Consequence:** Pi sessions visually indistinguishable from another agent in the filter UI and badge.
**Prevention:** Current color assignments: blue=claude, green=codex, amber=copilot, rose=gemini, purple=opencode, black=cursor, red=reserved. Pi must use a new variable. Teal (`#0d9488`) is unoccupied and visually distinct.

### Anti-Pattern 5: Wrong Session ID Prefix

**What goes wrong:** Using `"pi:" + rawID` where rawID already contains a prefix, or using a different string than what `FindSourceFile` expects.
**Consequence:** Session lookup failures, double-prefix IDs in the database.
**Prevention:** Session ID is `"pi:" + header.ID` (the UUID from the session header line). `FindSourceFile` strips `"pi:"` via `strings.TrimPrefix(sessionID, "pi:")` before searching.

---

## Scalability Considerations

Pi-agent sessions are JSONL files identical in size profile to Claude sessions. The existing worker pool (`maxWorkers = 8`, `batchSize = 100`) handles this without changes. Skip cache (mtime-based) prevents re-parsing unchanged files.

| Concern | Current approach | Pi behavior |
|---------|-----------------|-------------|
| Parse time | Worker pool, parallel | Same as Claude — no change needed |
| Skip detection | mtime + size in DB | `shouldSkipByPath()` — same as Gemini/Codex |
| Discovery | Directory walk | O(files) — same as Claude |
| Live updates | File watcher | Direct dir watch, same as Claude dirs |

---

## Sources

All findings are from direct code inspection of the agentsview codebase at commit `f397a93`. Confidence: HIGH — verified against actual source files.

| File | Confidence | How Used |
|------|-----------|----------|
| `internal/parser/gemini.go` | HIGH | Reference parser pattern |
| `internal/parser/types.go` | HIGH | Agent constant pattern |
| `internal/parser/taxonomy.go` | HIGH | Tool name mapping (pi tools already present) |
| `internal/sync/discovery.go` | HIGH | DiscoverGeminiSessions as discovery template |
| `internal/sync/engine.go` | HIGH | Full engine integration pattern |
| `internal/config/config.go` | HIGH | Config multi-dir pattern |
| `cmd/agentsview/main.go` | HIGH | Wiring and watcher pattern |
| `frontend/src/lib/utils/agents.ts` | HIGH | KNOWN_AGENTS registration |
| `frontend/src/lib/components/sidebar/SessionList.svelte` | HIGH | Filter UI — iterates KNOWN_AGENTS automatically |
| `frontend/src/App.svelte` | HIGH | Badge CSS class pattern |
| `frontend/src/app.css` | HIGH | Available accent color variables |
