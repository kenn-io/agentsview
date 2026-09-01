package tui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type fakeDataClient struct {
	list            *service.SessionList
	detail          *service.SessionDetail
	messages        *service.MessageList
	page            PageData
	mutations       []Mutation
	find            []int
	filters         []service.MessageFilter
	getSessionFn    func(context.Context, string) (*service.SessionDetail, error)
	sessionExtrasFn func(context.Context, string) (SessionExtras, error)
	messagesFn      func(context.Context, string, service.MessageFilter) (*service.MessageList, error)
	loadPageFn      func(context.Context, Page, PageQuery) (PageData, error)
	settingsFn      func(context.Context) (*Settings, error)
	mutateFn        func(context.Context, Mutation) (string, error)
	pageUpdates     <-chan pageUpdate
}

func (f *fakeDataClient) ListSessions(context.Context, service.ListFilter) (*service.SessionList, error) {
	return f.list, nil
}

func (f *fakeDataClient) Settings(ctx context.Context) (*Settings, error) {
	if f.settingsFn != nil {
		return f.settingsFn(ctx)
	}
	return &Settings{}, nil
}

func (f *fakeDataClient) GetSession(ctx context.Context, id string) (*service.SessionDetail, error) {
	if f.getSessionFn != nil {
		return f.getSessionFn(ctx, id)
	}
	return f.detail, nil
}

func (f *fakeDataClient) SessionExtras(ctx context.Context, id string) (SessionExtras, error) {
	if f.sessionExtrasFn != nil {
		return f.sessionExtrasFn(ctx, id)
	}
	return SessionExtras{}, nil
}

func (f *fakeDataClient) Messages(ctx context.Context, id string, filter service.MessageFilter) (*service.MessageList, error) {
	if f.messagesFn != nil {
		return f.messagesFn(ctx, id, filter)
	}
	f.filters = append(f.filters, filter)
	return f.messages, nil
}
func (f *fakeDataClient) FindSession(context.Context, string, string) ([]int, error) {
	return f.find, nil
}

func TestDestructiveCommandsRequireConfirmation(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.page = PageTrash
	m.pageData.Trash = []db.Session{{ID: "discard-me"}}

	next, command := m.executeCommand("delete-permanent")
	got := next.(*model)

	require.Nil(t, command)
	require.NotNil(t, got.confirm)
	assert.Equal(t, Mutation{Kind: "delete-permanent", SessionID: "discard-me"}, *got.confirm)
}

func TestFindInSessionLoadsWindowAroundFirstMatch(t *testing.T) {
	fake := &fakeDataClient{
		find:     []int{17, 42},
		messages: &service.MessageList{Messages: []db.Message{{Ordinal: 17}}},
	}
	m := newModel(context.Background(), fake, Options{})
	m.generation = 3
	m.detail = &service.SessionDetail{Session: db.Session{ID: "s1"}}
	m.sessions = []db.Session{{ID: "s1"}}

	_, findCommand := m.executeCommand("find regression")
	require.NotNil(t, findCommand)
	findResult := findCommand().(findLoadedMsg)
	_, windowCommand := m.Update(findResult)
	require.NotNil(t, windowCommand)
	windowResult := windowCommand().(sessionLoadedMsg)

	require.NoError(t, windowResult.err)
	require.NotNil(t, windowResult.messages)
	assert.Equal(t, 17, windowResult.messages.Messages[0].Ordinal)
	require.Len(t, fake.filters, 1)
	require.NotNil(t, fake.filters[0].Around)
	assert.Equal(t, 17, *fake.filters[0].Around)
	assert.Equal(t, "transcript match 1 of 2", m.status)
}

func TestNextMessageFromContinuesFullPages(t *testing.T) {
	last := 399
	full := make([]db.Message, 200)
	for i := range full {
		full[i].Ordinal = i + 200
	}

	next := nextMessageFrom(&service.MessageList{Messages: full, LastOrdinal: &last}, 200)

	require.NotNil(t, next)
	assert.Equal(t, 400, *next)
	assert.Nil(t, nextMessageFrom(&service.MessageList{Messages: full[:199], LastOrdinal: &last}, 200))
}
func (*fakeDataClient) Search(context.Context, service.SearchRequest) (*service.SessionSearchResult, error) {
	return &service.SessionSearchResult{}, nil
}

func (f *fakeDataClient) LoadPage(ctx context.Context, page Page, query PageQuery) (PageData, error) {
	if f.loadPageFn != nil {
		return f.loadPageFn(ctx, page, query)
	}
	return f.page, nil
}

