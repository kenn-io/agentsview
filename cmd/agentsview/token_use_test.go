package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// newTestDB opens a fresh SQLite DB in a temp dir for a single test.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

// upsertSession inserts a session with minimal required fields.
func upsertSession(
	t *testing.T, d *db.DB, id, agent, startedAt string,
) {
	t.Helper()
	s := db.Session{
		ID:           id,
		Project:      "test-project",
		Machine:      "local",
		Agent:        agent,
		MessageCount: 1,
	}
	if startedAt != "" {
		s.StartedAt = &startedAt
	}
	require.NoError(t, d.UpsertSession(s), "upsert %s", id)
}

func TestResolveSessionID_PrefixedInput_NoEvidence_UnchangedNotKnown(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// A prefixed input with no DB row and no disk evidence is
	// returned unchanged so downstream lookup/error messages
	// use what the caller typed, but known=false so the caller
	// skips the on-demand sync that would only warn about a
	// missing source file.
	input := "codex:019d5490-fe31-7e62-838c-8ba4193f245d"
	got, known := resolveRawSessionID(ctx, d, nil, input)
	assert.Equal(t, input, got)
	assert.False(t, known, "known should be false (no evidence)")
}

func TestResolveSessionID_HostPrefixedInput_ReturnedUnchanged(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Host-prefixed IDs are unambiguously canonical remote IDs;
	// resolution short-circuits without touching DB or disk.
	input := "other-host~codex:abc-123"
	got, known := resolveRawSessionID(ctx, d, nil, input)
	assert.Equal(t, input, got)
	assert.True(t, known, "known should be true (host-prefixed)")
}

func TestResolveSessionID_BareClaudeUUID_ExactMatch(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Claude sessions have no prefix; the bare UUID is the
	// canonical ID stored in sessions.id.
	id := "11111111-1111-1111-1111-111111111111"
	upsertSession(t, d, id, "claude", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, id)
	assert.Equal(t, id, got)
	assert.True(t, known, "known should be true (DB match)")
}

func TestResolveSessionID_BareCodexUUID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "019d5490-fe31-7e62-838c-8ba4193f245d"
	stored := "codex:" + bare
	upsertSession(t, d, stored, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	assert.Equal(t, stored, got)
	assert.True(t, known, "known should be true (DB match)")
}

