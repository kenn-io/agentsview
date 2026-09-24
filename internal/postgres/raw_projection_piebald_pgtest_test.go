//go:build pgtest

package postgres

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestRawProjectionPiebaldSourceLocalIdentity(t *testing.T) {
	root := t.TempDir()
	source, err := sql.Open("sqlite3", filepath.Join(root, parser.PiebaldDBFilename))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	_, err = source.ExecContext(t.Context(), `
CREATE TABLE projects(id INTEGER PRIMARY KEY,directory TEXT,name TEXT);
CREATE TABLE chats(id INTEGER PRIMARY KEY,title TEXT,created_at TEXT,updated_at TEXT,is_deleted INTEGER,message_count INTEGER,current_directory TEXT,worktree_path TEXT,branch_name TEXT,project_id INTEGER);
CREATE TABLE messages(id INTEGER PRIMARY KEY,parent_chat_id INTEGER DEFAULT 1,parent_message_id INTEGER,role TEXT,model TEXT,created_at TEXT DEFAULT '2026-01-01T00:00:00Z',updated_at TEXT,input_tokens INTEGER,output_tokens INTEGER,reasoning_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,status TEXT,finish_reason TEXT,error TEXT,enabled INTEGER DEFAULT 1);
CREATE TABLE message_parts(id INTEGER PRIMARY KEY,parent_chat_message_id INTEGER,part_index INTEGER,part_type TEXT);
CREATE TABLE message_part_text(message_part_id INTEGER PRIMARY KEY,is_thinking INTEGER);
CREATE TABLE message_content_nodes(id INTEGER PRIMARY KEY,parent_text_part_id INTEGER,node_index INTEGER,node_type TEXT);
CREATE TABLE message_node_text(node_id INTEGER PRIMARY KEY,content TEXT);
INSERT INTO chats VALUES(1,'Example chat','2026-01-01T00:00:00Z','2026-01-01T00:01:00Z',0,2,'','','',NULL);
INSERT INTO messages(id,parent_message_id,role) VALUES(1,NULL,'user'),(2,1,'assistant');
INSERT INTO message_parts VALUES(1,1,0,'text'),(2,2,0,'text');
INSERT INTO message_part_text VALUES(1,0),(2,0);
INSERT INTO message_content_nodes VALUES(1,1,0,'text'),(2,2,0,'text');
INSERT INTO message_node_text VALUES(1,'Please check this'),(2,'Checked');`)
	require.NoError(t, err)
	factory, ok := parser.ProviderFactoryByType(parser.AgentPiebald)
	require.True(t, ok)
	provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}, Machine: "capture-device"})
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	parse := func() rawderive.ParsedManifest {
		outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0], Machine: "capture-device"})
		require.NoError(t, err)
		require.Empty(t, outcome.SourceErrors)
		return rawderive.ParsedManifest{Outcome: outcome}
	}
	parsed := parse()
	require.Len(t, parsed.Outcome.Results, 1)
	assert.Equal(t, "piebald:1", parsed.Outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "1", parsed.Outcome.Results[0].Result.Session.SourceSessionID)

	f := newProjectionFixture(t)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	var publicIDs []string
	var firstReceipt string
	for i, scope := range []struct{ device, root, source string }{
		{"device-a", "root-a", "app.db"},
		{"device-b", "root-a", "app.db"},
		{"device-a", "root-b", "app.db"},
		{"device-a", "root-a", "other.db"},
	} {
		m, accepted := f.acceptScoped(t, scope.device, "initial", "", parser.AgentPiebald, scope.root, scope.source)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, parsed))
		resolved, err := f.sink.Resolve(t.Context(), f.alias(t, m))
		require.NoError(t, err)
		require.Equal(t, RawIdentityUnique, resolved.State)
		publicIDs = append(publicIDs, resolved.PublicID)
		if i == 0 {
			firstReceipt = accepted.Receipt
			require.NoError(t, h.RenameSession(t.Context(), resolved.PublicID, new("Saved on first source")))
		}
	}
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 4, "identical row IDs and content from independent databases must stay separate")
	for i, id := range publicIDs {
		session, err := h.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, id, session.ID, "adding another database must not change the first public ID")
		assert.Equal(t, "1", session.SourceSessionID)
		if i == 0 {
			assert.Equal(t, new("Saved on first source"), session.DisplayName)
		} else {
			assert.NotEqual(t, new("Saved on first source"), session.DisplayName)
		}
	}

	// The parser gives a fork the same source chat ID but a distinct member ID.
	// Its parent link must resolve within this database despite the other chat 1s.
	_, err = source.ExecContext(t.Context(), `
INSERT INTO messages(id,parent_message_id,role,enabled) VALUES(3,1,'user',0),(4,3,'assistant',0);
INSERT INTO message_parts VALUES(3,3,0,'text'),(4,4,0,'text');
INSERT INTO message_part_text VALUES(3,0),(4,0);
INSERT INTO message_content_nodes VALUES(3,3,0,'text'),(4,4,0,'text');
INSERT INTO message_node_text VALUES(3,'Try another approach'),(4,'Alternative answer');
UPDATE chats SET message_count=4;`)
	require.NoError(t, err)
	parsed = parse()
	require.Len(t, parsed.Outcome.Results, 2)
	assert.Equal(t, "1", parsed.Outcome.Results[1].Result.Session.SourceSessionID)
	m, _ := f.acceptScoped(t, "device-a", "with-fork", firstReceipt, parser.AgentPiebald, "root-a", "app.db")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, parsed))
	children, err := h.GetChildSessions(t.Context(), publicIDs[0])
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, new(publicIDs[0]), children[0].ParentSessionID)
	assert.NotEqual(t, publicIDs[0], children[0].ID)
	parent, err := h.GetSession(t.Context(), publicIDs[0])
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, publicIDs[0], parent.ID)
	assert.Equal(t, new("Saved on first source"), parent.DisplayName)
}
