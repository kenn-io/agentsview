package claudeai

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/cloudsync/transport"
	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeBrowserPageHonorsExplicitHasMore(t *testing.T) {
	t.Parallel()
	items, hasMore, err := decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}],"has_more":true}`))
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.True(t, hasMore, "a short page must continue when Claude explicitly sets has_more")

	_, hasMore, err = decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}],"has_more":false}`))
	require.NoError(t, err)
	assert.False(t, hasMore)
}

func TestServiceStartReturnsRunningJobIdempotently(t *testing.T) {
	t.Parallel()
	service := NewService(transport.NewBroker(), nil, t.TempDir(), "local")
	t.Cleanup(service.Close)

	started, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	attached, err := service.Start(context.Background(), SyncIncremental)
	require.ErrorIs(t, err, ErrSyncAlreadyRunning)
	assert.Equal(t, started.ID, attached.ID)
	assert.Equal(t, "running", attached.Status)
}

func TestServiceRejectsReadOnlyStore(t *testing.T) {
	writable := serviceTestDB(t)
	path := writable.Path()
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	service := NewService(transport.NewBroker(), readonly, t.TempDir(), "local")
	t.Cleanup(service.Close)
	_, err = service.Start(context.Background(), SyncIncremental)
	require.EqualError(t, err, "cloud import not available in read-only mode")
}

func TestServiceRunsBeyondStartRequestContext(t *testing.T) {
	t.Parallel()
	broker := transport.NewBroker()
	service := NewService(broker, nil, t.TempDir(), "local")
	t.Cleanup(service.Close)
	requestContext, cancel := context.WithCancel(context.Background())
	_, err := service.Start(requestContext, SyncIncremental)
	require.NoError(t, err)
	cancel()

	claimContext, claimCancel := context.WithTimeout(context.Background(), time.Second)
	defer claimCancel()
	request, err := broker.Claim(claimContext)
	require.NoError(t, err)
	require.NoError(t, broker.Complete(transport.Response{ID: request.ID, Lease: request.Lease, Status: 200, Body: json.RawMessage(`{"conversations":[],"has_more":false}`)}))
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, time.Second, 10*time.Millisecond)
}

func TestSchedulePersistsAndCloseStopsActiveJob(t *testing.T) {
	root := t.TempDir()
	service := NewService(transport.NewBroker(), nil, root, "local")
	t.Cleanup(service.Close)
	require.NoError(t, service.ConfigureSchedule(ScheduleConfig{Enabled: true, IntervalMinutes: 60}))
	reloaded := NewService(transport.NewBroker(), nil, root, "local")
	t.Cleanup(reloaded.Close)
	assert.True(t, reloaded.Schedule().Enabled)
	assert.Equal(t, 60, reloaded.Schedule().IntervalMinutes)
	_, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	service.Close()
	require.Eventually(t, func() bool { return service.Status().Status == "cancelled" }, time.Second, 10*time.Millisecond)
}

