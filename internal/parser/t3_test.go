package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type t3TestMessage struct {
	id          string
	role        string
	text        string
	createdAt   string
	updatedAt   string
	attachments string
}

type t3TestThread struct {
	id             string
	projectID      string
	title          string
	createdAt      string
	updatedAt      string
	branch         string
	worktreePath   string
	modelSelection string
	legacyModel    string
	deletedAt      string
	providerName   string
	providerInst   string
	messages       []t3TestMessage
}

type t3TestProject struct {
	id            string
	title         string
	workspaceRoot string
	createdAt     string
	updatedAt     string
}

// t3TestDB describes a fixture database. legacy reproduces a pre-migration
// generation: projection_threads carries a bare model column instead of
// model_selection_json, messages have no attachments_json, and thread sessions
// have no provider_instance_id.
type t3TestDB struct {
	legacy   bool
	projects []t3TestProject
	threads  []t3TestThread
}

func createT3DB(t *testing.T, spec t3TestDB) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), t3DBName)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()

	threadModelCol := "model_selection_json TEXT"
	if spec.legacy {
		threadModelCol = "model TEXT NOT NULL DEFAULT ''"
	}
	stmts := []string{
		`CREATE TABLE projection_projects (
			project_id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			workspace_root TEXT NOT NULL,
			created_at TEXT,
			updated_at TEXT,
			deleted_at TEXT
		)`,
		`CREATE TABLE projection_threads (
			thread_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			title TEXT NOT NULL,
			branch TEXT,
			worktree_path TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			deleted_at TEXT,
			` + threadModelCol + `
		)`,
	}
	if spec.legacy {
		stmts = append(stmts,
			`CREATE TABLE projection_thread_messages (
				message_id TEXT PRIMARY KEY,
				thread_id TEXT NOT NULL,
				role TEXT NOT NULL,
				text TEXT NOT NULL,
				is_streaming INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE TABLE projection_thread_sessions (
				thread_id TEXT PRIMARY KEY,
				status TEXT,
				provider_name TEXT,
				updated_at TEXT
			)`,
		)
	} else {
		stmts = append(stmts,
			`CREATE TABLE projection_thread_messages (
				message_id TEXT PRIMARY KEY,
				thread_id TEXT NOT NULL,
				role TEXT NOT NULL,
				text TEXT NOT NULL,
				is_streaming INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				attachments_json TEXT
			)`,
			`CREATE TABLE projection_thread_sessions (
				thread_id TEXT PRIMARY KEY,
				status TEXT,
				provider_name TEXT,
				provider_instance_id TEXT,
				updated_at TEXT
			)`,
		)
	}
	for _, stmt := range stmts {
		_, err := conn.Exec(stmt)
		require.NoError(t, err)
	}

	for _, p := range spec.projects {
		_, err := conn.Exec(
			`INSERT INTO projection_projects
			   (project_id, title, workspace_root, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			p.id, p.title, p.workspaceRoot,
			nullableT3(p.createdAt), nullableT3(p.updatedAt))
		require.NoError(t, err)
	}

	for _, th := range spec.threads {
		modelValue := th.modelSelection
		if spec.legacy {
			modelValue = th.legacyModel
		}
		modelCol := "model_selection_json"
		if spec.legacy {
			modelCol = "model"
		}
		_, err := conn.Exec(
			`INSERT INTO projection_threads
			   (thread_id, project_id, title, branch, worktree_path,
			    created_at, updated_at, deleted_at, `+modelCol+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			th.id, th.projectID, th.title, th.branch, th.worktreePath,
			th.createdAt, th.updatedAt, nullableT3(th.deletedAt), modelValue)
		require.NoError(t, err)

		if th.providerName != "" {
			if spec.legacy {
				_, err = conn.Exec(
					`INSERT INTO projection_thread_sessions
					   (thread_id, status, provider_name) VALUES (?, 'idle', ?)`,
					th.id, th.providerName)
			} else {
				_, err = conn.Exec(
					`INSERT INTO projection_thread_sessions
					   (thread_id, status, provider_name, provider_instance_id)
					 VALUES (?, 'idle', ?, ?)`,
					th.id, th.providerName, th.providerInst)
			}
			require.NoError(t, err)
		}

		for _, m := range th.messages {
			updatedAt := m.updatedAt
			if updatedAt == "" {
				updatedAt = m.createdAt
			}
			if spec.legacy {
				_, err = conn.Exec(
					`INSERT INTO projection_thread_messages
					   (message_id, thread_id, role, text, is_streaming,
					    created_at, updated_at)
					 VALUES (?, ?, ?, ?, 0, ?, ?)`,
					m.id, th.id, m.role, m.text, m.createdAt, updatedAt)
			} else {
				_, err = conn.Exec(
					`INSERT INTO projection_thread_messages
					   (message_id, thread_id, role, text, is_streaming,
					    created_at, updated_at, attachments_json)
					 VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
					m.id, th.id, m.role, m.text, m.createdAt, updatedAt,
					m.attachments)
			}
			require.NoError(t, err)
		}
	}
	return dbPath
}

