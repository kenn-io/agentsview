//go:build chtest

package clickhouse

import (
	"context"
	"fmt"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestReadStatusHonorsProjectFilters(t *testing.T) {
	_, syncer, local := newPushedStore(t)
	ctx := context.Background()
	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err)

	all, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, all.Sessions)
	assert.Equal(t, 4, all.Messages)

	alpha, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, []string{"alpha"}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, alpha.Sessions, "alpha root and its child")
	assert.Equal(t, 3, alpha.Messages)

	notAlpha, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, nil, []string{"alpha"})
	require.NoError(t, err)
	assert.Equal(t, 1, notAlpha.Sessions)
	assert.Equal(t, 1, notAlpha.Messages)
}

func TestStoreSessionsMessagesAndSearch(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()

	t.Run("list_sessions_default_sort_and_cursor", func(t *testing.T) {
		page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 1})
		require.NoError(t, err)
		assert.Equal(t, 2, page.Total)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, fixtureBetaID, page.Sessions[0].ID)
		require.NotEmpty(t, page.NextCursor)

		next, err := store.ListSessions(ctx, db.SessionFilter{
			Limit: 1, Cursor: page.NextCursor,
		})
		require.NoError(t, err)
		require.Len(t, next.Sessions, 1)
		assert.Equal(t, fixtureAlphaID, next.Sessions[0].ID)
		assert.Equal(t, 2, next.Total)
	})

	t.Run("orphan_subagent_with_null_parent_is_sidebar_root", func(t *testing.T) {
		store, syncer, local := newPushedStore(t)
		orphanID := "ch-orphan-child"
		orphan := fixtureSession(orphanID, "alpha", "orphan first", "2026-01-10T00:06:00.000Z", 1)
		orphan.RelationshipType = "subagent"
		orphan.ParentSessionID = nil
		_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session: orphan,
			Messages: []db.Message{
				fixtureMessage(orphanID, 0, "user", "orphan first", "2026-01-10T00:06:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
		_, err = syncer.Push(ctx, false, nil)
		require.NoError(t, err)

		index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
		require.NoError(t, err)
		ids := make([]string, len(index.Sessions))
		for i, row := range index.Sessions {
			ids[i] = row.ID
		}
		assert.Contains(t, ids, orphanID)
	})

	t.Run("sidebar_includes_child_under_parent", func(t *testing.T) {
		index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
		require.NoError(t, err)
		assert.Equal(t, 1, index.Total)
		ids := make([]string, len(index.Sessions))
		for i, row := range index.Sessions {
			ids[i] = row.ID
		}
		assert.ElementsMatch(t, []string{fixtureAlphaID, fixtureChildID}, ids)
	})

	t.Run("get_session_and_find", func(t *testing.T) {
		sess, err := store.GetSession(ctx, fixtureAlphaID)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "alpha", sess.Project)

		ids, err := store.FindSessionIDsByPartial(ctx, "ch-sync-alpha", 5)
		require.NoError(t, err)
		assert.Contains(t, ids, fixtureAlphaID)
	})

	t.Run("messages_window_and_tool_result", func(t *testing.T) {
		asc, err := store.GetMessages(ctx, fixtureAlphaID, 0, 10, true)
		require.NoError(t, err)
		require.Len(t, asc, 2)
		assert.Equal(t, []int{0, 1}, []int{asc[0].Ordinal, asc[1].Ordinal})
		require.Len(t, asc[1].ToolCalls, 2)
		require.Len(t, asc[1].ToolCalls[0].ResultEvents, 1)
		assert.Equal(t, "clickhouse result", asc[1].ToolCalls[0].ResultEvents[0].Content)
		assert.Equal(t, "clickhouse result", asc[1].ToolCalls[0].ResultContent)

		desc, err := store.GetMessages(ctx, fixtureAlphaID, 1, 10, false)
		require.NoError(t, err)
		require.Len(t, desc, 2)
		assert.Equal(t, []int{1, 0}, []int{desc[0].Ordinal, desc[1].Ordinal})

		anchor := 1
		window, err := store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
			Around: &anchor, Before: 1, After: 1,
		})
		require.NoError(t, err)
		require.NotEmpty(t, window)
		assert.Equal(t, 1, window[len(window)-1].Ordinal)

		counts, err := store.GetResumeModelCounts(ctx, fixtureAlphaID)
		require.NoError(t, err)
		assert.Equal(t, []db.ModelCount{{Model: "claude-test", Count: 1}}, counts)

		timing, err := store.GetSessionTiming(ctx, fixtureAlphaID)
		require.NoError(t, err)
		require.NotNil(t, timing)
	})

	t.Run("search_and_secrets", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{Query: "secret token", Limit: 5})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, fixtureAlphaID, page.Results[0].SessionID)
		assert.Equal(t, fixtureMachine, page.Results[0].Machine)

		content, err := store.SearchContent(ctx, db.ContentSearchFilter{
			Pattern:        "clickhouse result",
			Sources:        []string{"tool_result"},
			IncludeOneShot: true,
			Limit:          5,
		})
		require.NoError(t, err)
		require.NotEmpty(t, content.Matches)
		assert.Equal(t, "tool_result", content.Matches[0].Location)
		assert.Equal(t, fixtureAlphaID, content.Matches[0].SessionID)
		assert.Equal(t, fixtureMachine, content.Matches[0].Machine)
		require.NotNil(t, content.Matches[0].DisplayName)
		assert.Equal(t, "Alpha Saved Title", *content.Matches[0].DisplayName)

		findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
			Project: "alpha", Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, findings.Findings, 1)
		source, ok, err := store.SecretFindingSource(ctx, findings.Findings[0].SecretFinding)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Contains(t, source, "secret token")
	})

	t.Run("metadata_stars_and_pins", func(t *testing.T) {
		stats, err := store.GetStats(ctx, false, false)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.SessionCount)
		assert.Equal(t, 3, stats.MessageCount)

		projects, err := store.GetProjects(ctx, false, false)
		require.NoError(t, err)
		names := make([]string, len(projects))
		for i, p := range projects {
			names[i] = p.Name
		}
		assert.Equal(t, []string{"alpha", "beta"}, names)

		agents, err := store.GetAgents(ctx, false, false)
		require.NoError(t, err)
		require.Len(t, agents, 1)
		assert.Equal(t, "claude", agents[0].Name)

		machines, err := store.GetMachines(ctx, false, false)
		require.NoError(t, err)
		assert.Equal(t, []string{fixtureMachine}, machines)

		branches, err := store.GetBranches(ctx, false, false)
		require.NoError(t, err)
		foundMain := false
		for _, b := range branches {
			if b.Project == "alpha" && b.Branch == "main" {
				foundMain = true
			}
		}
		assert.True(t, foundMain)

		_, err = store.GetMachineLabels(ctx)
		require.NoError(t, err)
		_, err = store.GetMachineAliases(ctx)
		require.NoError(t, err)

		stars, err := store.ListStarredSessionIDs(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{fixtureAlphaID}, stars)

		pins, err := store.ListPinnedMessages(ctx, fixtureAlphaID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		require.NotNil(t, pins[0].Note)
		assert.Equal(t, "pin alpha", *pins[0].Note)
	})

	t.Run("writes_are_read_only", func(t *testing.T) {
		ok, err := store.StarSession(t.Context(), fixtureBetaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.False(t, ok)
		require.ErrorIs(t, store.UnstarSession(t.Context(), fixtureAlphaID), db.ErrReadOnly)
		require.ErrorIs(t, store.BulkStarSessions(t.Context(), []string{fixtureBetaID}), db.ErrReadOnly)
		pinID, err := store.PinMessage(t.Context(), fixtureAlphaID, 1, nil)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.Zero(t, pinID)
		require.ErrorIs(t, store.UnpinMessage(t.Context(), fixtureAlphaID, 1), db.ErrReadOnly)
		require.ErrorIs(t, store.RenameSession(t.Context(), fixtureAlphaID, nil), db.ErrReadOnly)
		require.ErrorIs(t, store.SoftDeleteSession(t.Context(), fixtureAlphaID), db.ErrReadOnly)
		_, err = store.RestoreSession(t.Context(), fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.DeleteSessionIfTrashed(t.Context(), fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.EmptyTrash(t.Context())
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.InsertInsight(t.Context(), db.Insight{})
		require.ErrorIs(t, err, db.ErrReadOnly)
	})
}

func TestSearchTreatsUnderscoreAsLiteral(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := context.Background()
	appendMessage(t, local, fixtureAlphaID, "hello_world unique token", "2026-01-10T00:04:00.000Z")
	appendMessage(t, local, fixtureBetaID, "helloXworld unique token", "2026-01-11T00:04:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	literal, err := store.Search(ctx, db.SearchFilter{Query: "hello_world", Limit: 5})
	require.NoError(t, err)
	require.Len(t, literal.Results, 1,
		"hello_world must match the underscore session and not helloXworld")
	assert.Equal(t, fixtureAlphaID, literal.Results[0].SessionID)
}

func TestGetSessionFullReturnsFilePath(t *testing.T) {
	store, _, local := newPushedStore(t)
	ctx := context.Background()
	want, err := local.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, want)
	require.NotNil(t, want.FilePath)

	got, err := store.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.FilePath)
	assert.Equal(t, *want.FilePath, *got.FilePath)
	assert.Nil(t, got.FileHash)
	assert.Nil(t, got.LocalModifiedAt)

	listed, err := store.GetSession(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, listed)
	assert.Nil(t, listed.FilePath)
}

func TestStoreGetSessionHidesTrash(t *testing.T) {
	ctx := context.Background()
	store, syncer, local := newPushedStore(t)
	require.NoError(t, local.SoftDeleteSession(t.Context(), fixtureBetaID))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	hidden, err := store.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	assert.Nil(t, hidden)

	full, err := store.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.DeletedAt)
	assert.Equal(t, fixtureBetaID, full.ID)
}

