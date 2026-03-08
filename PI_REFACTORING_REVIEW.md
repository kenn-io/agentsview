# Pi Support Refactoring Review

## Executive Summary

The Pi support has been successfully refactored to align with the new centralized agent registry pattern introduced in commit `d0f3abb`. All tests pass (Go: all packages, Frontend: 499 tests), and the implementation follows the established conventions for agent integration.

## Key Changes in Refactoring

### 1. Centralized Agent Registry (Aligned with Amp Pattern)

**Before:** Pi was integrated with hardcoded paths throughout the codebase
**After:** Pi is now registered in `parser.Registry` following the `AgentDef` pattern

```go
{
    Type:           AgentPi,
    DisplayName:    "Pi",
    EnvVar:         "PI_DIR",
    DefaultDirs:    []string{".pi/agent/sessions"},
    IDPrefix:       "pi:",              // NEW: Consistent prefixed session IDs
    FileBased:      true,
    DiscoverFunc:   DiscoverPiSessions,
    FindSourceFunc: FindPiSourceFile,
}
```

### 2. Session ID Prefixing (Critical Fix)

**Before:** Pi sessions used bare filenames (e.g., `pi-test-session-uuid`)
**After:** Pi sessions now use prefixed IDs (e.g., `pi:pi-test-session-uuid`)

This change:
- Enables proper `FindSourceFile` resolution via `AgentByPrefix`
- Aligns Pi with other agents like Codex (`codex:`), Copilot (`copilot:`), etc.
- Fixes the critical issue where `SyncSingleSession` couldn't locate Pi sessions

**Implementation in `internal/parser/pi.go:206`:**
```go
sess := &ParsedSession{
    ID: "pi:" + sessionID,  // Prefix added
    // ...
}
```

### 3. Engine Config Refactoring

**Before:** `NewEngine` took 9 positional parameters including separate `piDirs []string`
**After:** `NewEngine` uses `EngineConfig` with `AgentDirs map[AgentType][]string`

```go
engine := sync.NewEngine(database, sync.EngineConfig{
    AgentDirs: map[parser.AgentType][]string{
        parser.AgentPi: {piDir},
    },
    Machine: "local",
})
```

### 4. Discovery Functions Moved

**Before:** `DiscoverPiSessions` and `FindPiSourceFile` were in `internal/sync/discovery.go`
**After:** Functions moved to `internal/parser/discovery.go` and referenced via registry

This allows the engine to use `def.FindSourceFunc` generically for all agents.

## Architecture Compliance

### ✅ Registry-Driven `FindSourceFile`

The `FindSourceFile` implementation now properly handles Pi via the registry:

```go
func (e *Engine) FindSourceFile(sessionID string) string {
    def, ok := parser.AgentByPrefix(sessionID)
    if !ok || !def.FileBased || def.FindSourceFunc == nil {
        return ""
    }
    rawID := strings.TrimPrefix(sessionID, def.IDPrefix)
    for _, d := range e.agentDirs[def.Type] {
        if f := def.FindSourceFunc(d, rawID); f != "" {
            return f
        }
    }
    return ""
}
```

### ✅ Registry-Driven `SyncSingleSession`

Agent detection now uses `AgentByPrefix`:

```go
def, ok := parser.AgentByPrefix(sessionID)
if !ok {
    return fmt.Errorf("unknown agent for session %s", sessionID)
}
agent := def.Type
```

### ✅ Proper Prefix Stripping

`FindPiSourceFile` receives the raw session ID (prefix already stripped):

```go
// Called via def.FindSourceFunc(d, rawID) where rawID has no "pi:" prefix
func FindPiSourceFile(piDir, sessionID string) string {
    target := sessionID + ".jsonl"  // No prefix handling needed
    // ...
}
```

## Frontend Integration

### Agent Badge Support
- Pi entry in `KNOWN_AGENTS` with `var(--accent-teal)` color
- Label support: "Pi" (preserves case, not uppercased)
- CSS class: `.agent-pi` in `App.svelte`

### Tool Call Display
- Lowercase tool aliases handled (`bash`, `read`, `write`, `edit`, `grep`, `glob`, `find`)
- Pi-specific Edit format support (`edits[]` array with `op/pos/lines`)
- Path field variants (`path` vs `file_path`) supported
- `agent__intent` filtered from generic display

### Test Coverage
- `agents.test.ts`: 8 tests covering Pi color and label
- `content-parser.test.ts`: 81 tests including Pi tool aliases
- `tool-params.test.ts`: 48 tests including Pi Edit format

