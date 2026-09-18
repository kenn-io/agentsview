package parser

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiloProviderParseRelabelsOpenCodeSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_kilo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_kilo",
		"parentID":  "ses_parent",
		"directory": "/home/user/code/kiloapp",
		"title":     "Kilo Session",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "message", "ses_kilo", "msg_1.json",
	), map[string]any{
		"id":        "msg_1",
		"sessionID": "ses_kilo",
		"role":      "user",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})
	writeOpenCodeStorageFile(t, filepath.Join(
		root, "storage", "part", "msg_1", "prt_1.json",
	), map[string]any{
		"id":        "prt_1",
		"sessionID": "ses_kilo",
		"messageID": "msg_1",
		"type":      "text",
		"text":      "Hello from Kilo",
		"time": map[string]any{
			"created": 1700000000000,
		},
	})

	provider, ok := NewProvider(AgentKilo, ProviderConfig{
		Roots:   []string{root},
		Machine: "testmachine",
	})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "kilo:ses_kilo",
	})
	require.NoError(err)
	require.True(found)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  source,
		Machine: "testmachine",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(msgs, 1)

	assert.Equal("kilo:ses_kilo", sess.ID)
	assert.Equal("kilo:ses_parent", sess.ParentSessionID)
	assert.Equal(AgentKilo, sess.Agent)
	assert.Equal("kiloapp", sess.Project)
	assert.Equal("Hello from Kilo", msgs[0].Content)
}

func TestKiloProviderDiscoversSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionPath := filepath.Join(
		root, "storage", "session", "global", "ses_kilo.json",
	)
	writeOpenCodeStorageFile(t, sessionPath, map[string]any{
		"id":        "ses_kilo",
		"directory": "/home/user/code/kiloapp",
		"time": map[string]any{
			"created": 1700000000000,
			"updated": 1700000060000,
		},
	})

	provider, ok := NewProvider(AgentKilo, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	assert.Equal(sessionPath, sources[0].DisplayPath)
	assert.Equal("kiloapp", sources[0].ProjectHint)
	assert.Equal(AgentKilo, sources[0].Provider)
}

func TestKiloSQLiteVirtualPathRoundTrips(t *testing.T) {
	assert := assert.New(t)

	wantDBPath := filepath.Join(t.TempDir(), "kilo.db")
	virtual := KiloSQLiteVirtualPath(wantDBPath, "ses_kilo")
	dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(kiloFmt.dbName, virtual)
	require.True(t, ok)
	assert.Equal(wantDBPath, dbPath)
	assert.Equal("ses_kilo", sessionID)

	_, _, ok = parseOpenCodeFormatVirtualPath(
		kiloFmt.dbName,
		filepath.Join(t.TempDir(), "opencode.db")+"#ses_kilo",
	)
	assert.False(ok)
}

func TestKiloSQLiteProjectionWithoutSequence(t *testing.T) {
	for _, projection := range []bool{false, true} {
		t.Run(fmt.Sprint("projection=", projection), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			path, seed, writer := newTestDB(t)
			t.Cleanup(func() { writer.Close() })
			seed.AddProject("project-a", "/workspace/project-a")
			seed.AddSession("ses_kilo", "project-a", "", "", 1700000000000, 1700000060000)
			// Released Kilo schema before the projection-order migration.
			_, err := writer.ExecContext(t.Context(), `CREATE TABLE session_message (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL,
 time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
 INSERT INTO message VALUES ('msg_legacy', 'ses_kilo', 1700000000000, 1700000000000, '{"role":"user"}');
 INSERT INTO part VALUES ('part_legacy', 'msg_legacy', 'ses_kilo', 1700000000000, 1700000000000, '{"type":"text","text":"Earlier question"}');`)
			require.NoError(err)
			if projection {
				_, err = writer.ExecContext(t.Context(), `INSERT INTO session_message VALUES
 ('msg_z', 'ses_kilo', 'user', 1700000001000, 1700000001000, '{"text":"Next question"}'),
 ('msg_b', 'ses_kilo', 'assistant', 1700000002000, 1700000002000, '{"content":[{"type":"text","text":"Second answer"}]}'),
 ('msg_a', 'ses_kilo', 'assistant', 1700000002000, 1700000002000, '{"content":[{"type":"text","text":"First answer"}]}');`)
				require.NoError(err)
			}
			metas, err := ListKiloSessionMeta(path)
			require.NoError(err)
			require.Len(metas, 1)
			_, messages, err := parseOpenCodeDBSession(path, "ses_kilo", "testmachine")
			require.NoError(err)
			var content []string
			for _, message := range messages {
				content = append(content, message.Content)
			}
			if !projection {
				assert.Equal([]string{"Earlier question"}, content)
				return
			}
			assert.Equal([]string{"Earlier question", "Next question", "First answer", "Second answer"}, content)
			_, before, _, err := openCodeSessionCompositeMtime(t.Context(), writer, path, "ses_kilo")
			require.NoError(err)
			// Same row count and update-time maximum, but a different ordering key.
			_, err = writer.ExecContext(t.Context(), `UPDATE session_message SET time_created = 1700000003000 WHERE id = 'msg_a'`)
			require.NoError(err)
			_, after, _, err := openCodeSessionCompositeMtime(t.Context(), writer, path, "ses_kilo")
			require.NoError(err)
			assert.NotEqual(before, after)
			_, messages, err = parseOpenCodeDBSession(path, "ses_kilo", "testmachine")
			require.NoError(err)
			require.Len(messages, 4)
			assert.Equal("Second answer", messages[2].Content)
			assert.Equal("First answer", messages[3].Content)
		})
	}
}