func nullableT3(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// parseT3All parses every live thread using the same per-thread primitives the
// provider uses.
func parseT3All(t *testing.T, dbPath, machine string) []ParseResult {
	t.Helper()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()

	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	var out []ParseResult
	for _, m := range metas {
		result, err := parseT3ThreadFromDB(
			context.Background(), conn, dbPath, m.RawID, machine, info)
		require.NoError(t, err)
		if result != nil {
			out = append(out, *result)
		}
	}
	return out
}

func t3SampleDB(legacy bool) t3TestDB {
	return t3TestDB{
		legacy: legacy,
		projects: []t3TestProject{{
			id:            "proj-1",
			title:         "my-app",
			workspaceRoot: "/Users/alice/code/my-app",
		}},
		threads: []t3TestThread{{
			id:        "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55",
			projectID: "proj-1",
			title:     "Fix the flaky login test",
			createdAt: "2026-08-22T22:53:02.918Z",
			updatedAt: "2026-08-22T23:14:17.327Z",
			branch:    "fix/login-flake",
			modelSelection: `{"instanceId":"claudeAgent","model":"claude-opus-4-8",` +
				`"options":[{"id":"effort","value":"high"}]}`,
			legacyModel:  "claude-opus-4-8",
			providerName: "claudeAgent",
			providerInst: "claudeAgent",
			messages: []t3TestMessage{
				{
					id: "m-1", role: "user",
					text:      "the login test fails\nabout one run in five",
					createdAt: "2026-08-22T22:53:03.001Z",
				},
				{
					id: "m-2", role: "assistant",
					text:      "Found it: the fixture races the session cookie.",
					createdAt: "2026-08-22T22:55:10.500Z",
				},
				{
					id: "m-3", role: "user",
					text:      "ship it",
					createdAt: "2026-08-22T23:14:17.100Z",
				},
			},
		}},
	}
}

func TestParseT3Threads_CurrentSchema(t *testing.T) {
	dbPath := createT3DB(t, t3SampleDB(false))
	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)

	sess := results[0].Session
	assert.Equal(t, "t3:70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55", sess.ID)
	assert.Equal(t, AgentT3, sess.Agent)
	assert.Equal(t, "testbox", sess.Machine)
	assert.Equal(t, "Fix the flaky login test", sess.SessionName)
	assert.Equal(t, "/Users/alice/code/my-app", sess.Cwd)
	assert.Equal(t, ExtractProjectFromCwd("/Users/alice/code/my-app"), sess.Project)
	assert.Equal(t, "fix/login-flake", sess.GitBranch)
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, 2, sess.UserMessageCount)
	// Newlines are flattened so the preview stays one line.
	assert.Equal(t, "the login test fails about one run in five", sess.FirstMessage)
	assert.Equal(t, T3VirtualPath(dbPath, "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55"),
		sess.File.Path)

	messages := results[0].Messages
	require.Len(t, messages, 3)
	assert.Equal(t, []RoleType{RoleUser, RoleAssistant, RoleUser},
		[]RoleType{messages[0].Role, messages[1].Role, messages[2].Role})
	for i, msg := range messages {
		assert.Equal(t, i, msg.Ordinal)
		assert.Equal(t, len(msg.Content), msg.ContentLength)
		assert.False(t, msg.Timestamp.IsZero())
	}
	// The model is attributed to assistant turns only.
	assert.Equal(t, "claude-opus-4-8", messages[1].Model)
	assert.Empty(t, messages[0].Model)
	assert.Empty(t, messages[2].Model)
}