func TestResolveSessionID_Ambiguous_MostRecentWins(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "22222222-2222-2222-2222-222222222222"
	// Older codex session.
	upsertSession(t, d, "codex:"+bare, "codex", "2026-04-16T10:00:00Z")
	// Newer amp session with same raw UUID.
	upsertSession(t, d, "amp:"+bare, "amp", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	assert.Equal(t, "amp:"+bare, got, "most recent should win")
	assert.True(t, known)
}

func TestResolveSessionID_NotInDB_FoundOnDisk(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Create a codex session file on disk: the probe path
	// should resolve a bare raw UUID to the prefixed form.
	codexDir := filepath.Join(t.TempDir(), "codex-sessions")
	bare := "33333333-3333-3333-3333-333333333333"
	dayDir := filepath.Join(codexDir, "2026", "04", "17")
	require.NoError(t, os.MkdirAll(dayDir, 0o755), "mkdir")
	fname := "rollout-2026-04-17T10-00-00-" + bare + ".jsonl"
	fpath := filepath.Join(dayDir, fname)
	require.NoError(t, os.WriteFile(fpath, []byte("{}\n"), 0o644), "write")

	agentDirs := map[parser.AgentType][]string{
		parser.AgentCodex: {codexDir},
	}
	got, known := resolveRawSessionID(ctx, d, agentDirs, bare)
	assert.Equal(t, "codex:"+bare, got, "disk probe")
	assert.True(t, known, "disk probe found match")
}

func TestResolveSessionID_NotFoundAnywhere_PassThrough(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	bare := "44444444-4444-4444-4444-444444444444"
	got, known := resolveRawSessionID(ctx, d, nil, bare)
	assert.Equal(t, bare, got, "pass-through")
	assert.False(t, known, "known should be false (nothing found)")
}

func TestResolveSessionID_BareClaudeAndPrefixedSameUUID_ClaudeExactWins(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Edge: a bare Claude UUID that ALSO exists as a prefixed
	// session (e.g. codex:<same-uuid>). The Claude row is an
	// exact match and should win over the suffix match.
	bare := "55555555-5555-5555-5555-555555555555"
	upsertSession(t, d, bare, "claude", "2026-04-16T10:00:00Z")
	upsertSession(t, d, "codex:"+bare, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	assert.Equal(t, bare, got, "exact claude match")
	assert.True(t, known)
}

func TestResolveSessionID_ExactMatchWinsOverNewerCollisions(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Bare Claude session is the exact match but older than
	// multiple prefixed sessions sharing the same suffix. The
	// exact row must always win, even if a LIMIT on the suffix
	// query would exclude it by recency.
	bare := "88888888-8888-8888-8888-888888888888"
	upsertSession(t, d, bare, "claude", "2026-04-10T10:00:00Z")
	upsertSession(t, d, "codex:"+bare, "codex",
		"2026-04-15T10:00:00Z")
	upsertSession(t, d, "amp:"+bare, "amp",
		"2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, bare)
	assert.Equal(t, bare, got,
		"exact match must beat newer suffix collisions")
	assert.True(t, known)
}

func TestResolveSessionID_KimiRawID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Kimi raw IDs have the shape "<project-hash>:<session-uuid>".
	// The stored canonical form prepends "kimi:".
	raw := "proj-hash-abc:66666666-6666-6666-6666-666666666666"
	stored := "kimi:" + raw
	upsertSession(t, d, stored, "kimi", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	assert.Equal(t, stored, got, "kimi raw ID resolves")
	assert.True(t, known)
}

func TestResolveSessionID_OpenClawRawID_ResolvesToPrefixed(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// OpenClaw raw IDs have the shape "<agentId>:<sessionId>".
	raw := "main:abc-123"
	stored := "openclaw:" + raw
	upsertSession(t, d, stored, "openclaw", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	assert.Equal(t, stored, got, "openclaw raw ID resolves")
	assert.True(t, known)
}

func TestResolveSessionID_CanonicalKimiID_ResolvesWhenInDB(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// A canonical Kimi ID already in the DB resolves via the
	// exact-match branch. A canonical ID with no DB row and no
	// disk evidence falls through to known=false so no
	// misleading sync warning is emitted.
	input := "kimi:proj-abc:77777777-7777-7777-7777-777777777777"
	upsertSession(t, d, input, "kimi", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, input)
	assert.Equal(t, input, got, "exact DB match")
	assert.True(t, known, "exact DB match")
}

func TestResolveSessionID_CanonicalCodexID_OnDiskNotInDB(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Canonical "codex:<uuid>" not yet synced but present on
	// disk must resolve via the canonical disk probe, which strips
	// the prefix before asking the agent source lookup.
	codexDir := filepath.Join(t.TempDir(), "codex-sessions")
	uuid := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dayDir := filepath.Join(codexDir, "2026", "04", "17")
	require.NoError(t, os.MkdirAll(dayDir, 0o755), "mkdir")
	fname := "rollout-2026-04-17T10-00-00-" + uuid + ".jsonl"
	require.NoError(t, os.WriteFile(
		filepath.Join(dayDir, fname), []byte("{}\n"), 0o644,
	), "write")

	agentDirs := map[parser.AgentType][]string{
		parser.AgentCodex: {codexDir},
	}
	input := "codex:" + uuid
	got, known := resolveRawSessionID(ctx, d, agentDirs, input)
	assert.Equal(t, input, got, "canonical on disk")
	assert.True(t, known, "canonical disk probe")
}

func TestResolveSessionID_ProviderAuthoritativeCursorOnDiskNotInDB(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	cursorDir := t.TempDir()
	rawID := "provider-cursor"
	transcriptPath := filepath.Join(
		cursorDir,
		"Users-fiona-Documents-demo",
		"agent-transcripts",
		rawID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(
		transcriptPath,
		[]byte(`{"role":"user","content":"hi"}`+"\n"),
		0o644,
	))

	agentDirs := map[parser.AgentType][]string{
		parser.AgentCursor: {cursorDir},
	}
	got, known := resolveRawSessionID(ctx, d, agentDirs, rawID)
	assert.Equal(t, "cursor:"+rawID, got,
		"provider FindSource should resolve unsynced raw cursor IDs")
	assert.True(t, known, "provider disk probe")

	got, known = resolveRawSessionID(ctx, d, agentDirs, "cursor:"+rawID)
	assert.Equal(t, "cursor:"+rawID, got,
		"canonical provider ID should resolve via provider FindSource")
	assert.True(t, known, "canonical provider disk probe")
}

func TestResolveSessionID_RawOpenClawCollidesWithCodexPrefix(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// OpenClaw permits arbitrary alphanumeric-dash-underscore
	// agent IDs, so a user may have one literally named "codex".
	// The raw OpenClaw ID "codex:abc-123" is stored as
	// "openclaw:codex:abc-123". Passing the raw form must not
	// be short-circuited as a canonical Codex ID — DB suffix
	// resolution must take precedence.
	raw := "codex:abc-123"
	stored := "openclaw:" + raw
	upsertSession(t, d, stored, "openclaw",
		"2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	assert.Equal(t, stored, got,
		"raw openclaw must beat canonical-prefix short-circuit")
	assert.True(t, known)
}

func TestResolveSessionID_UnderscoreID_NoFalseMatch(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Underscore is a LIKE wildcard in SQLite. If the query
	// uses LIKE naively, a raw id "20260403_aaa" would match
	// rows whose id ends with ":20260403Xaaa" (X = any char).
	// Insert a decoy that would only match under naive LIKE
	// semantics, plus a true match, and assert the true match
	// wins.
	raw := "20260403_aaa"
	decoy := "codex:20260403Xaaa"
	real := "codex:" + raw
	upsertSession(t, d, decoy, "codex", "2026-04-16T10:00:00Z")
	upsertSession(t, d, real, "codex", "2026-04-17T10:00:00Z")

	got, known := resolveRawSessionID(ctx, d, nil, raw)
	assert.Equal(t, real, got, "underscore is literal")
	assert.True(t, known)
}

func TestAgentHasDiskSourceLookupIncludesProviderAuthoritativeAgents(t *testing.T) {
	for _, agent := range []parser.AgentType{
		parser.AgentGptme,
		parser.AgentPi,
		parser.AgentOMP,
		parser.AgentWorkBuddy,
		parser.AgentCortex,
		parser.AgentKimi,
		parser.AgentQwenPaw,
		parser.AgentOpenHands,
		parser.AgentCursor,
		parser.AgentVibe,
		parser.AgentClaude,
		parser.AgentCowork,
		parser.AgentHermes,
	} {
		def, ok := parser.AgentByType(agent)
		require.True(t, ok, "agent %s", agent)
		assert.True(t, agentHasDiskSourceLookup(def),
			"token-use disk probe must include provider-authoritative %s", agent)
	}
}

func TestUsageExitCode_TokenData(t *testing.T) {
	u := &db.SessionUsage{HasTokenData: true}
	assert.Equal(t, tokenUseExitOK, usageExitCode(u))
}

func TestUsageExitCode_CostOnly(t *testing.T) {
	u := &db.SessionUsage{HasTokenData: false, HasCost: true}
	assert.Equal(t, tokenUseExitOK, usageExitCode(u),
		"cost-only must not be exit 3")
}

func TestUsageExitCode_NoData(t *testing.T) {
	u := &db.SessionUsage{}
	assert.Equal(t, tokenUseExitNoTokenData, usageExitCode(u))
}

func TestUsageExitCode_NotFound(t *testing.T) {
	assert.Equal(t, tokenUseExitNotFound, usageExitCode(nil))
}
