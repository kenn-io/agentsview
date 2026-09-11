//go:build pgtest

package postgres

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Losing a sidechain split, session lineage, source UUID occurrence, offset,
// or chunk hash makes the hosted snapshot diverge from the SQLite corpus.
func TestHostedEmbeddingSidechainLineageParity(t *testing.T) {
	f, store, _ := embeddingFixture(t)
	_, err := f.runtime.Exec(`
INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,parent_session_id,relationship_type) VALUES
 ('a-parent','project','machine','claude',5,2,NULL,''),
 ('b-child','project','machine','claude',3,1,'a-parent','subagent');
INSERT INTO messages(session_id,ordinal,role,source_uuid,content,is_sidechain) VALUES
 ('a-parent',0,'user','p-u','parent question',false),
 ('a-parent',1,'assistant','p-a','main α',false),
 ('a-parent',2,'assistant','p-b','branch β',true),
 ('a-parent',3,'assistant','','branch γ',true),
 ('a-parent',4,'user','p-u2','branch question',true),
 ('b-child',0,'user','c-u','child question',false),
 ('b-child',1,'assistant','c-a','child answer',false),
 ('b-child',2,'assistant','','child detail',false)`)
	require.NoError(t, err)
	reconcileEmbedding(t, store)
	leases, err := store.Claim(t.Context(), "lineage", 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 2)

	var hosted []HostedEmbeddingDocument
	for _, lease := range leases {
		snapshot, readErr := store.ReadSession(t.Context(), lease)
		require.NoError(t, readErr)
		hosted = append(hosted, snapshot.Documents...)
	}

	expectedUnits := []db.EmbeddableUnit{
		{SessionID: "a-parent", Kind: "user", SourceUUID: "p-u", Ordinal: 0, OrdinalEnd: 0, Content: "parent question"},
		{SessionID: "a-parent", Kind: "run", SourceUUID: "p-a", Ordinal: 1, OrdinalEnd: 1, Content: "main α", Offsets: []db.UnitOffset{{Ordinal: 1}}},
		{SessionID: "a-parent", Kind: "run", SourceUUID: "p-b", Ordinal: 2, OrdinalEnd: 3, Subordinate: true, Content: "branch β\n\nbranch γ", Offsets: []db.UnitOffset{{Ordinal: 2}, {Ordinal: 3, RuneStart: 10, ByteStart: 11}}},
		{SessionID: "a-parent", Kind: "user", SourceUUID: "p-u2", Ordinal: 4, OrdinalEnd: 4, Subordinate: true, Content: "branch question"},
		{SessionID: "b-child", Kind: "user", SourceUUID: "c-u", Ordinal: 0, OrdinalEnd: 0, Subordinate: true, Content: "child question"},
		{SessionID: "b-child", Kind: "run", SourceUUID: "c-a", Ordinal: 1, OrdinalEnd: 2, Subordinate: true, Content: "child answer\n\nchild detail", Offsets: []db.UnitOffset{{Ordinal: 1}, {Ordinal: 2, RuneStart: 14, ByteStart: 14}}},
	}
	expected := []HostedEmbeddingDocument{
		{Key: "u:a-parent:p-u", Unit: expectedUnits[0], ContentHash: "16cf559f3c56f94c5d20560a8a32b0ba77a86e4dd30280f9e50bbba2169cf331", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "parent question", InputHash: "3f9ff6069f862d91b3c90cb7488b5ea117be0778082db9d68a66a66aba06ecf7"}}},
		{Key: "r:a-parent:p-a", Unit: expectedUnits[1], ContentHash: "1eb30c00f30c0a6fc4757b00a6336499185bf16be6b8a5d91761cd102254b428", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "main α", InputHash: "3cf9efe0d125efc5ccac2d39b9f7b17e7f827838f3147334d9f4cbf972d52eae"}}},
		{Key: "r:a-parent:p-b", Unit: expectedUnits[2], ContentHash: "d633ff5b51884e72fdf186a97649d4fec3902405829c82e03093b760c8fcada0", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "branch β\n\nbranch γ", InputHash: "cd24e23ebe9779712e67e896d164b4fc782a2f5ae3e353f4cef0623deb9331ab"}}},
		{Key: "u:a-parent:p-u2", Unit: expectedUnits[3], ContentHash: "f631ffdfddcd4a48c2494e19519f8e3c5531cf690799cc66c08e9cae5a82a69b", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "branch question", InputHash: "62e2b0f86d72d42c227ba77ee8d1db654cca92c065720d9206c13ccca50092f8"}}},
		{Key: "u:b-child:c-u", Unit: expectedUnits[4], ContentHash: "a0a40d39ccbc91bc2ad7e648be5fe3e1ca6ab52c3c37fff84a1ad29c1aef2fa6", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "child question", InputHash: "b919ac1aeffc94bfba801967f880f75fe0529d322d3a0bad963d10d7dca20d98"}}},
		{Key: "r:b-child:c-a", Unit: expectedUnits[5], ContentHash: "8d618a84d7667a768a7c016dc297cda31f86f6451e242fac6a1c55ae5b78c533", Chunks: []HostedEmbeddingChunk{{Index: 0, Text: "child answer\n\nchild detail", InputHash: "a5f7d436c7038613e59951d5e82bb1bd1cdc21fb2ed32d9161c77709270bd81c"}}},
	}
	assert.Equal(t, expected, hosted)

	local, err := db.Open(filepath.Join(t.TempDir(), "lineage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	parent := "a-parent"
	require.NoError(t, local.UpsertSession(db.Session{ID: "a-parent", Project: "project", Machine: "machine", Agent: "claude", MessageCount: 5, UserMessageCount: 2}))
	require.NoError(t, local.UpsertSession(db.Session{ID: "b-child", Project: "project", Machine: "machine", Agent: "claude", MessageCount: 3, UserMessageCount: 1, ParentSessionID: &parent, RelationshipType: "subagent"}))
	require.NoError(t, local.InsertMessages([]db.Message{
		{SessionID: "a-parent", Ordinal: 0, Role: "user", SourceUUID: "p-u", Content: "parent question"},
		{SessionID: "a-parent", Ordinal: 1, Role: "assistant", SourceUUID: "p-a", Content: "main α"},
		{SessionID: "a-parent", Ordinal: 2, Role: "assistant", SourceUUID: "p-b", Content: "branch β", IsSidechain: true},
		{SessionID: "a-parent", Ordinal: 3, Role: "assistant", Content: "branch γ", IsSidechain: true},
		{SessionID: "a-parent", Ordinal: 4, Role: "user", SourceUUID: "p-u2", Content: "branch question", IsSidechain: true},
		{SessionID: "b-child", Ordinal: 0, Role: "user", SourceUUID: "c-u", Content: "child question"},
		{SessionID: "b-child", Ordinal: 1, Role: "assistant", SourceUUID: "c-a", Content: "child answer"},
		{SessionID: "b-child", Ordinal: 2, Role: "assistant", Content: "child detail"},
	}))
	var localUnits []db.EmbeddableUnit
	_, err = local.ScanEmbeddableUnits(t.Context(), "", false, func(unit db.EmbeddableUnit) error {
		localUnits = append(localUnits, unit)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, expectedUnits, localUnits)
}