// A pre-canonicalization database has no model_selection_json,
// attachments_json, or provider_instance_id. It must still parse, with the
// model read from the legacy column.
func TestParseT3Threads_LegacySchema(t *testing.T) {
	dbPath := createT3DB(t, t3SampleDB(true))
	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, "Fix the flaky login test", results[0].Session.SessionName)
	require.Len(t, results[0].Messages, 3)
	assert.Equal(t, "claude-opus-4-8", results[0].Messages[1].Model)
}

func TestParseT3Threads_SkipsDeletedThreads(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads = append(spec.threads, t3TestThread{
		id:        "9c2f0f2a-1111-4222-8333-444455556666",
		projectID: "proj-1",
		title:     "abandoned",
		createdAt: "2026-08-21T10:00:00.000Z",
		updatedAt: "2026-08-21T10:05:00.000Z",
		deletedAt: "2026-08-21T11:00:00.000Z",
		messages: []t3TestMessage{{
			id: "d-1", role: "user", text: "never mind",
			createdAt: "2026-08-21T10:00:01.000Z",
		}},
	})
	dbPath := createT3DB(t, spec)

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, "Fix the flaky login test", results[0].Session.SessionName)

	assert.False(t, T3ThreadExists(dbPath, "9c2f0f2a-1111-4222-8333-444455556666"))
	assert.True(t, T3ThreadExists(dbPath, "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55"))
}

// The thread's own updated_at is the primary change signal, but a message
// written after it must still move the thread's mtime -- otherwise a writer
// that stops bumping the thread row would freeze change detection.
func TestT3ThreadMetaTracksNewestActivity(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads[0].updatedAt = "2026-08-22T22:56:00.000Z"
	spec.threads[0].messages = append(spec.threads[0].messages, t3TestMessage{
		id: "m-4", role: "assistant", text: "done",
		createdAt: "2026-08-22T23:30:00.000Z",
	})
	dbPath := createT3DB(t, spec)

	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	require.Len(t, metas, 1)

	want := parseTimestamp("2026-08-22T23:30:00.000Z").UnixNano()
	assert.Equal(t, want, metas[0].FileMtime)

	// The watcher's token carries the same millisecond timestamp, with the
	// digest folded into the otherwise-unused sub-millisecond bits.
	token, err := T3SourceMtime(metas[0].VirtualPath)
	require.NoError(t, err)
	assert.Equal(t, want, token-token%int64(time.Millisecond))

	// A message past the thread's updated_at also ends the session there --
	// otherwise activity ordering and date windows would go stale for exactly
	// the writes the change token was built to see.
	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, want, results[0].Session.EndedAt.UnixNano())
}

// An in-place edit bumps only the message's updated_at. The discovery token
// and the parse-persisted mtime must both advance to that stamp -- if they
// diverged, the reparse the fingerprint triggers would be discarded as
// unchanged and the edit lost.
func TestT3ChangeTokenSeesInPlaceMessageEdits(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads[0].messages[1].updatedAt = "2026-08-23T09:00:00.000Z"
	dbPath := createT3DB(t, spec)

	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	require.Len(t, metas, 1)

	want := parseTimestamp("2026-08-23T09:00:00.000Z").UnixNano()
	assert.Equal(t, want, metas[0].FileMtime, "discovery token")

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, want, results[0].Session.File.Mtime,
		"the parse must persist the same change token discovery computed")
	// An edit is conversation activity, so it also moves the session's end.
	assert.Equal(t, want, results[0].Session.EndedAt.UnixNano())
}

// A workspace-root change surfaces only through the project row's updated_at,
// so the change token must include the project timestamps on both sides.
func TestT3ChangeTokenSeesProjectChanges(t *testing.T) {
	spec := t3SampleDB(false)
	spec.projects[0].updatedAt = "2026-08-23T10:30:00.000Z"
	dbPath := createT3DB(t, spec)

	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	require.Len(t, metas, 1)

	want := parseTimestamp("2026-08-23T10:30:00.000Z").UnixNano()
	assert.Equal(t, want, metas[0].FileMtime, "discovery token")

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, want, results[0].Session.File.Mtime,
		"the parse must persist the same change token discovery computed")
	// A workspace rename is not conversation activity: it advances the change
	// token but must not move the session's end.
	assert.Equal(t, parseTimestamp(spec.threads[0].updatedAt).UnixNano(),
		results[0].Session.EndedAt.UnixNano())
}

