package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAugureDesktopStateDB builds a minimal Hermes-schema state.db under
// an .augure-desktop root: one session with a user message, an assistant
// tool-call message, a tool result row, and realistic token/cost columns.
// The estimated cost is a real present-zero (0, not NULL) to guard the
// nullable-float decode treating 0 as absent.
func createAugureDesktopStateDB(t *testing.T, root string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			model TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			message_count INTEGER DEFAULT 0,
			tool_call_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			tool_call_id TEXT,
			tool_calls TEXT,				tool_name TEXT,
				timestamp REAL NOT NULL,
				finish_reason TEXT,
				reasoning TEXT,
				reasoning_content TEXT,
				reasoning_details TEXT,
				codex_reasoning_items TEXT,
				codex_message_items TEXT
			);
		CREATE TABLE session_model_usage (
			session_id TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT
		);
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (26);
		INSERT INTO sessions (
			id, source, model, started_at, ended_at, message_count,				tool_call_count, input_tokens, output_tokens, cache_read_tokens,
				cache_write_tokens, reasoning_tokens, estimated_cost_usd,
				actual_cost_usd, cost_status, cost_source, title, api_call_count
		) VALUES (
			'20260910_075655_ca54ab', 'desktop', 'ossington-5',
			1789094215.0, 1789094500.0, 3, 1, 5200, 310, 4100, 250, 90,
			NULL, 0.0, 'estimated', 'augure', 'Augure desktop session', 3
		);
		INSERT INTO messages (session_id, role, content, timestamp) VALUES
			('20260910_075655_ca54ab', 'user', 'List the parser files.',
				1789094216.0),
			('20260910_075655_ca54ab', 'assistant', 'Running ls.',
				1789094220.0),
			('20260910_075655_ca54ab', 'tool', 'codex.go\nprovider.go\n',
				1789094225.0);
		UPDATE messages SET tool_call_id = 'call_t1', tool_name = 'terminal'
			WHERE role = 'tool';
		INSERT INTO session_model_usage (
			session_id, model, input_tokens, output_tokens, cost_status,
			cost_source
		) VALUES (
			'20260910_075655_ca54ab', 'ossington-5', 5200, 310,
			'estimated', 'augure'
		);
	`)
	require.NoError(t, err)
	return filepath.Join(root, "state.db")
}

// TestAugureDesktopProviderParsesStateDB runs the full Discover -> Parse
// path over a fixture built under an .augure-desktop root, guarding the
// spec seam: augure-desktop: IDs, Hermes-schema parsing, and usage events
// carrying the state DB's authoritative model and cost.
func TestAugureDesktopProviderParsesStateDB(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".augure-desktop")
	require.NoError(t, os.MkdirAll(root, 0o755))
	stateDB := createAugureDesktopStateDB(t, root)

	provider, ok := NewProvider(AgentAugureDesktop, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, AgentAugureDesktop, sources[0].Provider)
	assert.Equal(t, stateDB, sources[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  sources[0],
		Machine: "devbox",
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	sess := result.Session

	const id = "20260910_075655_ca54ab"
	assert.Equal(t, AgentAugureDesktop, sess.Agent)
	assert.Equal(t, "augure-desktop:"+id, sess.ID)
	assert.Equal(t, "Augure desktop session", sess.SessionName)
	// SourceSessionID keeps the raw store id for raw lookups.
	assert.Equal(t, id, sess.SourceSessionID)
	assert.Equal(t, "devbox", sess.Machine)
	// The mechanical project rewrite swaps the hermes prefix for the fork
	// name; the desktop source keeps its own suffix.
	assert.Equal(t, "augure-desktop-desktop", sess.Project)

	// REAL-epoch seconds decode to UTC times.
	assert.Equal(
		t, time.Unix(1789094215, 0).UTC(), sess.StartedAt.UTC(),
	)
	assert.True(t, sess.EndedAt.After(sess.StartedAt))

	// State-DB aggregates land on the session.
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 310, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 5200+4100, sess.PeakContextTokens)

	// Usage events carry the raw store's session ID rewritten onto the
	// augure-desktop: namespace, plus the recorded model. actual_cost_usd = 0
	// (SQL 0, not NULL) decodes as a present-zero cost: the nullable-float
	// decode keeps 0 and NULL distinct, so a real recorded $0 survives. An
	// estimated 0 deliberately does not masquerade as $0, matching stock
	// Hermes.
	require.NotEmpty(t, result.UsageEvents)
	event := result.UsageEvents[0]
	assert.Equal(t, "augure-desktop:"+id, event.SessionID)
	assert.Equal(t, "ossington-5", event.Model)
	require.NotNil(t, event.Cost)
	assert.Equal(t, "estimated", event.CostStatus)
	assert.Zero(t, event.Cost.Microdollars,
		"actual_cost_usd = 0 must decode as present-zero, not absent")

	require.NotEmpty(t, result.Messages)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "List the parser files.", result.Messages[0].Content)
}

// TestAugureDesktopAcceptsConfiguredCustomRoot pins the reviewer-trust
// contract: an explicitly configured root is accepted even without the
// .augure-desktop marker (a restored backup or custom install dir), because
// auto-discovery only ever proposes marker-named defaults.
func TestAugureDesktopAcceptsConfiguredCustomRoot(t *testing.T) {
	root := t.TempDir()
	createAugureDesktopStateDB(t, root)

	provider, ok := NewProvider(AgentAugureDesktop, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1,
		"a configured custom root must be parsed, not silently dropped")
	assert.Equal(t, AgentAugureDesktop, sources[0].Provider)
}

// TestAugureDesktopDefaultRootSpelling covers the two default root spellings
// (the POSIX .augure-desktop and the Windows %LOCALAPPDATA%\augure-desktop)
// that keep default discovery disjoint from Hermes without any runtime gate.
func TestAugureDesktopDefaultRootSpelling(t *testing.T) {
	spec, ok := AgentByType(AgentAugureDesktop)
	require.True(t, ok)
	assert.Equal(t, []string{".augure-desktop", "AppData/Local/augure-desktop"},
		spec.DefaultDirs)
}

// TestAugureDesktopProjectRelabel pins the project-rebrand contract: only
// the state-DB-synthesized names ("hermes", "hermes-<source>") are rebranded
// to the fork's producer name; explicit project hints pass through
// untouched.
func TestAugureDesktopProjectRelabel(t *testing.T) {
	relabel := func(project string, synthesized bool) string {
		result := &ParseResult{Session: ParsedSession{
			ID:                         "hermes:abc",
			Agent:                      AgentHermes,
			Project:                    project,
			projectSynthesizedByHermes: synthesized,
		}}
		relabelHermesResultAsAugureDesktop(result)
		return result.Session.Project
	}

	assert.Equal(t, "augure-desktop", relabel("hermes", true))
	assert.Equal(t, "augure-desktop-desktop", relabel("hermes-desktop", true))
	assert.Equal(t, "augure-desktop-work", relabel("hermes-work", true))
	// Explicit hints survive verbatim, including hermes-prefixed names.
	assert.Equal(t, "hermes-tools", relabel("hermes-tools", false))
	assert.Equal(t, "my-project", relabel("my-project", false))
	assert.Empty(t, relabel("", false))
}

// TestAugureDesktopTranscriptProjectRelabel covers the transcript parse
// paths' own synthesis fallbacks: without a hint, the project is
// synthesized from the session platform ("hermes-<platform>") or bare
// "hermes", and the relabel must rebrand those exactly like the state-DB
// path; an explicit hint survives untouched.
func TestAugureDesktopTranscriptProjectRelabel(t *testing.T) {
	const jsonBody = `{
		"session_start":"2026-05-14T10:00:00Z",
		"last_updated":"2026-05-14T10:20:00Z",
		"messages":[
			{"role":"user","content":"hello","timestamp":"2026-05-14T10:01:00Z"},
			{"role":"assistant","content":"reply","timestamp":"2026-05-14T10:02:00Z"}
		]
	}`
	build := func(t *testing.T, platform string, hint string) string {
		t.Helper()
		body := jsonBody
		if platform != "" {
			body = fmt.Sprintf(`{"platform":%q,"session_start":"2026-05-14T10:00:00Z",`+
				`"last_updated":"2026-05-14T10:20:00Z","messages":[`+
				`{"role":"user","content":"hello","timestamp":"2026-05-14T10:01:00Z"},`+
				`{"role":"assistant","content":"reply","timestamp":"2026-05-14T10:02:00Z"}]}`, platform)
		}
		path := createTestFile(t, "session_20260910_075655_ca54ab.json", body)
		provider, ok := NewProvider(AgentAugureDesktop, ProviderConfig{
			Roots: []string{t.TempDir()}, Machine: "devbox",
		})
		require.True(t, ok)
		hp, ok := provider.(*hermesProvider)
		require.True(t, ok)
		sess, msgs, err := hp.parseSession(path, hint, "devbox")
		require.NoError(t, err)
		require.NotNil(t, sess)
		result := &ParseResult{Session: *sess, Messages: msgs}
		relabelHermesResultAsAugureDesktop(result)
		return result.Session.Project
	}

	assert.Equal(t, "augure-desktop-discord", build(t, "discord", ""))
	assert.Equal(t, "augure-desktop", build(t, "", ""))
	assert.Equal(t, "hermes-tools", build(t, "discord", "hermes-tools"))
}

// TestAugureDesktopSessionIDRelabel guards the prefix swap semantics shared
// with the TraeX and Augure CLI relabels.
func TestAugureDesktopSessionIDRelabel(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"hermes prefix", "hermes:20260910_075655", "augure-desktop:20260910_075655"},
		{"already relabeled", "augure-desktop:abc", "augure-desktop:abc"},
		{"raw id is untouched", "20260910_075655", "20260910_075655"},
		{
			"only the first occurrence is replaced",
			"hermes:hermes:abc",
			"augure-desktop:hermes:abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, augureDesktopSessionID(tt.id))
		})
	}
}

// TestAugureDesktopRegistryEntry pins the registry shape the config and
// frontend consume.
func TestAugureDesktopRegistryEntry(t *testing.T) {
	def, ok := AgentByType(AgentAugureDesktop)
	require.True(t, ok)
	assert.Equal(t, "augure-desktop:", def.IDPrefix)
	assert.Equal(t, "AUGURE_DESKTOP_DIR", def.EnvVar)
	assert.Equal(t, "augure_desktop_dirs", def.ConfigKey)
	assert.Equal(t, "Augure Desktop", def.DisplayName)
	assert.Equal(t, []string{
		".augure-desktop",
		"AppData/Local/augure-desktop",
	}, def.DefaultDirs)
	assert.True(t, def.FileBased)
	assert.True(t, def.RemoteSyncExcluded,
		"roots hold raw state.db/WAL plus app state; excluded until a "+
			"safe allowlisted export exists")
}