func TestDecodeBrowserPageFallsBackWhenHasMoreIsAbsent(t *testing.T) {
	t.Parallel()
	_, hasMore, err := decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}]}`))
	require.NoError(t, err)
	assert.True(t, hasMore)
}

func TestServiceImportsExplicitlyPaginatedConversationPages(t *testing.T) {
	broker := transport.NewBroker()
	database := serviceTestDB(t)
	service := NewService(broker, database, t.TempDir(), "local")
	t.Cleanup(service.Close)

	var mu sync.Mutex
	var offsets []int
	stop := serveTransport(t, broker, func(request transport.Request) transport.Response {
		switch request.Operation {
		case OperationListConversations:
			var params ListParams
			require.NoError(t, json.Unmarshal(request.Params, &params))
			mu.Lock()
			offsets = append(offsets, params.Offset)
			mu.Unlock()
			if params.Offset == 0 {
				return transportResponse(request, 200, `{"conversations":[`+string(testSummary("one"))+`],"has_more":true}`)
			}
			return transportResponse(request, 200, `{"conversations":[`+string(testSummary("two"))+`],"has_more":false}`)
		case OperationGetConversation:
			var params DetailParams
			require.NoError(t, json.Unmarshal(request.Params, &params))
			return transportResponse(request, 200, string(testDetail(params.ConversationID, "content "+params.ConversationID)))
		default:
			return transportResponse(request, 400, `{}`)
		}
	})
	t.Cleanup(stop)

	_, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []int{0, 1}, offsets)
	assert.Equal(t, 2, service.Status().Imported)
	for _, id := range []string{"one", "two"} {
		session, err := database.GetSession(context.Background(), "claude-ai:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
	}
}

func TestServiceFetchesOnlyChangedConversations(t *testing.T) {
	root := t.TempDir()
	cached := BrowserConversation{Summary: testSummary("unchanged"), Conversation: testDetail("unchanged", "cached")}
	prepared, err := PrepareBrowserImport(root, []BrowserConversation{cached})
	require.NoError(t, err)
	require.NoError(t, prepared.Commit())

	broker := transport.NewBroker()
	service := NewService(broker, serviceTestDB(t), root, "local")
	t.Cleanup(service.Close)
	var detailIDs []string
	var mu sync.Mutex
	stop := serveTransport(t, broker, func(request transport.Request) transport.Response {
		switch request.Operation {
		case OperationListConversations:
			return transportResponse(request, 200, `{"conversations":[`+string(testSummary("unchanged"))+`,`+string(testSummary("changed"))+`],"has_more":false}`)
		case OperationGetConversation:
			var params DetailParams
			require.NoError(t, json.Unmarshal(request.Params, &params))
			mu.Lock()
			detailIDs = append(detailIDs, params.ConversationID)
			mu.Unlock()
			return transportResponse(request, 200, string(testDetail(params.ConversationID, "fresh")))
		default:
			return transportResponse(request, 400, `{}`)
		}
	})
	t.Cleanup(stop)

	_, err = service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"changed"}, detailIDs)
	assert.Equal(t, 1, service.Status().Changed)
	assert.Equal(t, 1, service.Status().Skipped)
}

func TestServiceFailedImportDoesNotCommitManifest(t *testing.T) {
	broker := transport.NewBroker()
	root := t.TempDir()
	service := NewService(broker, serviceTestDB(t), root, "local", func(func() error) error {
		return errors.New("archive write failed")
	})
	t.Cleanup(service.Close)
	stop := serveTransport(t, broker, func(request transport.Request) transport.Response {
		switch request.Operation {
		case OperationListConversations:
			return transportResponse(request, 200, `{"conversations":[`+string(testSummary("uncommitted"))+`],"has_more":false}`)
		case OperationGetConversation:
			return transportResponse(request, 200, string(testDetail("uncommitted", "content")))
		default:
			return transportResponse(request, 400, `{}`)
		}
	})
	t.Cleanup(stop)

	_, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return service.Status().Status == "failed" }, 2*time.Second, 10*time.Millisecond)
	manifest, err := readManifest(filepath.Join(root, cacheManifestName))
	require.NoError(t, err)
	assert.NotContains(t, manifest, "uncommitted")
}

func TestServiceRepairRestoresOnlyFetchedClaudeSession(t *testing.T) {
	broker := transport.NewBroker()
	database := serviceTestDB(t)
	service := NewService(broker, database, t.TempDir(), "local")
	t.Cleanup(service.Close)
	mode := SyncIncremental
	stop := serveTransport(t, broker, func(request transport.Request) transport.Response {
		switch request.Operation {
		case OperationListConversations:
			return transportResponse(request, 200, `{"conversations":[`+string(testSummary("repairable"))+`],"has_more":false}`)
		case OperationGetConversation:
			content := "initial"
			if mode == SyncRepair {
				content = "repaired"
			}
			return transportResponse(request, 200, string(testDetail("repairable", content)))
		default:
			return transportResponse(request, 400, `{}`)
		}
	})
	t.Cleanup(stop)

	_, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, database.DeleteSession("claude-ai:repairable"))
	require.True(t, database.IsSessionExcluded("claude-ai:repairable"))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "codex:unrelated", Project: "test", Machine: "local", Agent: "codex",
	}))
	require.NoError(t, database.DeleteSession("codex:unrelated"))
	require.True(t, database.IsSessionExcluded("codex:unrelated"))

	mode = SyncRepair
	_, err = service.Start(context.Background(), SyncRepair)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, 2*time.Second, 10*time.Millisecond)
	assert.False(t, database.IsSessionExcluded("claude-ai:repairable"))
	assert.True(t, database.IsSessionExcluded("codex:unrelated"))
	session, err := database.GetSession(context.Background(), "claude-ai:repairable")
	require.NoError(t, err)
	require.NotNil(t, session)
}

func serviceTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

func serveTransport(t *testing.T, broker *transport.Broker, handler func(transport.Request) transport.Response) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			request, err := broker.Claim(ctx)
			cancel()
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			response := handler(request)
			_ = broker.Complete(response)
		}
	}()
	return func() { close(done) }
}

func transportResponse(request transport.Request, status int, body string) transport.Response {
	return transport.Response{ID: request.ID, Lease: request.Lease, Status: status, Body: json.RawMessage(body)}
}

func testSummary(id string) json.RawMessage {
	return json.RawMessage(`{"uuid":"` + id + `","name":"` + id + `","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}`)
}

func testDetail(id, text string) json.RawMessage {
	return json.RawMessage(`{"uuid":"` + id + `","chat_messages":[{"uuid":"message-` + id + `","sender":"human","text":"` + text + `","created_at":"2025-01-01T00:00:00Z"}]}`)
}