// The dropped-row rule: an empty message never reaches the transcript, but its
// timestamps still count toward the change token because the discovery query
// aggregates over every row.
func TestT3ChangeTokenIncludesDroppedMessages(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads[0].messages = append(spec.threads[0].messages, t3TestMessage{
		id: "m-empty", role: "assistant", text: "  ",
		createdAt: "2026-08-23T11:00:00.000Z", attachments: "[]",
	})
	dbPath := createT3DB(t, spec)

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Len(t, results[0].Messages, 3, "empty message stays dropped")
	want := parseTimestamp("2026-08-23T11:00:00.000Z").UnixNano()
	assert.Equal(t, want, results[0].Session.File.Mtime)

	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, want, metas[0].FileMtime)
}

// t3 is event-sourced: a projection rebuild can rewrite content while every
// event-derived timestamp stands still. The digest must move when the content
// does, on both the discovery and parse sides, while the mtime token stays
// put -- that pair is exactly what UnchangedResultMTimeAndHash consumes.
func TestT3DigestSeesSameTimestampRewrite(t *testing.T) {
	dbPath := createT3DB(t, t3SampleDB(false))

	tokenOf := func() (int64, string, string) {
		t.Helper()
		conn, err := OpenT3DB(dbPath)
		require.NoError(t, err)
		defer conn.Close()
		metas, err := ListT3ThreadMetas(conn, dbPath)
		require.NoError(t, err)
		require.Len(t, metas, 1)
		results := parseT3All(t, dbPath, "testbox")
		require.Len(t, results, 1)
		// The invariant everything hangs on: discovery and parse agree.
		assert.Equal(t, metas[0].FileMtime, results[0].Session.File.Mtime)
		assert.Equal(t, metas[0].Fingerprint, results[0].Session.File.Hash)
		return metas[0].FileMtime, metas[0].Fingerprint, results[0].Messages[1].Content
	}

	mtimeBefore, hashBefore, _ := tokenOf()
	require.NotEmpty(t, hashBefore)

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'Refolded: the cookie race was in the fixture.'
		  WHERE message_id = 'm-2'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	mtimeAfter, hashAfter, content := tokenOf()
	assert.Equal(t, mtimeBefore, mtimeAfter,
		"no timestamp moved, so the mtime token must not either")
	assert.NotEqual(t, hashBefore, hashAfter,
		"the digest is the only signal that can see this rewrite")
	assert.Equal(t, "Refolded: the cookie race was in the fixture.", content)
}

