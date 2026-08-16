package db

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeSessionDropsSelfParent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		parent           *string
		parserParent     *string
		wantParent       *string
		wantParserParent *string
	}{
		{
			name:   "self parent cleared",
			parent: Ptr("s1"),
		},
		{
			name:             "self parent falls back to parser parent",
			parent:           Ptr("s1"),
			parserParent:     Ptr("main"),
			wantParent:       Ptr("main"),
			wantParserParent: Ptr("main"),
		},
		{
			name:         "self parser parent cleared",
			parserParent: Ptr("s1"),
		},
		{
			name:         "both self references cleared",
			parent:       Ptr("s1"),
			parserParent: Ptr("s1"),
		},
		{
			name:             "real parent kept",
			parent:           Ptr("main"),
			parserParent:     Ptr("main"),
			wantParent:       Ptr("main"),
			wantParserParent: Ptr("main"),
		},
		{
			name: "nil parent kept",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Session{
				ID: "s1", ParentSessionID: tc.parent,
				ParserParentSessionID: tc.parserParent,
				RelationshipType:      "subagent",
			}
			SanitizeSession(&s)
			assert.Equal(t, tc.wantParent, s.ParentSessionID)
			assert.Equal(t, tc.wantParserParent, s.ParserParentSessionID)
			assert.Equal(t, "subagent", s.RelationshipType,
				"dropping the parent claim must not retag the row")
		})
	}
}

// TestSelfParentNeverPersists pins the contract at every session write
// boundary: a parser or imported artifact that names the session as its own
// parent stores a NULL parent, so the row stays a hierarchy root instead of
// disappearing under itself.
func TestSelfParentNeverPersists(t *testing.T) {
	const origin = "peer-a1b2c3"
	selfParented := func(id string) Session {
		return Session{
			ID: id, Project: "p", Machine: origin, Agent: "claude",
			ParentSessionID: Ptr(id), RelationshipType: "subagent",
		}
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, d *DB) string
	}{
		{
			name: "UpsertSession",
			write: func(t *testing.T, d *DB) string {
				require.NoError(t, d.UpsertSession(selfParented("upsert-self")))
				return "upsert-self"
			},
		},
		{
			name: "WriteSessionBatch",
			write: func(t *testing.T, d *DB) string {
				_, err := d.WriteSessionBatch([]SessionBatchWrite{{
					Session:  selfParented("batch-self"),
					Messages: []Message{spawnEdgeTo("batch-self", "batch-self", "self")},
				}})
				require.NoError(t, err)
				return "batch-self"
			},
		},
		{
			name: "ApplyArtifactImportedSession",
			write: func(t *testing.T, d *DB) string {
				gid := origin + "~import-self"
				result, err := d.ApplyArtifactImportedSession(
					context.Background(),
					ArtifactImportedSession{
						Origin: origin, GID: gid,
						ManifestHash:      strings.Repeat("b", 64),
						ImportedSessionID: gid,
					},
					SessionBatchWrite{
						Session:         selfParented(gid),
						Messages:        []Message{spawnEdgeTo(gid, gid, "self")},
						ReplaceMessages: true,
					},
				)
				require.NoError(t, err)
				require.True(t, result.Written)
				return gid
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			id := tc.write(t, d)
			s, err := d.GetSession(context.Background(), id)
			require.NoError(t, err)
			require.NotNil(t, s)
			assert.Nil(t, s.ParentSessionID)
			assert.Equal(t, "subagent", s.RelationshipType)
		})
	}
}
