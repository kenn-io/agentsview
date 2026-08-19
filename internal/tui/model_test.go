package tui

import (
	"context"
	"os"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type fakeDataClient struct {
	list      *service.SessionList
	detail    *service.SessionDetail
	messages  *service.MessageList
	page      PageData
	mutations []Mutation
	find      []int
	filters   []service.MessageFilter
}

func (f *fakeDataClient) ListSessions(context.Context, service.ListFilter) (*service.SessionList, error) {
	return f.list, nil
}
func (f *fakeDataClient) GetSession(context.Context, string) (*service.SessionDetail, error) {
	return f.detail, nil
}
func (*fakeDataClient) SessionExtras(context.Context, string) (SessionExtras, error) {
	return SessionExtras{}, nil
}
func (f *fakeDataClient) Messages(_ context.Context, _ string, filter service.MessageFilter) (*service.MessageList, error) {
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

	next := nextMessageFrom(&service.MessageList{Messages: full, LastOrdinal: &last})

	require.NotNil(t, next)
	assert.Equal(t, 400, *next)
	assert.Nil(t, nextMessageFrom(&service.MessageList{Messages: full[:199], LastOrdinal: &last}))
}
func (*fakeDataClient) Search(context.Context, service.SearchRequest) (*service.SessionSearchResult, error) {
	return &service.SessionSearchResult{}, nil
}
func (f *fakeDataClient) LoadPage(context.Context, Page, PageQuery) (PageData, error) {
	return f.page, nil
}
func (f *fakeDataClient) Mutate(_ context.Context, mutation Mutation) (string, error) {
	f.mutations = append(f.mutations, mutation)
	return "done", nil
}
func (*fakeDataClient) WatchEvents(context.Context) (<-chan ServerEvent, error) {
	events := make(chan ServerEvent)
	close(events)
	return events, nil
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
	loaded := command().(sessionLoadedMsg)
	next, _ = m.Update(loaded)
	m = next.(*model)

	view := m.View()
	assert.Contains(t, view.Content, "Build release")
	assert.Contains(t, view.Content, "Done")
	assert.Contains(t, view.Content, "tool Read")
	assert.Contains(t, view.Content, "README.md")
	assert.NotContains(t, view.Content, "\x1b]52")
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