// The live watcher compares T3SourceMtime for inequality, so a rewrite whose
// timestamps do not move must still change the returned token -- the digest
// in the sub-millisecond bits is what carries it.
func TestT3WatchTokenSeesSameTimestampRewrite(t *testing.T) {
	dbPath := createT3DB(t, t3SampleDB(false))
	vpath := T3VirtualPath(dbPath, "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55")

	before, err := T3SourceMtime(vpath)
	require.NoError(t, err)

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec(
		`UPDATE projection_thread_messages
		    SET text = 'Refolded with identical timestamps.'
		  WHERE message_id = 'm-2'`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	after, err := T3SourceMtime(vpath)
	require.NoError(t, err)
	assert.NotEqual(t, before, after,
		"the watch token must move when only content did")
	assert.Equal(t,
		before-before%int64(time.Millisecond),
		after-after%int64(time.Millisecond),
		"the millisecond part is the unchanged timestamp")
}

func TestT3AttachmentPlaceholder(t *testing.T) {
	assert.Equal(t, "[Image: shot.webp]",
		t3AttachmentPlaceholder(`[{"type":"image","name":"shot.webp"}]`))
	assert.Equal(t, "[Image]",
		t3AttachmentPlaceholder(`[{"type":"image"}]`))
	assert.Equal(t, "[Attachment: notes.pdf]",
		t3AttachmentPlaceholder(`[{"type":"file","name":"notes.pdf"}]`))
	assert.Equal(t, "[Image: a.png]\n[Attachment]",
		t3AttachmentPlaceholder(
			`[{"type":"image","name":"a.png"},{"type":"file"}]`))
	assert.Equal(t, "[Attachment]", t3AttachmentPlaceholder(`not json`))
}

// The watermark listing is the cheap half of change detection: it must agree
// with the full meta scan on every thread's token and stream in ascending
// virtual-path order, or the stored-freshness merge would silently skip or
// double-emit members.
func TestT3ThreadWatermarksMatchMetaTokens(t *testing.T) {
	spec := t3SampleDB(false)
	spec.projects[0].updatedAt = "2026-08-23T10:30:00.000Z"
	spec.threads = append(spec.threads, t3TestThread{
		id: "0a000000-0000-4000-8000-000000000001", projectID: "proj-1",
		title: "second", createdAt: "2026-08-21T09:00:00.000Z",
		updatedAt: "2026-08-21T09:05:00.000Z",
		messages: []t3TestMessage{{
			id: "s-1", role: "user", text: "hello",
			createdAt: "2026-08-21T09:00:01.000Z",
		}},
	})
	dbPath := createT3DB(t, spec)

	conn, err := OpenT3DB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	shape, err := inspectT3Schema(context.Background(), conn)
	require.NoError(t, err)

	want := map[string]int64{}
	metas, err := ListT3ThreadMetas(conn, dbPath)
	require.NoError(t, err)
	for _, m := range metas {
		want[m.VirtualPath] = m.FileMtime
	}

	var paths []string
	got := map[string]int64{}
	err = forEachT3ThreadWatermark(context.Background(), conn, shape, dbPath,
		func(_, virtualPath string, watermarkNS int64) error {
			paths = append(paths, virtualPath)
			got[virtualPath] = watermarkNS
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.IsIncreasing(t, paths)
}

func TestT3ContainerPathForEvent(t *testing.T) {
	dbPath := createT3DB(t, t3SampleDB(false))
	root := filepath.Dir(dbPath)
	assert.Equal(t, dbPath, T3ContainerPathForEvent(root, dbPath))
	assert.Equal(t, dbPath, T3ContainerPathForEvent(root, dbPath+"-wal"))
	assert.Equal(t, dbPath, T3ContainerPathForEvent(root, dbPath+"-shm"))
	assert.Empty(t, T3ContainerPathForEvent(root, filepath.Join(root, "other.db")))
	assert.Empty(t, T3ContainerPathForEvent("", dbPath))
}

func TestT3BatchMemberPresent(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads = append(spec.threads, t3TestThread{
		id: "9c2f0f2a-1111-4222-8333-444455556666", projectID: "proj-1",
		title: "gone", createdAt: "2026-08-21T10:00:00.000Z",
		updatedAt: "2026-08-21T10:05:00.000Z",
		deletedAt: "2026-08-21T11:00:00.000Z",
	})
	dbPath := createT3DB(t, spec)
	container := multiSessionSource{Container: dbPath}
	member := func(id string) multiSessionSource {
		return multiSessionSource{
			Container: dbPath, MemberID: id, Path: T3VirtualPath(dbPath, id),
		}
	}
	live := member("70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55")
	deleted := member("9c2f0f2a-1111-4222-8333-444455556666")
	missing := member("00000000-0000-4000-8000-000000000000")

	present := t3BatchMemberPresent(
		container, []multiSessionSource{live, deleted, missing})
	assert.True(t, present[live.Path])
	assert.False(t, present[deleted.Path], "soft-deleted thread is absent")
	assert.False(t, present[missing.Path])

	// An unreadable database reports every member present so a locked or
	// vanished container never tombstones the archive.
	broken := multiSessionSource{
		Container: filepath.Join(t.TempDir(), "missing", t3DBName),
	}
	present = t3BatchMemberPresent(broken, []multiSessionSource{live, deleted})
	assert.True(t, present[live.Path])
	assert.True(t, present[deleted.Path])
}

func TestT3VirtualPathRoundTrip(t *testing.T) {
	dbPath := filepath.Join("/tmp", "userdata", t3DBName)
	virtual := T3VirtualPath(dbPath, "thread-a")
	gotDB, gotID, ok := parseT3VirtualPath(virtual)
	require.True(t, ok)
	assert.Equal(t, dbPath, gotDB)
	assert.Equal(t, "thread-a", gotID)

	// A path whose container is not state.sqlite is not a t3 source.
	_, _, ok = parseT3VirtualPath(filepath.Join("/tmp", "other.db") + "#thread-a")
	assert.False(t, ok)
}

// A worktree thread works in its own checkout; the project's workspace_root is
// the repository it was cut from, so the worktree wins as the cwd.
func TestParseT3Thread_WorktreePathWinsOverWorkspaceRoot(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads[0].worktreePath = "/Users/alice/code/my-app-worktrees/login"
	dbPath := createT3DB(t, spec)

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Equal(t, "/Users/alice/code/my-app-worktrees/login",
		results[0].Session.Cwd)
}

// A thread whose project row is gone is still a real conversation; it just
// loses the cwd.
func TestParseT3Thread_ToleratesMissingProject(t *testing.T) {
	spec := t3SampleDB(false)
	spec.projects = nil
	dbPath := createT3DB(t, spec)

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.Cwd)
	assert.Equal(t, "unknown", results[0].Session.Project)
	assert.Len(t, results[0].Messages, 3)
}

// An image-only message carries no text but is still a turn the user took.
// A message that is empty in every respect is not.
func TestParseT3Thread_MessageEmptinessRules(t *testing.T) {
	spec := t3SampleDB(false)
	spec.threads[0].messages = append(spec.threads[0].messages,
		t3TestMessage{
			id: "m-img", role: "user", text: "   ",
			createdAt:   "2026-08-22T23:20:00.000Z",
			attachments: `[{"type":"image","name":"screenshot.webp"}]`,
		},
		t3TestMessage{
			id: "m-empty", role: "assistant", text: "",
			createdAt: "2026-08-22T23:21:00.000Z", attachments: "[]",
		},
	)
	dbPath := createT3DB(t, spec)

	results := parseT3All(t, dbPath, "testbox")
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 4)
	// The image-only turn renders a placeholder instead of a blank message.
	assert.Equal(t, "[Image: screenshot.webp]", results[0].Messages[3].Content)
	assert.Equal(t, len("[Image: screenshot.webp]"),
		results[0].Messages[3].ContentLength)
	assert.Equal(t, RoleUser, results[0].Messages[3].Role)
	// Ordinals stay contiguous after the dropped message.
	for i, msg := range results[0].Messages {
		assert.Equal(t, i, msg.Ordinal)
	}
}

func TestInspectT3Schema_RejectsIncompatibleDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), t3DBName)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	// projection_threads without the timestamps the parser requires.
	_, err = conn.Exec(`CREATE TABLE projection_threads (
		thread_id TEXT PRIMARY KEY, title TEXT)`)
	require.NoError(t, err)

	_, err = inspectT3Schema(context.Background(), conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "projection_threads")
}

