//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest/hermesfixture"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionHermesProfileIdentityAndDatabaseTransition(t *testing.T) {
	home := t.TempDir()
	writer := hermesfixture.OpenState(t, home)
	_, err := writer.ExecContext(t.Context(), `
INSERT INTO sessions(id,message_count,title) VALUES('parent',1,'Parent'),('child',1,'Child');
UPDATE sessions SET parent_session_id='parent' WHERE id='child';
INSERT INTO messages(session_id,role,content) VALUES('parent','user','parent content'),('child','user','original content');`)
	require.NoError(t, err)
	parse := func() rawderive.ParsedManifest {
		p, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: []string{home}, Machine: "capture-device"})
		require.True(t, ok)
		sources, err := p.Discover(t.Context())
		require.NoError(t, err)
		out := rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}, ReplaceSessionContent: true}
		for _, s := range sources {
			result, err := p.Parse(t.Context(), parser.ParseRequest{Source: s, Machine: "capture-device", ForceParse: true})
			require.NoError(t, err)
			require.Empty(t, result.SourceErrors)
			out.Outcome.Results = append(out.Outcome.Results, result.Results...)
		}
		return out
	}
	parsed := parse()
	require.Len(t, parsed.Outcome.Results, 2)
	f := newProjectionFixture(t)
	h, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	var manifests []rawsync.CanonicalManifest
	var receipts, children, parents []string
	alias := func(m rawsync.CanonicalManifest, member string) string {
		var id string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT a.alias_id FROM raw_session_public_aliases a JOIN raw_session_branches b ON b.branch_id=a.anchor_branch WHERE b.source_id=$1 AND b.member_id=$2`, rawSourceID(m), "hermes:"+member).Scan(&id))
		resolved, err := f.sink.Resolve(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, RawIdentityUnique, resolved.State)
		return resolved.PublicID
	}
	for _, source := range []string{"profiles/first/state.db", "profiles/second/state.db"} {
		m, commit := f.acceptScoped(t, "device-a", "initial", "", parser.AgentHermes, "profiles-root", source)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, parsed))
		manifests = append(manifests, m)
		receipts = append(receipts, commit.Receipt)
		children = append(children, alias(m, "child"))
		parents = append(parents, alias(m, "parent"))
	}
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 4, "the same native IDs and content in independent profiles are separate sessions")
	assert.NotEqual(t, children[0], children[1])
	assert.NotEqual(t, parents[0], parents[1])
	require.NoError(t, h.RenameSession(t.Context(), children[0], new("Saved first profile")))
	for i, id := range children {
		child, err := h.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, child)
		assert.Equal(t, new(parents[i]), child.ParentSessionID)
		if i == 0 {
			assert.Equal(t, new("Saved first profile"), child.DisplayName)
		} else {
			assert.Equal(t, new("Child"), child.DisplayName)
		}
	}
	// A file-only generation has an empty native SourceSessionID. It must update
	// the existing public session and retain omitted database-only members.
	require.NoError(t, writer.Close())
	require.NoError(t, os.Remove(filepath.Join(home, "state.db")))
	require.NoError(t, os.Mkdir(filepath.Join(home, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "sessions", "child.jsonl"), []byte(`{"role":"user","content":"replacement content"}`+"\n"), 0o600))
	transcript := parse()
	require.Len(t, transcript.Outcome.Results, 1)
	assert.Empty(t, transcript.Outcome.Results[0].Result.Session.SourceSessionID)
	next, _ := f.acceptScoped(t, "device-a", "file-only", receipts[0], parser.AgentHermes, "profiles-root", manifests[0].Manifest.SourceKey)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, next), next, transcript))
	assert.Equal(t, children[0], alias(next, "child"))
	child, err := h.GetSession(t.Context(), children[0])
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, new("Saved first profile"), child.DisplayName)
	messages, err := h.GetMessages(t.Context(), children[0], 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "replacement content", messages[0].Content)
	other, err := h.GetMessages(t.Context(), children[1], 0, 10, true)
	require.NoError(t, err)
	require.Len(t, other, 1)
	assert.Equal(t, "original content", other[0].Content)
	retained, err := h.GetSession(t.Context(), parents[0])
	require.NoError(t, err)
	assert.NotNil(t, retained, "missing DB members remain archived")
	page, err = h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 4)
}

func TestRawProjectionHermesMissingParentDoesNotResolveAcrossProfiles(t *testing.T) {
	makeHome := func(parent bool) string {
		t.Helper()
		home := t.TempDir()
		writer := hermesfixture.OpenState(t, home)
		if parent {
			_, err := writer.ExecContext(t.Context(), `
INSERT INTO sessions(id,message_count,title) VALUES('parent',1,'Parent');
INSERT INTO messages(session_id,role,content) VALUES('parent','user','parent content');`)
			require.NoError(t, err)
		} else {
			_, err := writer.ExecContext(t.Context(), `
INSERT INTO sessions(id,parent_session_id,message_count,title) VALUES('child','parent',1,'Child');
INSERT INTO messages(session_id,role,content) VALUES('child','user','child content');`)
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		return home
	}
	parse := func(home string) rawderive.ParsedManifest {
		t.Helper()
		p, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: []string{home}, Machine: "capture-device"})
		require.True(t, ok)
		sources, err := p.Discover(t.Context())
		require.NoError(t, err)
		out := rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true}, ReplaceSessionContent: true}
		for _, source := range sources {
			result, err := p.Parse(t.Context(), parser.ParseRequest{Source: source, Machine: "capture-device", ForceParse: true})
			require.NoError(t, err)
			require.Empty(t, result.SourceErrors)
			out.Outcome.Results = append(out.Outcome.Results, result.Results...)
		}
		return out
	}
	childHome := makeHome(false)
	parentHome := makeHome(true)
	childParsed, parentParsed := parse(childHome), parse(parentHome)
	require.Len(t, childParsed.Outcome.Results, 1)
	require.Len(t, parentParsed.Outcome.Results, 1)

	f := newProjectionFixture(t)
	childManifest, _ := f.acceptScoped(t, "device-a", "child", "", parser.AgentHermes, "profiles-root", "profiles/first/state.db")
	parentManifest, _ := f.acceptScoped(t, "device-a", "parent", "", parser.AgentHermes, "profiles-root", "profiles/second/state.db")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, childManifest), childManifest, childParsed))
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, parentManifest), parentManifest, parentParsed))

	publicID := func(m rawsync.CanonicalManifest, member string) string {
		t.Helper()
		var alias string
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `
SELECT a.alias_id FROM raw_session_public_aliases a
JOIN raw_session_branches b ON b.branch_id=a.anchor_branch
WHERE b.source_id=$1 AND b.member_id=$2`, rawSourceID(m), "hermes:"+member).Scan(&alias))
		resolved, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		require.Equal(t, RawIdentityUnique, resolved.State)
		return resolved.PublicID
	}
	childID := publicID(childManifest, "child")
	parentID := publicID(parentManifest, "parent")
	assert.NotEqual(t, childID, parentID)
	h, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	child, err := h.GetSessionFull(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Nil(t, child.ParentSessionID, "a same-named Hermes parent in another profile is unrelated")
	assert.Empty(t, child.ParentSessionIDs)
}