## Test Results

### Go Tests (All Passing)
```
ok  	github.com/wesm/agentsview/internal/config	0.289s
ok  	github.com/wesm/agentsview/internal/db	0.870s
ok  	github.com/wesm/agentsview/internal/insight	2.807s
ok  	github.com/wesm/agentsview/internal/parser	1.248s
ok  	github.com/wesm/agentsview/internal/server	7.699s
ok  	github.com/wesm/agentsview/internal/sync	2.086s
```

### Frontend Tests (All Passing)
```
Test Files  24 passed (24)
Tests       499 passed (499)
```

### Integration Test Validation

`TestPiSessionIntegration` validates:
1. Discovery: Finds Pi session files in `<piDir>/<encoded-cwd>/<session>.jsonl`
2. Parsing: Extracts messages, tool calls, thinking blocks
3. Session ID: Confirms `"pi:pi-test-session-uuid"` format in DB
4. Agent Type: Confirms `"pi"` agent field
5. FindSourceFile: Locates source via `"pi:"` prefixed ID
6. SyncSingleSession: Re-syncs Pi sessions correctly

## Critical Issues Resolved

### Issue 1: FindSourceFile Missing Pi (RESOLVED)
**Status:** Fixed by registry pattern
**Before:** Pi sessions weren't found because they had no prefix
**After:** `AgentByPrefix("pi:xxx")` returns Pi definition, enabling lookup

### Issue 2: SyncSingleSession Agent Detection (RESOLVED)
**Status:** Fixed by registry pattern
**Before:** Pi fell through to `AgentClaude` default case
**After:** `AgentByPrefix` correctly identifies Pi sessions

### Issue 3: Session ID Collision Risk (RESOLVED)
**Status:** Fixed by "pi:" prefix
**Before:** Bare session IDs could collide with Claude sessions
**After:** Prefixed IDs are namespaced (e.g., `pi:session-123`)

## Code Quality Assessment

| Aspect | Rating | Notes |
|--------|--------|-------|
| **Pattern Consistency** | 10/10 | Follows registry pattern identically to Amp |
| **Test Coverage** | 9/10 | Parser, discovery, integration tests all present |
| **Error Handling** | 9/10 | Graceful fallbacks in `AgentByPrefix` |
| **Documentation** | 8/10 | Good inline comments, tests document behavior |
| **Frontend Integration** | 9/10 | Proper badge, tool display, test coverage |
| **Backwards Compatibility** | 7/10 | Session IDs changed (requires DB refresh) |

## Migration Notes

### For Existing Pi Sessions
After deploying this change, existing Pi sessions in the database will have bare session IDs (no "pi:" prefix). To migrate:

1. **Option A (Recommended):** Clear DB and re-sync all sessions
   ```bash
   rm ~/.agentsview/sessions.db
   ./agentsview
   ```

2. **Option B:** Manual migration script to update session IDs
   ```sql
   UPDATE sessions SET id = 'pi:' || id WHERE agent = 'pi';
   ```

### Configuration Changes
No changes required. Pi continues to use:
- Env var: `PI_DIR`
- Config key: `pi_dirs` (if multi-directory support needed)
- Default: `~/.pi/agent/sessions`

## Comparison with Amp Integration

Both agents follow the identical pattern:

| Aspect | Amp | Pi |
|--------|-----|-----|
| Registry Entry | ✅ | ✅ |
| ID Prefix | `"amp:"` | `"pi:"` |
| File Discovery | `DiscoverAmpSessions` | `DiscoverPiSessions` |
| Find Source | `FindAmpSourceFile` | `FindPiSourceFile` |
| Custom Parser | `ParseAmpSession` | `ParsePiSession` |
| Frontend Badge | `accent-coral` | `accent-teal` |
| Frontend Label | "Amp" | "Pi" |

## Conclusion

The Pi support refactoring is **complete and well-implemented**. The transition to the centralized registry pattern:

1. **Fixes** the critical issues identified in the original review
2. **Aligns** Pi with the new standard agent integration pattern
3. **Improves** maintainability by reducing code duplication
4. **Maintains** full test coverage (499 frontend + comprehensive Go tests)

The implementation is production-ready and follows the same quality standards as the Amp integration.

## Action Items

None blocking. Optional follow-ups:
- [ ] Add DB migration script for existing Pi sessions (if preserving data is required)
- [ ] Document session ID format change in release notes
- [ ] Update user documentation for Pi configuration