func TestStoreGetSessionVersionChangesAfterPush(t *testing.T) {
	ctx := context.Background()
	store, syncer, local := newPushedStore(t)
	count, version, ok := store.GetSessionVersion(t.Context(), fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 2, count)

	appendMessage(t, local, fixtureAlphaID, "a later message", "2026-01-10T00:03:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	count2, version2, ok := store.GetSessionVersion(t.Context(), fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 3, count2)
	assert.NotEqual(t, version, version2)
}

func TestStoreMessageWindowReportsRevisionWithRows(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()

	from := 0
	revision := ""
	msgs, err := store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
		From: &from, Limit: 10, Asc: true,
		Roles:            []string{"user", "assistant"},
		ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{msgs[0].Ordinal, msgs[1].Ordinal})
	assert.Equal(t, "1", revision,
		"linear page must report the fixture session revision")

	anchor := 1
	revision = ""
	msgs, err = store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
		Around: &anchor, Before: 5, After: 5,
		Roles:            []string{"user", "assistant"},
		ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{msgs[0].Ordinal, msgs[1].Ordinal})
	assert.Equal(t, "1", revision,
		"around window must report the fixture session revision")

	revision = ""
	msgs, err = store.GetMessagesWindow(ctx, "missing", db.MessageWindow{
		Around: &anchor, Before: 5, After: 5, ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Empty(t, msgs)
	assert.Empty(t, revision, "no rows means no revision to describe them")
}

func TestMessageHydration(t *testing.T) {
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	ctx := t.Context()
	require.NoError(t, EnsureSchemaOn(ctx, conn))
	for _, query := range []string{
		`INSERT INTO sessions (id,push_version,transcript_revision) VALUES ('hydration',1,'rev')`,
		`INSERT INTO messages (id,session_id,ordinal,role,content,timestamp,push_version) SELECT number+1,'hydration',number,arrayElement(['user','assistant','user','assistant','system','user','assistant','user','assistant','system','user','assistant'],number+1),'msg','2026-01-01 00:00:00',1 FROM numbers(12)`,
		`INSERT INTO tool_calls (message_id,session_id,message_ordinal,call_index,tool_name,category,input_json,result_content_length,push_version) SELECT number+1,'hydration',number,0,'Read','Read','x',1,1 FROM numbers(12)`,
		`INSERT INTO tool_result_events (session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,event_index,timestamp,push_version) SELECT 'hydration',number,0,'tool','completed','r',1,0,'2026-01-02 00:00:00',1 FROM numbers(12)`,
		`INSERT INTO tool_result_events (session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,event_index,timestamp,push_version) VALUES ('hydration',2,0,'tool','completed','later',5,1,'2026-01-03 00:00:00',1)`,
	} {
		_, err := conn.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	store := NewStoreFromDB(conn)
	from, anchor, empty := 7, 4, 9000
	for _, tc := range []struct {
		name   string
		window db.MessageWindow
		want   []int
		events uint64
	}{
		{"sparse", db.MessageWindow{Limit: 2, Asc: true, Roles: []string{"user"}}, []int{0, 2}, 3},
		{"descending", db.MessageWindow{From: &from, Limit: 2, Roles: []string{"user"}}, []int{7, 5}, 2},
		{"around", db.MessageWindow{Around: &anchor, Before: 1, After: 1, Roles: []string{"user"}}, []int{2, 4, 5}, 4},
		{"empty", db.MessageWindow{From: &empty, Asc: true, Limit: 2}, []int{}, 0},
		{"full", db.MessageWindow{}, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before []uint64
			for round := range 2 {
				queryID := fmt.Sprintf("hydration-%s-%s-%d", database, tc.name, round)
				queryCtx := chdriver.Context(ctx, chdriver.WithQueryID(queryID), chdriver.WithSettings(chdriver.Settings{"log_queries": 1}))
				var msgs []db.Message
				var err error
				if tc.name == "full" {
					msgs, err = store.GetAllMessages(queryCtx, "hydration")
				} else {
					revision := ""
					window := tc.window
					window.ObservedRevision = &revision
					msgs, err = store.GetMessagesWindow(queryCtx, "hydration", window)
					if len(tc.want) > 0 {
						assert.Equal(t, "rev", revision)
					}
				}
				require.NoError(t, err)
				ordinals := make([]int, len(msgs))
				for i, msg := range msgs {
					ordinals[i] = msg.Ordinal
					require.Len(t, msg.ToolCalls, 1)
					if msg.Ordinal == 2 {
						require.Len(t, msg.ToolCalls[0].ResultEvents, 2)
						assert.Equal(t, []string{"r", "later"}, []string{msg.ToolCalls[0].ResultEvents[0].Content, msg.ToolCalls[0].ResultEvents[1].Content})
						assert.Equal(t, []int{1, 5}, []int{msg.ToolCalls[0].ResultEvents[0].ContentLength, msg.ToolCalls[0].ResultEvents[1].ContentLength})
						assert.Equal(t, []int{0, 1}, []int{msg.ToolCalls[0].ResultEvents[0].EventIndex, msg.ToolCalls[0].ResultEvents[1].EventIndex})
						assert.Contains(t, msg.ToolCalls[0].ResultEvents[1].Timestamp, "2026-01-03")
					} else if msg.Ordinal != 11 {
						assert.Equal(t, "r", msg.ToolCalls[0].ResultContent)
					}
				}
				assert.Equal(t, tc.want, ordinals)
				_, err = conn.ExecContext(ctx, `SYSTEM FLUSH LOGS`)
				require.NoError(t, err)
				var calls, events, callBytes, eventBytes, callQueries, eventQueries uint64
				require.NoError(t, conn.QueryRowContext(ctx, `
					SELECT sumIf(result_rows,position(query,'FROM tool_calls')>0),sumIf(result_rows,position(query,'FROM tool_result_events')>0),
						sumIf(result_bytes,position(query,'FROM tool_calls')>0),sumIf(result_bytes,position(query,'FROM tool_result_events')>0),
						countIf(position(query,'FROM tool_calls')>0),countIf(position(query,'FROM tool_result_events')>0)
					FROM system.query_log WHERE current_database=currentDatabase() AND type='QueryFinish' AND is_initial_query=1 AND query_id=?`, queryID).Scan(&calls, &events, &callBytes, &eventBytes, &callQueries, &eventQueries))
				assert.Equal(t, uint64(len(tc.want)), calls)
				assert.Equal(t, tc.events, events)
				if len(tc.want) == 0 {
					assert.Zero(t, callQueries)
					assert.Zero(t, eventQueries)
				} else {
					assert.Equal(t, uint64(1), callQueries)
					assert.Equal(t, uint64(1), eventQueries)
				}
				bytes := []uint64{callBytes, eventBytes}
				if round == 0 {
					before = bytes
					for _, table := range []string{"tool_calls", "tool_result_events"} {
						column, ordinal := "input_json", "message_ordinal"
						if table == "tool_result_events" {
							column, ordinal = "content", "tool_call_message_ordinal"
						}
						_, err = conn.ExecContext(ctx, `INSERT INTO `+table+` SELECT * REPLACE (repeat('z',1000000) AS `+column+`,push_version+1 AS push_version) FROM `+table+` WHERE session_id='hydration' AND `+ordinal+`=11`)
						require.NoError(t, err)
					}
				} else if tc.name != "full" {
					assert.Equal(t, before, bytes)
				}
				t.Logf("round=%d call rows=%d event rows=%d result bytes=%v", round, calls, events, bytes)
			}
		})
	}
}