func TestInspectT3Schema_RejectsDatabaseWithoutProjections(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), t3DBName)
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE unrelated (id TEXT)`)
	require.NoError(t, err)

	_, err = inspectT3Schema(context.Background(), conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing t3 projection_threads table")
}

func TestT3ProviderDiscoversThreadsInSharedDatabase(t *testing.T) {
	spec := t3SampleDB(false)
	dbPath := createT3DB(t, spec)
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentT3, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	// Discovery surfaces the shared database once; Parse fans it out into one
	// session per thread.
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentT3, sources[0].Provider)
	assert.Equal(t, dbPath, sources[0].DisplayPath)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: sources[0], Machine: "testbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "t3:70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55", sess.ID)
	assert.Len(t, outcome.Results[0].Result.Messages, 3)
	// Each fanned-out session is addressed by its own virtual member path, so
	// one thread's change never invalidates its neighbours.
	assert.Equal(t,
		T3VirtualPath(dbPath, "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55"),
		sess.File.Path)

	// A raw thread ID resolves back to exactly that thread's member path, and
	// RequireFreshSource proves the row is still there rather than returning a
	// tombstone.
	ref, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:       "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55",
		FullSessionID:      "t3:70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t,
		T3VirtualPath(dbPath, "70d3aebd-8924-4ad9-9c1a-2f1b6d1f4a55"),
		ref.DisplayPath)

	// A deleted thread must not resolve.
	_, found, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:       "00000000-0000-4000-8000-000000000000",
		FullSessionID:      "t3:00000000-0000-4000-8000-000000000000",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	assert.False(t, found)

	// A WAL sibling event resolves back to the container.
	refs, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: dbPath + "-wal", WatchRoot: root},
	)
	require.NoError(t, err)
	require.NotEmpty(t, refs)
}

// The transcripts share a directory with t3's credential files, so no part of
// the root may reach a remote sync artifact.
func TestT3RootIsExcludedFromRemoteSync(t *testing.T) {
	assert.True(t, RemoteSyncExcludedAgent(AgentT3))
	def, ok := AgentByType(AgentT3)
	require.True(t, ok)
	assert.False(t, def.FileBased)
	assert.Equal(t, "t3:", def.IDPrefix)
	assert.Equal(t, []string{".t3/userdata", ".t3/dev"}, def.DefaultDirs)
}
