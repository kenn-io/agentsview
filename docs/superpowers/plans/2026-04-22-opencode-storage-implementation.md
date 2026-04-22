# OpenCode Storage Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add transparent OpenCode support for both file-backed `storage/` sessions and legacy `opencode.db`, with file-backed storage preferred and live watcher support enabled.

**Architecture:** Treat OpenCode as file-first under the existing `OPENCODE_DIR` root. Add a source resolver that chooses file-backed `storage/` when present and SQLite only when storage is absent, then route discovery, parsing, single-session refresh, and watcher-triggered sync through that same source decision. Keep the external `opencode:<sessionID>` ID format unchanged so stored rows and frontend behavior do not churn.

**Tech Stack:** Go, fsnotify watcher pipeline, SQLite (`github.com/mattn/go-sqlite3`), existing agentsview parser/sync test suites

---

## File Map

### Existing files to modify

- `internal/parser/types.go`
  - Mark OpenCode as participating in the file-based pipeline and add watch subdirs for `storage/session`, `storage/message`, and `storage/part`.
- `internal/parser/discovery.go`
  - Add OpenCode source resolution, storage discovery, source lookup, and changed-path classification helpers.
- `internal/parser/opencode.go`
  - Add file-backed OpenCode parsing and unify shared message/part/session building logic where practical.
- `internal/parser/opencode_test.go`
  - Add file-backed parser fixtures and tests while keeping existing SQLite coverage intact.
- `internal/parser/types_test.go`
  - Update registry invariants and add resolver/discovery assertions.
- `internal/sync/engine.go`
  - Route OpenCode through the normal file-based path when storage is active, add `processOpenCode`, update `classifyOnePath`, and keep SQLite fallback behavior.
- `internal/sync/test_helpers_test.go`
  - Add a file-backed OpenCode fixture builder next to the existing SQLite helper.
- `internal/sync/engine_integration_test.go`
  - Add bulk sync, watch-path, and fallback integration coverage for file-backed OpenCode.

### New helper units expected inside existing files

- `internal/parser/discovery.go`
  - `OpenCodeSourceMode`
  - `ResolveOpenCodeSource`
  - `DiscoverOpenCodeSessions`
  - `FindOpenCodeSourceFile`
  - `ResolveOpenCodeSessionPath`
- `internal/parser/opencode.go`
  - `ParseOpenCodeStorageSession`
  - `ParseOpenCodeStorageFile`
  - file-backed row/helper structs for session/message/part JSON
- `internal/sync/engine.go`
  - `processOpenCode`
  - a small OpenCode branch in `classifyOnePath` that maps message/part changes back to the owning session file

---

### Task 1: Resolve OpenCode Source and Discovery

**Files:**
- Modify: `internal/parser/types.go`
- Modify: `internal/parser/discovery.go`
- Modify: `internal/parser/types_test.go`

- [ ] **Step 1: Write the failing discovery and resolver tests**

Add the following tests to `internal/parser/types_test.go` and `internal/parser/opencode_test.go`-adjacent discovery coverage if you prefer to colocate them there:

```go
func TestResolveOpenCodeSourcePrefersStorage(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	dbPath := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := ResolveOpenCodeSource(root)
	if got.Mode != OpenCodeSourceStorage {
		t.Fatalf("Mode = %v, want %v", got.Mode, OpenCodeSourceStorage)
	}
	if got.SessionRoot != filepath.Join(root, "storage", "session") {
		t.Fatalf("SessionRoot = %q", got.SessionRoot)
	}
}

func TestDiscoverOpenCodeSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "storage", "session", "global")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "ses_test.json")
	data := []byte(`{"id":"ses_test","directory":"/home/user/code/my-app"}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	got := DiscoverOpenCodeSessions(root)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Path != path {
		t.Fatalf("Path = %q, want %q", got[0].Path, path)
	}
	if got[0].Agent != AgentOpenCode {
		t.Fatalf("Agent = %q, want %q", got[0].Agent, AgentOpenCode)
	}
}