func (f *fakeDataClient) LoadPageUpdates(
	context.Context, Page, PageQuery,
) (<-chan pageUpdate, bool) {
	return f.pageUpdates, f.pageUpdates != nil
}
func (f *fakeDataClient) Mutate(
	ctx context.Context, mutation Mutation,
) (string, error) {
	if f.mutateFn != nil {
		return f.mutateFn(ctx, mutation)
	}
	f.mutations = append(f.mutations, mutation)
	return "done", nil
}
func (*fakeDataClient) WatchEvents(context.Context) (<-chan ServerEvent, error) {
	events := make(chan ServerEvent)
	close(events)
	return events, nil
}

func TestInitListsSessionsWhileStartupWorkContinues(t *testing.T) {
	settingsStarted := make(chan struct{})
	syncStarted := make(chan Mutation, 1)
	release := make(chan struct{})
	fake := &fakeDataClient{
		list: &service.SessionList{Sessions: []db.Session{{ID: "visible"}}},
		settingsFn: func(ctx context.Context) (*Settings, error) {
			close(settingsStarted)
			select {
			case <-release:
				return &Settings{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		mutateFn: func(ctx context.Context, mutation Mutation) (string, error) {
			syncStarted <- mutation
			select {
			case <-release:
				return "sync complete", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	m := newModel(t.Context(), fake, Options{
		ResolveReadOnly: true,
		StartupSync:     true,
	})

	command := m.Init()
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 4)
	results := make(chan tea.Msg, len(batch))
	for _, load := range batch {
		go func(cmd tea.Cmd) { results <- cmd() }(load)
	}

	select {
	case <-settingsStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "settings request did not start")
	}
	select {
	case mutation := <-syncStarted:
		assert.Equal(t, Mutation{Kind: "startup-sync"}, mutation)
	case <-time.After(time.Second):
		require.FailNow(t, "startup sync did not start")
	}
	var listed bool
	for range 2 {
		select {
		case result := <-results:
			if msg, ok := result.(sessionsLoadedMsg); ok {
				require.NotNil(t, msg.page)
				assert.Equal(t, "visible", msg.page.Sessions[0].ID)
				listed = true
			}
		case <-time.After(time.Second):
			require.FailNow(t, "session list waited for startup work")
		}
	}
	assert.True(t, listed)
	close(release)
}

func TestSettingsFailureSurvivesSuccessfulSessionLoad(t *testing.T) {
	m := newModel(t.Context(), &fakeDataClient{}, Options{})
	m.generation = 1

	_, _ = m.Update(settingsLoadedMsg{err: errors.New("settings unavailable")})
	_, _ = m.Update(sessionsLoadedMsg{
		generation: 1,
		page:       &service.SessionList{},
	})

	assert.Contains(t, m.renderFooter(120), "settings unavailable")
}

func TestStartupSyncFailureSurvivesSuccessfulPageLoad(t *testing.T) {
	m := newModel(t.Context(), &fakeDataClient{}, Options{})
	m.generation = 1

	_, _ = m.Update(mutationDoneMsg{
		kind: "startup-sync", err: errors.New("startup sync failed"),
	})
	_, _ = m.Update(pageLoadedMsg{
		generation: 1, page: PageDashboard, data: PageData{}, done: true,
	})

	assert.Contains(t, m.renderFooter(120), "startup sync failed")
}

func TestModelIgnoresStaleSessionPage(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.generation = 4

	next, _ := m.Update(sessionsLoadedMsg{
		generation: 3,
		page:       &service.SessionList{Sessions: []db.Session{{ID: "stale"}}},
	})

	got := next.(*model)
	assert.Empty(t, got.sessions)
	assert.Equal(t, uint64(4), got.generation)
}

func TestModelDoesNotReloadForServerHeartbeat(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.generation = 4
	events := make(chan ServerEvent)

	next, command := m.Update(eventMsg{
		event:  ServerEvent{Event: "heartbeat", Data: "2026-08-19T00:00:00Z"},
		events: events,
	})

	require.NotNil(t, command)
	assert.Equal(t, uint64(4), next.(*model).generation)
	assert.False(t, next.(*model).loading)
}

func TestReportPageDoesNotReloadForDataChangedEvent(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.page = PageDashboard
	m.generation = 4
	events := make(chan ServerEvent)

	next, command := m.Update(eventMsg{
		event:  ServerEvent{Event: "data_changed", Data: `{"scope":"messages"}`},
		events: events,
	})

	require.NotNil(t, command)
	assert.Equal(t, uint64(4), next.(*model).generation)
	assert.False(t, next.(*model).loading)
}

func TestModelLoadsSessionAndRendersSafeTranscript(t *testing.T) {
	name := "Build release"
	fake := &fakeDataClient{}
	m := newModel(context.Background(), fake, Options{})
	m.width, m.height, m.generation, m.loading = 130, 30, 1, true

	next, command := m.Update(sessionsLoadedMsg{
		generation: 1,
		page:       &service.SessionList{Sessions: []db.Session{{ID: "s1", Agent: "codex", Project: "app", DisplayName: &name, MessageCount: 1}}},
	})
	m = next.(*model)
	require.NotNil(t, command)
	fake.detail = &service.SessionDetail{Session: m.sessions[0]}
	fake.messages = &service.MessageList{Messages: []db.Message{{
		ID: 7, SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content: "# Done\n\x1b]52;c;ZXZpbA==\x07safe",
		ToolCalls: []db.ToolCall{{
			ToolName: "Read", Category: "read", InputJSON: `{"path":"README.md"}`,
			ResultContent: "contents\x1b]52;c;ZXZpbA==\x07",
		}},
	}}}
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	for _, load := range batch {
		next, _ = m.Update(load())
		m = next.(*model)
	}

	view := m.View()
	assert.Contains(t, view.Content, "Build release")
	assert.Contains(t, view.Content, "Done")
	assert.Contains(t, view.Content, "tool Read")
	assert.Contains(t, view.Content, "README.md")
	assert.NotContains(t, view.Content, "\x1b]52")
}

func TestSelectedSessionShowsPreviewWhileTranscriptLoads(t *testing.T) {
	name := "New session"
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus = 100, 30, 2
	m.sessions = []db.Session{{ID: "old"}, {ID: "new", DisplayName: &name, Agent: "codex", MessageCount: 73}}
	m.selected = 1
	m.detail = &service.SessionDetail{Session: db.Session{ID: "old"}}
	m.messages = []db.Message{{ID: 1, Content: "old transcript"}}

	command := m.loadSelectedSession()

	require.NotNil(t, command)
	require.NotNil(t, m.detail)
	assert.Equal(t, "new", m.detail.ID)
	assert.Empty(t, m.messages)
	assert.True(t, m.transcriptLoading)
	view := m.View().Content
	assert.Contains(t, view, "New session")
	assert.Contains(t, view, m.strings.Loading)
	assert.NotContains(t, view, "old transcript")
}

func TestSelectedSessionDisplaysMessagesBeforeSlowMetadata(t *testing.T) {
	started := make(chan string, 3)
	releaseSlow := make(chan struct{})
	wait := func(ctx context.Context, name string) error {
		started <- name
		select {
		case <-releaseSlow:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	filters := make(chan service.MessageFilter, 1)
	fake := &fakeDataClient{}
	fake.getSessionFn = func(ctx context.Context, id string) (*service.SessionDetail, error) {
		err := wait(ctx, "detail")
		return &service.SessionDetail{Session: db.Session{ID: id}}, err
	}
	fake.messagesFn = func(_ context.Context, _ string, filter service.MessageFilter) (*service.MessageList, error) {
		started <- "messages"
		filters <- filter
		return &service.MessageList{Messages: []db.Message{{ID: 1, Content: "ready"}}}, nil
	}
	fake.sessionExtrasFn = func(ctx context.Context, _ string) (SessionExtras, error) {
		err := wait(ctx, "extras")
		return SessionExtras{Timing: &db.SessionTimingSummary{}}, err
	}
	m := newModel(context.Background(), fake, Options{})
	m.sessions = []db.Session{{ID: "session", Agent: "codex"}}

	command := m.loadSelectedSession()
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	results := make(chan tea.Msg, len(batch))
	for _, load := range batch {
		go func() { results <- load() }()
	}
	seen := make(map[string]bool)
	for range 2 {
		select {
		case call := <-started:
			seen[call] = true
		case <-time.After(time.Second):
			close(releaseSlow)
			require.FailNow(t, "primary session requests did not start concurrently")
		}
	}
	assert.Equal(t, map[string]bool{"detail": true, "messages": true}, seen)

	next, extrasCommand := m.Update(<-results)
	m = next.(*model)
	require.NotNil(t, extrasCommand)
	require.Len(t, m.messages, 1)
	assert.Equal(t, "ready", m.messages[0].Content)
	assert.False(t, m.transcriptLoading)
	assert.Nil(t, m.extras.Timing)
	assert.Equal(t, initialMessageLimit, (<-filters).Limit)

	extrasResult := make(chan tea.Msg, 1)
	go func() { extrasResult <- extrasCommand() }()
	select {
	case call := <-started:
		assert.Equal(t, "extras", call)
	case <-time.After(time.Second):
		close(releaseSlow)
		require.FailNow(t, "session extras did not start after transcript loaded")
	}

	close(releaseSlow)
	next, _ = m.Update(<-results)
	m = next.(*model)
	next, _ = m.Update(<-extrasResult)
	m = next.(*model)
	assert.NotNil(t, m.extras.Timing)
}

func TestTranscriptMessageFilterOmitsToolContentWhenToolsHidden(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})

	visible := m.transcriptMessageFilter(service.MessageFilter{Limit: 50})
	assert.Nil(t, visible.ToolContent)

	m.showTools = false
	hidden := m.transcriptMessageFilter(service.MessageFilter{Limit: 50})
	require.NotNil(t, hidden.ToolContent)
	assert.False(t, *hidden.ToolContent)
}

func TestEnterFocusesLoadedSessionWithoutReloading(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.focus = 1
	m.sessions = []db.Session{{ID: "session"}}
	m.detail = &service.SessionDetail{Session: db.Session{ID: "session"}}

	next, command := m.updateKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))

	require.Nil(t, command)
	assert.Equal(t, 2, next.(*model).focus)
}

func TestEnterRetriesFailedSessionLoad(t *testing.T) {
	detailCalls, messageCalls := 0, 0
	fake := &fakeDataClient{
		getSessionFn: func(_ context.Context, id string) (*service.SessionDetail, error) {
			detailCalls++
			return &service.SessionDetail{Session: db.Session{ID: id}}, nil
		},
		messagesFn: func(_ context.Context, _ string, _ service.MessageFilter) (*service.MessageList, error) {
			messageCalls++
			return &service.MessageList{}, nil
		},
	}
	m := newModel(context.Background(), fake, Options{})
	m.focus, m.generation = 1, 1
	m.sessions = []db.Session{{ID: "session"}}
	_ = m.loadSelectedSession()
	_, _ = m.Update(sessionLoadedMsg{
		generation: 1, load: m.sessionLoadGeneration, sessionID: "session",
		initialMessages: true, err: errors.New("temporary failure"),
	})

	next, command := m.updateKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	m = next.(*model)
	require.NotNil(t, command)
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	for _, load := range batch {
		_ = load()
	}

	assert.Equal(t, 2, m.focus)
	assert.Equal(t, 1, detailCalls)
	assert.Equal(t, 1, messageCalls)
	assert.False(t, m.sessionLoadFailed)
}

func TestPersistedStateRoundTrip(t *testing.T) {
	path := t.TempDir() + "/tui-state.json"
	want := persistedState{
		Page: PageTrends, Project: "api", Terms: "latency, cache", IncludeAutomated: true,
		ActivityPreset: "month", TrendGranularity: "day", InsightType: "weekly",
	}

	require.NoError(t, saveState(path, want))
	got := loadState(path)
	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.Equal(t, want, got)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestExecuteCommandChangesFilters(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.filter.Cursor = "old-page"
	m.cursorHistory = []string{"older-page"}

	next, command := m.executeCommand("project agentsview")
	got := next.(*model)

	assert.Equal(t, "agentsview", got.filter.Project)
	assert.Equal(t, "agentsview", got.query.Project)
	assert.Empty(t, got.filter.Cursor)
	assert.Empty(t, got.cursorHistory)
	require.NotNil(t, command)
	_, ok := command().(sessionsLoadedMsg)
	assert.True(t, ok)
}

func TestWindowLoadSelectsAnchorOrdinal(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.generation = 2
	anchor := 42

	next, _ := m.Update(sessionLoadedMsg{
		generation: 2,
		messages: &service.MessageList{Messages: []db.Message{
			{Ordinal: 40}, {Ordinal: 41}, {Ordinal: 42}, {Ordinal: 43},
		}},
		anchor: &anchor,
	})

	assert.Equal(t, 2, next.(*model).messageSelected)
}

func TestReportPageDoesNotReuseStaleSessionSelection(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.sessions = []db.Session{{ID: "stale"}}
	m.page = PageInsights

	assert.Empty(t, m.selectedSessionID())
}

func TestReportPagesScrollWithoutSessionRows(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.page = PageTrends
	m.focus = 1
	m.pageData.Trends = &db.TrendsTermsResponse{}

	next, command := m.move(1)

	require.Nil(t, command)
	assert.Equal(t, 1, next.(*model).scroll)
}

func TestSwitchPageKeepsCachedContentVisibleDuringRefresh(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height = 100, 30
	m.page, m.generation = PageDashboard, 2

	next, _ := m.Update(pageLoadedMsg{
		generation: 2,
		page:       PageDashboard,
		data: PageData{Analytics: &db.AnalyticsSummary{
			TotalSessions: 73,
		}},
	})
	m = next.(*model)
	_, _ = m.switchPage(PageUsage)
	next, command := m.switchPage(PageDashboard)
	m = next.(*model)

	require.NotNil(t, command)
	assert.True(t, m.loading)
	report := m.renderReport(80, 20)
	assert.Contains(t, report, "73")
	assert.NotContains(t, report, m.strings.Loading)
}

func TestDashboardDisplaysSurfacesAsTheyLoad(t *testing.T) {
	updates := make(chan pageUpdate, 2)
	m := newModel(context.Background(), &fakeDataClient{pageUpdates: updates}, Options{})
	m.width, m.height, m.page = 100, 30, PageDashboard
	command := m.loadCurrent()
	updates <- pageUpdate{Data: PageData{Analytics: &db.AnalyticsSummary{TotalSessions: 73}}}

	first := command()
	next, command := m.Update(first)
	m = next.(*model)

	require.NotNil(t, command)
	assert.True(t, m.loading)
	assert.Contains(t, m.renderReport(80, 20), "73")

	updates <- pageUpdate{
		Data: PageData{Analytics: &db.AnalyticsSummary{TotalSessions: 74}},
		Done: true,
	}
	second := command()
	next, command = m.Update(second)
	m = next.(*model)

	assert.Nil(t, command)
	assert.False(t, m.loading)
	assert.Contains(t, m.renderReport(80, 20), "74")
}

func TestSwitchPageCancelsPreviousPageLoad(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	fake := &fakeDataClient{
		loadPageFn: func(ctx context.Context, page Page, _ PageQuery) (PageData, error) {
			if page == PageDashboard {
				started <- struct{}{}
				<-ctx.Done()
				canceled <- struct{}{}
				return PageData{}, ctx.Err()
			}
			return PageData{}, nil
		},
	}
	m := newModel(context.Background(), fake, Options{})
	m.page = PageDashboard
	first := m.loadCurrent()
	done := make(chan struct{})
	go func() {
		_ = first()
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow(t, "dashboard load did not start")
	}

	_, second := m.switchPage(PageUsage)

	require.NotNil(t, second)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		require.FailNow(t, "dashboard load was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "canceled dashboard load did not return")
	}
}

func TestAnalyticsLinesIgnoreNilAgentRows(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.pageData.Analytics = &db.AnalyticsSummary{
		Agents: map[string]*db.AgentSummary{"codex": nil},
	}

	var lines []string
	require.NotPanics(t, func() { lines = m.analyticsLines() })
	assert.NotEmpty(t, lines)
}

func TestModelIgnoresDetailForPreviouslySelectedSession(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.generation = 4
	m.sessions = []db.Session{{ID: "current"}}

	next, _ := m.Update(sessionLoadedMsg{
		generation: 4, sessionID: "old",
		detail: &service.SessionDetail{Session: db.Session{ID: "old"}},
	})

	assert.Nil(t, next.(*model).detail)
}

func TestSystemStringsUseSupportedLocale(t *testing.T) {
	t.Setenv("LC_ALL", "zh_TW.UTF-8")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "")

	strings := systemStrings()

	assert.Equal(t, "工作階段", strings.PageNames[PageSessions])
	assert.Equal(t, "唯讀", strings.ReadOnly)
}

func TestGitHubTokenUsesMaskedInput(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})

	next, command := m.executeCommand("github-token")
	got := next.(*model)

	require.NotNil(t, command)
	assert.Equal(t, "github-token", got.inputMode)
	assert.Equal(t, textinput.EchoPassword, got.input.EchoMode)
}

var _ tea.Model = (*model)(nil)