func TestFindOpenCodeSourceFilePrefersStorage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "storage", "session", "global", "ses_123.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"id":"ses_123"}`), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "opencode.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write db marker: %v", err)
	}

	got := FindOpenCodeSourceFile(root, "ses_123")
	if got != path {
		t.Fatalf("FindOpenCodeSourceFile() = %q, want %q", got, path)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/parser -run 'TestResolveOpenCodeSourcePrefersStorage|TestDiscoverOpenCodeSessions|TestFindOpenCodeSourceFilePrefersStorage' -count=1
```

Expected:

```text
FAIL
undefined: ResolveOpenCodeSource
undefined: DiscoverOpenCodeSessions
undefined: FindOpenCodeSourceFile
```

- [ ] **Step 3: Implement the resolver, discovery, and registry wiring**

Update `internal/parser/types.go` so OpenCode participates in file watching and source lookup:

```go
{
	Type:           AgentOpenCode,
	DisplayName:    "OpenCode",
	EnvVar:         "OPENCODE_DIR",
	ConfigKey:      "opencode_dirs",
	DefaultDirs:    []string{".local/share/opencode"},
	IDPrefix:       "opencode:",
	WatchSubdirs:   []string{"storage/session", "storage/message", "storage/part"},
	FileBased:      true,
	DiscoverFunc:   DiscoverOpenCodeSessions,
	FindSourceFunc: FindOpenCodeSourceFile,
},
```

Add source resolution and storage discovery helpers to `internal/parser/discovery.go`:

```go
type OpenCodeSourceMode string

const (
	OpenCodeSourceNone    OpenCodeSourceMode = ""
	OpenCodeSourceStorage OpenCodeSourceMode = "storage"
	OpenCodeSourceSQLite  OpenCodeSourceMode = "sqlite"
)

type OpenCodeSource struct {
	Mode        OpenCodeSourceMode
	Root        string
	SessionRoot string
	DBPath      string
}

func ResolveOpenCodeSource(root string) OpenCodeSource {
	sessionRoot := filepath.Join(root, "storage", "session")
	if st, err := os.Stat(sessionRoot); err == nil && st.IsDir() {
		return OpenCodeSource{
			Mode:        OpenCodeSourceStorage,
			Root:        root,
			SessionRoot: sessionRoot,
			DBPath:      filepath.Join(root, "opencode.db"),
		}
	}
	dbPath := filepath.Join(root, "opencode.db")
	if _, err := os.Stat(dbPath); err == nil {
		return OpenCodeSource{
			Mode:   OpenCodeSourceSQLite,
			Root:   root,
			DBPath: dbPath,
		}
	}
	return OpenCodeSource{Root: root}
}

func DiscoverOpenCodeSessions(root string) []DiscoveredFile {
	src := ResolveOpenCodeSource(root)
	if src.Mode != OpenCodeSourceStorage {
		return nil
	}
	var files []DiscoveredFile
	_ = filepath.WalkDir(src.SessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		files = append(files, DiscoveredFile{Path: path, Agent: AgentOpenCode})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func FindOpenCodeSourceFile(root, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}
	src := ResolveOpenCodeSource(root)
	if src.Mode == OpenCodeSourceStorage {
		var found string
		_ = filepath.WalkDir(src.SessionRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if filepath.Base(path) == sessionID+".json" {
				found = path
				return io.EOF
			}
			return nil
		})
		return found
	}
	if src.Mode == OpenCodeSourceSQLite {
		return src.DBPath + "#" + sessionID
	}
	return ""
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./internal/parser -run 'TestResolveOpenCodeSourcePrefersStorage|TestDiscoverOpenCodeSessions|TestFindOpenCodeSourceFilePrefersStorage|TestFileBasedAgentsHaveConfigKey' -count=1
```

Expected:

```text
ok  	github.com/wesm/agentsview/internal/parser
```

- [ ] **Step 5: Commit**

```bash
git add internal/parser/types.go internal/parser/discovery.go internal/parser/types_test.go
git commit -m "feat(parser): detect opencode storage source"
```

---

### Task 2: Add File-Backed OpenCode Parsing

**Files:**
- Modify: `internal/parser/opencode.go`
- Modify: `internal/parser/opencode_test.go`

- [ ] **Step 1: Write the failing file-backed parser tests**

Add these tests to `internal/parser/opencode_test.go`:

```go
func TestParseOpenCodeStorageSession_StandardSession(t *testing.T) {
	root := t.TempDir()
	sessionPath := writeOpenCodeStorageSession(t, root, "global", map[string]any{
		"id":        "ses_storage",
		"projectID": "global",
		"directory": "/home/user/code/myapp",
		"title":     "Test Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageMessage(t, root, "ses_storage", "msg_1", map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_storage",
		"role":      "user",
		"time":      map[string]any{"created": 1700000000000},
	})
	writeOpenCodeStoragePart(t, root, "msg_1", "prt_1", map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_storage",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "hello from storage",
		"time":      map[string]any{"start": 1700000000000, "end": 1700000000000},
	})

	sess, msgs, err := ParseOpenCodeStorageSession(sessionPath, "machine")
	if err != nil {
		t.Fatalf("ParseOpenCodeStorageSession: %v", err)
	}
	if sess == nil {
		t.Fatal("session is nil")
	}
	if sess.ID != "opencode:ses_storage" {
		t.Fatalf("ID = %q", sess.ID)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello from storage" {
		t.Fatalf("msgs = %#v", msgs)
	}
}

func TestParseOpenCodeStorageSession_ToolReasoningAndTokens(t *testing.T) {
	root := t.TempDir()
	sessionPath := writeOpenCodeStorageSession(t, root, "global", map[string]any{
		"id":        "ses_tools",
		"projectID": "global",
		"directory": "/tmp/proj",
		"title":     "",
		"time":      map[string]any{"created": 1700000000000, "updated": 1700000030000},
	})
	writeOpenCodeStorageMessage(t, root, "ses_tools", "msg_a", map[string]any{
		"id":        "msg_a",
		"sessionID": "ses_tools",
		"role":      "assistant",
		"modelID":   "claude-sonnet-4-5",
		"time":      map[string]any{"created": 1700000010000, "completed": 1700000012000},
		"tokens": map[string]any{
			"input": 12,
			"output": 34,
			"cache": map[string]any{"read": 56, "write": 78},
		},
	})
	writeOpenCodeStoragePart(t, root, "msg_a", "prt_r", map[string]any{
		"id": "prt_r", "sessionID": "ses_tools", "messageID": "msg_a",
		"type": "reasoning", "text": "Let me think",
		"time": map[string]any{"start": 1700000010000, "end": 1700000010500},
	})
	writeOpenCodeStoragePart(t, root, "msg_a", "prt_t", map[string]any{
		"id": "prt_t", "sessionID": "ses_tools", "messageID": "msg_a",
		"type": "tool", "callID": "call_1", "tool": "bash",
		"state": map[string]any{"input": map[string]any{"command": "pwd"}},
	})
	writeOpenCodeStoragePart(t, root, "msg_a", "prt_x", map[string]any{
		"id": "prt_x", "sessionID": "ses_tools", "messageID": "msg_a",
		"type": "text", "text": "done",
	})

	sess, msgs, err := ParseOpenCodeStorageSession(sessionPath, "m")
	if err != nil {
		t.Fatalf("ParseOpenCodeStorageSession: %v", err)
	}
	if sess == nil || len(msgs) != 1 {
		t.Fatalf("unexpected parse result: %#v %#v", sess, msgs)
	}
	if !msgs[0].HasThinking || !msgs[0].HasToolUse {
		t.Fatalf("message flags = %#v", msgs[0])
	}
	if len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ToolName != "bash" {
		t.Fatalf("tool calls = %#v", msgs[0].ToolCalls)
	}
	if !msgs[0].HasContextTokens || !msgs[0].HasOutputTokens {
		t.Fatalf("token flags = %#v", msgs[0])
	}
}
```

Add helper writers in the same test file:

```go
func writeOpenCodeStorageSession(t *testing.T, root, projectID string, data map[string]any) string {
	t.Helper()
	path := filepath.Join(root, "storage", "session", projectID, data["id"].(string)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	buf, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	return path
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/parser -run 'TestParseOpenCodeStorageSession_StandardSession|TestParseOpenCodeStorageSession_ToolReasoningAndTokens' -count=1
```

Expected:

```text
FAIL
undefined: ParseOpenCodeStorageSession
```

- [ ] **Step 3: Implement file-backed OpenCode parsing with shared normalization**

In `internal/parser/opencode.go`, add file-backed parsing entry points and reuse the existing normalization helpers where possible:

```go
type openCodeStorageSessionFile struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type openCodeStorageMessageFile struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ModelID   string `json:"modelID"`
	Provider  string `json:"providerID"`
	Time      struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type openCodeStoragePartFile struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	MessageID string          `json:"messageID"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Tool      string          `json:"tool"`
	CallID    string          `json:"callID"`
	State     json.RawMessage `json:"state"`
	Time      struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
}

func ParseOpenCodeStorageSession(sessionPath, machine string) (*ParsedSession, []ParsedMessage, error) {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read session %s: %w", sessionPath, err)
	}
	var sf openCodeStorageSessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, nil, fmt.Errorf("decode session %s: %w", sessionPath, err)
	}
	messageDir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))), "message", sf.ID)
	partRoot := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))), "part")
	return buildOpenCodeStorageSession(sf, messageDir, partRoot, sessionPath, machine)
}
```

Build messages by reading `storage/message/<sessionID>/*.json`, sorting by `time.created`, and collecting parts from `storage/part/<messageID>/*.json`. Preserve existing SQLite semantics:

```go
func buildOpenCodeStorageSession(
	sf openCodeStorageSessionFile,
	messageDir, partRoot, sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	msgFiles, err := filepath.Glob(filepath.Join(messageDir, "*.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("glob messages for %s: %w", sf.ID, err)
	}
	var rows []openCodeStorageMessageRow
	for _, path := range msgFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var mf openCodeStorageMessageFile
		if json.Unmarshal(raw, &mf) != nil {
			continue
		}
		rows = append(rows, openCodeStorageMessageRow{
			id: mf.ID, role: mf.Role, modelID: mf.ModelID,
			dataRaw: string(raw), timeCreated: mf.Time.Created,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].timeCreated < rows[j].timeCreated })

	var parsed []ParsedMessage
	var firstMsg string
	for ordinal, row := range rows {
		role := normalizeOpenCodeRole(row.role)
		if role == "" {
			continue
		}
		parts, err := loadOpenCodeStorageParts(partRoot, row.id)
		if err != nil {
			continue
		}
		pm := buildOpenCodeStorageMessage(ordinal, role, row.timeCreated, parts)
		applyOpenCodeTokenUsage(&pm, openCodeMessageData{Role: row.role, ModelID: row.modelID}, row.dataRaw)
		if strings.TrimSpace(pm.Content) == "" && !pm.HasToolUse {
			continue
		}
		if role == RoleUser && firstMsg == "" {
			firstMsg = truncate(strings.ReplaceAll(pm.Content, "\n", " "), 300)
		}
		parsed = append(parsed, pm)
	}
	if len(parsed) == 0 {
		return nil, nil, nil
	}
	if sf.Title != "" && !isOpenCodeDefaultTitle(sf.Title) {
		firstMsg = truncate(sf.Title, 300)
	}
	sess := &ParsedSession{
		ID:           "opencode:" + sf.ID,
		Project:      ExtractProjectFromCwd(sf.Directory),
		Machine:      machine,
		Agent:        AgentOpenCode,
		FirstMessage: firstMsg,
		StartedAt:    millisToTime(sf.Time.Created),
		EndedAt:      millisToTime(sf.Time.Updated),
		MessageCount: len(parsed),
		File:         FileInfo{Path: sessionPath, Mtime: mustStatMtime(sessionPath)},
	}
	if sf.ParentID != "" {
		sess.ParentSessionID = "opencode:" + sf.ParentID
	}
	accumulateMessageTokenUsage(sess, parsed)
	return sess, parsed, nil
}
```

- [ ] **Step 4: Run parser tests to verify they pass**

Run:

```bash
go test ./internal/parser -run 'TestParseOpenCodeDB_|TestParseOpenCodeStorageSession_' -count=1
```

Expected:

```text
ok  	github.com/wesm/agentsview/internal/parser
```

- [ ] **Step 5: Commit**

```bash
git add internal/parser/opencode.go internal/parser/opencode_test.go
git commit -m "feat(parser): parse file-backed opencode sessions"
```

---

### Task 3: Route OpenCode Through File Sync and Changed-Path Classification

**Files:**
- Modify: `internal/sync/engine.go`
- Modify: `internal/parser/discovery.go`

- [ ] **Step 1: Write the failing engine tests for file-backed OpenCode path classification**

Add targeted tests that prove changed `storage/message` and `storage/part` files map back to the session file:

```go
func TestFindOpenCodeSessionPathForMessageChange(t *testing.T) {
	root := t.TempDir()
	sessionPath := writeOpenCodeStorageSession(t, root, "global", map[string]any{
		"id": "ses_path_map", "projectID": "global", "directory": "/tmp/proj",
		"time": map[string]any{"created": 1, "updated": 2},
	})
	msgPath := writeOpenCodeStorageMessage(t, root, "ses_path_map", "msg_1", map[string]any{
		"id": "msg_1", "sessionID": "ses_path_map", "role": "user", "time": map[string]any{"created": 1},
	})

	got := ResolveOpenCodeSessionPath(root, msgPath)
	if got != sessionPath {
		t.Fatalf("ResolveOpenCodeSessionPath() = %q, want %q", got, sessionPath)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/parser -run 'TestFindOpenCodeSessionPathForMessageChange' -count=1
```

Expected:

```text
FAIL
undefined: ResolveOpenCodeSessionPath
```

- [ ] **Step 3: Implement changed-path mapping and file-based OpenCode processing**

Add an OpenCode branch to `internal/sync/engine.go`:

```go
func (e *Engine) processOpenCode(file parser.DiscoveredFile, info os.FileInfo) processResult {
	sess, msgs, err := parser.ParseOpenCodeStorageSession(file.Path, e.machine)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{skip: true}
	}
	return processResult{
		results: []parser.ParseResult{{
			Session:  *sess,
			Messages: msgs,
		}},
	}
}
```

Wire it into `processFile`:

```go
case parser.AgentOpenCode:
	res = e.processOpenCode(file, info)
```

Add OpenCode path classification in `classifyOnePath`:

```go
for _, openCodeDir := range e.agentDirs[parser.AgentOpenCode] {
	if openCodeDir == "" {
		continue
	}
	sessionPath := parser.ResolveOpenCodeSessionPath(openCodeDir, path)
	if sessionPath == "" {
		continue
	}
	return parser.DiscoveredFile{
		Path:  sessionPath,
		Agent: parser.AgentOpenCode,
	}, true
}
```

`ResolveOpenCodeSessionPath` in `internal/parser/discovery.go` should:

- return the session file directly for `storage/session/.../*.json`
- decode `sessionID` from changed message/part JSON
- map back to `storage/session/<projectID>/<sessionID>.json`

Use this concrete structure:

```go
func ResolveOpenCodeSessionPath(root, changedPath string) string {
	src := ResolveOpenCodeSource(root)
	if src.Mode != OpenCodeSourceStorage {
		return ""
	}
	if rel, ok := isUnder(src.SessionRoot, changedPath); ok {
		if strings.Count(rel, string(filepath.Separator)) == 1 && strings.HasSuffix(rel, ".json") {
			return changedPath
		}
	}

	var probe struct {
		SessionID string `json:"sessionID"`
	}
	raw, err := os.ReadFile(changedPath)
	if err != nil || json.Unmarshal(raw, &probe) != nil || probe.SessionID == "" {
		return ""
	}

	matches, err := filepath.Glob(filepath.Join(src.SessionRoot, "*", probe.SessionID+".json"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[0]
}
```

- [ ] **Step 4: Run the targeted parser and sync tests**

Run:

```bash
go test ./internal/parser -run 'TestFindOpenCodeSessionPathForMessageChange|TestDiscoverOpenCodeSessions' -count=1
go test ./internal/sync -run 'TestSyncEngineOpenCodeBulkSync|TestSyncEngineOpenCodeToolCallReplace' -count=1
```

Expected:

```text
ok  	github.com/wesm/agentsview/internal/parser
ok  	github.com/wesm/agentsview/internal/sync
```

- [ ] **Step 5: Commit**

```bash
git add internal/sync/engine.go internal/parser/discovery.go
git commit -m "feat(sync): watch and sync file-backed opencode sessions"
```

---

### Task 4: Preserve SQLite Fallback and Add Integration Coverage

**Files:**
- Modify: `internal/sync/test_helpers_test.go`
- Modify: `internal/sync/engine_integration_test.go`
- Modify: `internal/parser/opencode_test.go`

- [ ] **Step 1: Add file-backed OpenCode test helpers**

Extend `internal/sync/test_helpers_test.go` with a storage fixture builder:

```go
type openCodeStorageFixture struct {
	root string
}

func createOpenCodeStorage(t *testing.T, dir string) *openCodeStorageFixture {
	t.Helper()
	root := filepath.Join(dir, "storage")
	for _, sub := range []string{"session", "message", "part"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return &openCodeStorageFixture{root: root}
}
```

- [ ] **Step 2: Write failing integration tests for storage-first behavior**

Add these tests to `internal/sync/engine_integration_test.go`:

```go
func TestSyncEngineOpenCodeStorageBulkSync(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorage(t, env.opencodeDir)
	oc.addSession(t, "global", "ses_storage_1", "/home/user/code/myapp", "Storage Session", 1704067200000, 1704067205000)
	oc.addUserText(t, "ses_storage_1", "msg_u1", "hello storage", 1704067200000)
	oc.addAssistantText(t, "ses_storage_1", "msg_a1", "reply storage", 1704067200001, "claude-sonnet-4-5")

	env.engine.SyncAll(context.Background(), nil)

	assertSessionMessageCount(t, env.db, "opencode:ses_storage_1", 2)
	assertMessageContent(t, env.db, "opencode:ses_storage_1", "hello storage", "reply storage")
}

func TestSyncSingleOpenCodeStorageMessageChange(t *testing.T) {
	env := setupTestEnv(t)
	oc := createOpenCodeStorage(t, env.opencodeDir)
	oc.addSession(t, "global", "ses_storage_2", "/home/user/code/myapp", "", 1704067200000, 1704067205000)
	msgPath := oc.addAssistantText(t, "ses_storage_2", "msg_a1", "first", 1704067200001, "claude-sonnet-4-5")

	env.engine.SyncAll(context.Background(), nil)
	oc.replaceAssistantText(t, msgPath, "second")

	if err := env.engine.SyncSingleSession("opencode:ses_storage_2"); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}
	assertMessageContent(t, env.db, "opencode:ses_storage_2", "second")
}

func TestOpenCodeStorageWinsOverSQLite(t *testing.T) {
	env := setupTestEnv(t)
	ocDB := createOpenCodeDB(t, env.opencodeDir)
	ocDB.addProject(t, "proj-1", "/home/user/code/myapp")
	ocDB.addSession(t, "ses_dual", "proj-1", 1704067200000, 1704067205000)
	ocDB.addMessage(t, "msg_db_u1", "ses_dual", "user", 1704067200000)
	ocDB.addTextPart(t, "part_db_u1", "ses_dual", "msg_db_u1", "sqlite copy", 1704067200000)

	oc := createOpenCodeStorage(t, env.opencodeDir)
	oc.addSession(t, "global", "ses_dual", "/home/user/code/myapp", "Storage Wins", 1704067200000, 1704067205000)
	oc.addUserText(t, "ses_dual", "msg_storage_u1", "storage copy", 1704067200000)

	env.engine.SyncAll(context.Background(), nil)
	assertMessageContent(t, env.db, "opencode:ses_dual", "storage copy")
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```bash
go test ./internal/sync -run 'TestSyncEngineOpenCodeStorageBulkSync|TestSyncSingleOpenCodeStorageMessageChange|TestOpenCodeStorageWinsOverSQLite' -count=1
```

Expected:

```text
FAIL
undefined: createOpenCodeStorage
```

- [ ] **Step 4: Implement helper methods and keep SQLite fallback green**

Add fixture helpers that write the exact OpenCode storage layout the parser expects:

```go
func (oc *openCodeStorageFixture) addSession(
	t *testing.T, projectID, sessionID, directory, title string, created, updated int64,
) string {
	t.Helper()
	path := filepath.Join(oc.root, "session", projectID, sessionID+".json")
	payload := map[string]any{
		"id": sessionID,
		"projectID": projectID,
		"directory": directory,
		"title": title,
		"time": map[string]any{"created": created, "updated": updated},
	}
	writeJSONFixture(t, path, payload)
	return path
}
```

Do not remove existing SQLite tests. After the storage tests pass, rerun the existing OpenCode SQLite integration cases to prove fallback still works.

- [ ] **Step 5: Run the full relevant verification set**

Run:

```bash
go test ./internal/parser -run 'TestParseOpenCode' -count=1
go test ./internal/sync -run 'TestSyncEngineOpenCode|TestResyncAllOpenCodeOnly|TestResyncAllAbortsMixedSourceEmptyFiles|TestOpenCodeStorageWinsOverSQLite' -count=1
go test ./internal/parser ./internal/sync -count=1
```

Expected:

```text
ok  	github.com/wesm/agentsview/internal/parser
ok  	github.com/wesm/agentsview/internal/sync
```

- [ ] **Step 6: Commit**

```bash
git add internal/sync/test_helpers_test.go internal/sync/engine_integration_test.go internal/parser/opencode_test.go
git commit -m "test(sync): cover opencode storage and sqlite fallback"
```

---

## Self-Review

### Spec coverage

- Transparent `OPENCODE_DIR` auto-detection is covered in Task 1.
- File-backed parsing of sessions/messages/parts is covered in Task 2.
- Watch participation and changed-path single-session refresh are covered in Task 3.
- SQLite fallback and storage-first precedence are covered in Task 4.

### Placeholder scan

- No `TODO`, `TBD`, or “similar to previous task” placeholders remain.
- Each code-changing task includes concrete file paths, code blocks, commands, and commit messages.

### Type consistency

- Resolver names stay consistent: `ResolveOpenCodeSource`, `FindOpenCodeSourceFile`, `ResolveOpenCodeSessionPath`.
- Parser entry point stays consistent: `ParseOpenCodeStorageSession`.
- Sync entry point stays consistent: `processOpenCode`.
