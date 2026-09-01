package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type model struct {
	ctx                                                context.Context
	client                                             DataClient
	statePath                                          string
	strings                                            stringsTable
	readOnly, resolveReadOnly, startupSync             bool
	page                                               Page
	navIndex, focus, selected, messageSelected, scroll int
	width, height                                      int
	loading                                            bool
	generation                                         uint64
	status, errText                                    string
	startupSettingsErr, startupSyncErr                 string
	inputMode                                          string
	input                                              textinput.Model
	help                                               bool
	confirm                                            *Mutation
	query                                              PageQuery
	filter                                             service.ListFilter
	sessions                                           []db.Session
	searchResults                                      []db.SearchResult
	nextCursor                                         string
	searchCursor, nextSearchCursor                     int
	cursorHistory                                      []string
	searchHistory                                      []int
	detail                                             *service.SessionDetail
	messages                                           []db.Message
	extras                                             SessionExtras
	messageCount                                       int
	nextMessageOrdinal                                 *int
	findMatches                                        []int
	findIndex                                          int
	pageData                                           PageData
	pageCache                                          map[Page]PageData
	reportCache                                        map[Page]renderedReport
	open                                               func(string) error
	theme, messageLayout                               string
	highContrast                                       bool
	showThinking, showTools                            bool
	renderedMessages                                   map[renderedMessageKey]renderedMessage
	renderedMessageBytes                               int
	renderedMessageTick                                uint64
	markdownRenderers                                  map[markdownRendererKey]*glamour.TermRenderer
	transcriptLoading                                  bool
	sessionLoadFailed                                  bool
	sessionLoadGeneration                              uint64
	sessionLoadContext                                 context.Context
	cancelSessionLoad                                  context.CancelFunc
	cancelPageLoad                                     context.CancelFunc
}

type renderedMessageKey struct {
	loadGeneration           uint64
	messageID                int64
	index, width, lineBudget int
	theme, layout            string
}

type renderedMessage struct {
	content                string
	lines                  []string
	sourceBytes, sizeBytes int
	complete               bool
	lastUsed               uint64
}

type markdownRendererKey struct {
	width int
	theme string
}

type renderedReport struct {
	selected                              int
	theme, messageLayout                  string
	highContrast, showThinking, showTools bool
	lines                                 []string
}

type sessionsLoadedMsg struct {
	generation uint64
	page       *service.SessionList
	search     *service.SessionSearchResult
	err        error
}

type sessionLoadedMsg struct {
	generation      uint64
	load            uint64
	sessionID       string
	detail          *service.SessionDetail
	messages        *service.MessageList
	anchor          *int
	append          bool
	initialMessages bool
	pageSize        int
	err             error
}

type sessionExtrasLoadedMsg struct {
	generation uint64
	load       uint64
	sessionID  string
	extras     SessionExtras
	err        error
}

type settingsLoadedMsg struct {
	settings *Settings
	err      error
}

type findLoadedMsg struct {
	generation uint64
	ordinals   []int
	err        error
}

type pageLoadedMsg struct {
	generation uint64
	page       Page
	data       PageData
	err        error
	done       bool
	updates    <-chan pageUpdate
}
type mutationDoneMsg struct {
	kind    string
	message string
	err     error
}
type eventConnectedMsg struct {
	events <-chan ServerEvent
	err    error
}
type eventMsg struct {
	event  ServerEvent
	events <-chan ServerEvent
}
type reconnectMsg struct{}

func newModel(ctx context.Context, client DataClient, opts Options) *model {
	state := loadState(opts.StatePath)
	if state.Page == "" {
		state.Page = PageSessions
	}
	nav := 0
	for i, page := range pages {
		if page == state.Page {
			nav = i
		}
	}
	m := &model{
		ctx: ctx, client: client, statePath: opts.StatePath, strings: systemStrings(),
		readOnly: opts.ReadOnly, resolveReadOnly: opts.ResolveReadOnly,
		startupSync: opts.StartupSync,
		page:        state.Page, navIndex: nav, selected: 0,
		query: PageQuery{
			Project: state.Project, Agent: state.Agent, Machine: state.Machine,
			From: state.From, To: state.To, Terms: state.Terms, Timezone: opts.Timezone,
			Model: state.Model, GitBranch: state.GitBranch,
			ExcludeProject: state.ExcludeProject, ExcludeAgent: state.ExcludeAgent,
			ExcludeModel: state.ExcludeModel, ActiveSince: state.ActiveSince,
			Termination: state.Termination, MinUserMessages: state.MinUserMessages,
			CompareDimension: state.CompareDimension, CompareLeft: state.CompareLeft,
			CompareRight: state.CompareRight, IncludeOneShot: state.IncludeOneShot,
			IncludeAutomated: state.IncludeAutomated,
			ActivityPreset:   state.ActivityPreset, ActivityDate: state.ActivityDate,
			ActivityBucket: state.ActivityBucket, ActivityAutomation: state.ActivityAutomation,
			TrendGranularity: state.TrendGranularity, InsightType: state.InsightType,
		},
		filter: service.ListFilter{
			Project: state.Project, ExcludeProject: state.ExcludeProject,
			Agent: state.Agent, Machine: state.Machine, GitBranch: state.GitBranch,
			Date: state.Date, DateFrom: state.From, DateTo: state.To,
			ActiveSince: state.ActiveSince, MinMessages: state.MinMessages,
			MaxMessages: state.MaxMessages, MinUserMessages: state.MinUserMessages,
			IncludeOneShot: state.IncludeOneShot, IncludeAutomated: state.IncludeAutomated,
			IncludeChildren: state.IncludeChildren, Outcome: state.Outcome,
			HealthGrade: state.HealthGrade, Termination: state.Termination,
			MinToolFailures: state.MinToolFailures, HasSecret: state.HasSecret,
			Starred: state.Starred, OrderBy: state.OrderBy,
			Limit: 100,
		},
		open: opts.Open, theme: state.Theme, messageLayout: state.MessageLayout,
		highContrast: state.HighContrast,
		showThinking: !state.HideThinking, showTools: !state.HideTools,
		pageCache: make(map[Page]PageData), reportCache: make(map[Page]renderedReport),
	}
	m.input = textinput.New()
	m.input.CharLimit = 4096
	if m.theme == "" {
		m.theme = "auto"
	}
	if m.messageLayout == "" {
		m.messageLayout = "default"
	}
	return m
}

func (m *model) Init() tea.Cmd {
	commands := []tea.Cmd{
		m.loadCurrent(),
		connectEventsCmd(m.ctx, m.client),
	}
	if m.resolveReadOnly {
		commands = append(commands, m.loadSettings())
	}
	if m.startupSync {
		commands = append(commands, m.mutate(Mutation{Kind: "startup-sync"}))
	}
	return tea.Batch(commands...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case sessionsLoadedMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.stopPageLoad()
		m.loading = false
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		m.errText = ""
		if msg.search != nil {
			m.searchResults = msg.search.Results
			m.nextSearchCursor = msg.search.NextCursor
			m.sessions = nil
		} else if msg.page != nil {
			m.sessions = msg.page.Sessions
			m.nextCursor = msg.page.NextCursor
			m.searchResults = nil
		}
		m.clampSelection()
		return m, m.loadSelectedSession()
	case sessionLoadedMsg:
		if msg.generation != m.generation || msg.load != m.sessionLoadGeneration {
			return m, nil
		}
		if msg.sessionID != "" && msg.sessionID != m.selectedSessionID() {
			return m, nil
		}
		if msg.initialMessages {
			m.transcriptLoading = false
		}
		if msg.err != nil {
			m.sessionLoadFailed = true
			m.errText = msg.err.Error()
			return m, nil
		}
		if msg.detail != nil {
			m.detail = msg.detail
			m.messageCount = msg.detail.MessageCount
		}
		if msg.messages != nil {
			if msg.append {
				m.messages = append(m.messages, msg.messages.Messages...)
			} else {
				m.messages = msg.messages.Messages
				m.clearRenderedMessages()
			}
			m.nextMessageOrdinal = nextMessageFrom(msg.messages, msg.pageSize)
			if !msg.append {
				m.messageSelected = 0
				if msg.anchor != nil {
					for i, message := range m.messages {
						if message.Ordinal == *msg.anchor {
							m.messageSelected = i
							break
						}
					}
				}
			}
		}
		if msg.initialMessages {
			return m, m.loadSessionExtras(msg.sessionID, msg.generation, msg.load)
		}
		return m, nil
	case sessionExtrasLoadedMsg:
		if msg.generation != m.generation || msg.load != m.sessionLoadGeneration {
			return m, nil
		}
		if msg.sessionID != m.selectedSessionID() {
			return m, nil
		}
		if msg.err != nil {
			m.sessionLoadFailed = true
			m.errText = msg.err.Error()
			return m, nil
		}
		m.extras = msg.extras
		return m, nil
	case settingsLoadedMsg:
		if msg.err != nil {
			m.startupSettingsErr = msg.err.Error()
			return m, nil
		}
		m.startupSettingsErr = ""
		if msg.settings != nil {
			m.readOnly = msg.settings.ReadOnly
		}
		return m, nil
	case findLoadedMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		m.findMatches, m.findIndex = msg.ordinals, 0
		if len(msg.ordinals) == 0 {
			m.status = "no transcript matches"
			return m, nil
		}
		m.status = fmt.Sprintf("transcript match 1 of %d", len(msg.ordinals))
		return m, m.loadMessageWindow(msg.ordinals[0])
	case pageLoadedMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		done := msg.done || msg.updates == nil
		m.loading = !done
		if done {
			m.stopPageLoad()
		}
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		m.pageData = msg.data
		m.pageCache[msg.page] = msg.data
		delete(m.reportCache, msg.page)
		m.errText = ""
		if msg.data.Settings != nil {
			m.readOnly = msg.data.Settings.ReadOnly
			m.startupSettingsErr = ""
		}
		if !done {
			return m, waitPageUpdateCmd(msg.generation, msg.page, msg.updates)
		}
		return m, nil
	case mutationDoneMsg:
		m.loading = false
		if msg.kind == "startup-sync" {
			if msg.err != nil {
				m.startupSyncErr = msg.err.Error()
				return m, nil
			}
			m.startupSyncErr = ""
			m.status = msg.message
			return m, m.loadCurrent()
		}
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		if msg.kind == "sync" || msg.kind == "resync" {
			m.startupSyncErr = ""
		}
		m.status, m.errText = msg.message, ""
		if looksLikeLocation(msg.message) && m.open != nil {
			if err := m.open(msg.message); err != nil {
				m.errText = err.Error()
			}
		}
		return m, m.loadCurrent()
	case eventConnectedMsg:
		if msg.err != nil {
			return m, reconnectEventsCmd()
		}
		return m, waitEventCmd(msg.events)
	case eventMsg:
		if msg.event.Event != "data_changed" || m.page != PageSessions {
			return m, waitEventCmd(msg.events)
		}
		return m, tea.Batch(m.loadCurrent(), waitEventCmd(msg.events))
	case reconnectMsg:
		return m, connectEventsCmd(m.ctx, m.client)
	}
	return m, nil
}

func (m *model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.inputMode != "" {
		return m.updateInput(msg)
	}
	if m.confirm != nil {
		switch key {
		case "y", "Y", "enter":
			mutation := *m.confirm
			m.confirm = nil
			return m, m.mutate(mutation)
		case "n", "N", "esc":
			m.confirm = nil
		}
		return m, nil
	}
	if m.help {
		if key == "?" || key == "esc" || key == "q" {
			m.help = false
		}
		return m, nil
	}
	switch key {
	case "ctrl+c", "q":
		m.persist()
		return m, tea.Quit
	case "?":
		m.help = true
	case "tab":
		m.focus = (m.focus + 1) % m.paneCount()
	case "shift+tab":
		m.focus--
		if m.focus < 0 {
			m.focus = m.paneCount() - 1
		}
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "g", "home":
		if m.page == PageSessions && m.focus == 2 {
			m.messageSelected, m.scroll = 0, 0
			return m, nil
		}
		if m.page != PageSessions {
			m.setSelection(0)
			m.scroll = 0
			return m, nil
		}
		m.setSelection(0)
		return m, m.loadSelectedSession()
	case "G", "end":
		if m.page == PageSessions && m.focus == 2 {
			m.messageSelected = max(0, len(m.messages)-1)
			m.scroll = m.messageSelected
			return m, nil
		}
		if m.page != PageSessions {
			m.setSelection(m.itemCount() - 1)
			m.scroll = max(m.selected, m.reportLineCount()-1)
			return m, nil
		}
		m.setSelection(m.itemCount() - 1)
		return m, m.loadSelectedSession()
	case "enter":
		if m.focus == 0 {
			return m.switchPage(pages[m.navIndex])
		}
		if m.page == PageSessions {
			id := m.selectedSessionID()
			if id == "" {
				return m, nil
			}
			m.focus = 2
			if m.detail != nil && m.detail.ID == id && !m.sessionLoadFailed {
				return m, nil
			}
			return m, m.loadSelectedSession()
		}
		if m.page == PagePinned && m.selected >= 0 && m.selected < len(m.pageData.Pins) {
			pin := m.pageData.Pins[m.selected]
			return m.openReferencedSession(pin.SessionID, pin.Ordinal)
		}
		if m.page == PageRecentEdits && m.pageData.RecentEdits != nil && m.selected >= 0 && m.selected < len(m.pageData.RecentEdits.Files) {
			file := m.pageData.RecentEdits.Files[m.selected]
			ordinal := 0
			if len(file.Edits) > 0 {
				ordinal = file.Edits[0].Ordinal
			}
			return m.openReferencedSession(file.LastSessionID, ordinal)
		}
	case "/":
		return m, m.beginInput("search")
	case ":":
		return m, m.beginInput("command")
	case "n", "pgdown":
		if m.page == PageSessions && m.focus == 2 && m.nextMessageOrdinal != nil {
			return m, m.loadMoreMessages()
		}
		return m.nextPage()
	case "p", "pgup":
		return m.previousPage()
	case "r":
		return m, m.loadCurrent()
	case "d":
		if m.page == PageSessions {
			id := m.selectedSessionID()
			if id == "" || m.readOnly {
				return m, nil
			}
			mutation := Mutation{Kind: "delete", SessionID: id}
			m.confirm = &mutation
		}
	case "s":
		if m.page == PageSessions {
			id := m.selectedSessionID()
			if id == "" || m.readOnly {
				return m, nil
			}
			return m, m.mutate(Mutation{Kind: "star", SessionID: id})
		}
	case "]":
		return m.nextFindMatch(1)
	case "[":
		return m.nextFindMatch(-1)
	}
	return m, nil
}

func (m *model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.inputMode = ""
		m.input.EchoMode = textinput.EchoNormal
		m.input.Blur()
		m.input.Reset()
		return m, nil
	case "enter":
		mode, value := m.inputMode, strings.TrimSpace(m.input.Value())
		m.inputMode = ""
		m.input.Blur()
		m.input.Reset()
		m.input.EchoMode = textinput.EchoNormal
		if mode == "github-token" {
			return m, m.mutate(Mutation{Kind: "github-token", Value: value})
		}
		if mode == "search" {
			if m.page == PageSessions {
				m.query.Search = value
				m.searchCursor, m.nextSearchCursor = 0, 0
				m.cursorHistory, m.searchHistory = nil, nil
			}
			if m.page == PageRecentEdits {
				m.query.Search = value
				m.query.Offset = 0
			}
			if m.page == PageTrends {
				m.query.Terms = value
			}
			return m, m.loadCurrent()
		}
		return m.executeCommand(value)
	default:
		var command tea.Cmd
		m.input, command = m.input.Update(msg)
		return m, command
	}
}

func (m *model) beginInput(mode string) tea.Cmd {
	m.inputMode = mode
	m.input.Reset()
	m.input.EchoMode = textinput.EchoNormal
	if mode == "github-token" {
		m.input.EchoMode = textinput.EchoPassword
	}
	return m.input.Focus()
}

func (m *model) executeCommand(command string) (tea.Model, tea.Cmd) {
	name, value, _ := strings.Cut(command, " ")
	value = strings.TrimSpace(value)
	switch name {
	case "q", "quit":
		m.persist()
		return m, tea.Quit
	case "project":
		m.query.Project, m.filter.Project = value, value
	case "exclude-project":
		m.query.ExcludeProject, m.filter.ExcludeProject = value, value
	case "agent":
		m.query.Agent, m.filter.Agent = value, value
	case "exclude-agent":
		m.query.ExcludeAgent = value
	case "machine":
		m.query.Machine, m.filter.Machine = value, value
	case "model":
		m.query.Model = value
	case "exclude-model":
		m.query.ExcludeModel = value
	case "branch":
		m.query.GitBranch, m.filter.GitBranch = value, value
	case "date":
		m.filter.Date = value
	case "from":
		m.query.From, m.filter.DateFrom = value, value
	case "to":
		m.query.To, m.filter.DateTo = value, value
	case "active-since":
		m.query.ActiveSince, m.filter.ActiveSince = value, value
	case "terms":
		m.query.Terms = value
	case "activity-preset":
		if value != "day" && value != "week" && value != "month" && value != "custom" {
			m.errText = "activity-preset must be day, week, month, or custom"
			return m, nil
		}
		m.query.ActivityPreset = value
	case "activity-date":
		m.query.ActivityDate = value
	case "activity-bucket":
		if value != "" && value != "5m" && value != "15m" && value != "1h" && value != "1d" && value != "1w" {
			m.errText = "activity-bucket must be 5m, 15m, 1h, 1d, or 1w"
			return m, nil
		}
		m.query.ActivityBucket = value
	case "activity-automation":
		if value != "" && value != "all" && value != "interactive" && value != "automated" {
			m.errText = "activity-automation must be all, interactive, or automated"
			return m, nil
		}
		m.query.ActivityAutomation = value
	case "granularity":
		if value != "day" && value != "week" && value != "month" {
			m.errText = "granularity must be day, week, or month"
			return m, nil
		}
		m.query.TrendGranularity = value
	case "insight-type":
		m.query.InsightType = value
	case "termination":
		m.query.Termination, m.filter.Termination = value, value
	case "outcome":
		m.filter.Outcome = value
	case "health":
		m.filter.HealthGrade = value
	case "min-messages":
		parsed, err := parseNonNegative(value, "min-messages")
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		m.filter.MinMessages = parsed
	case "max-messages":
		parsed, err := parseNonNegative(value, "max-messages")
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		m.filter.MaxMessages = parsed
	case "min-user-messages":
		parsed, err := parseNonNegative(value, "min-user-messages")
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		m.filter.MinUserMessages, m.query.MinUserMessages = parsed, parsed
	case "min-failures":
		parsed, err := parseNonNegative(value, "min-failures")
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		m.filter.MinToolFailures = &parsed
	case "has-secret":
		m.filter.HasSecret = parseToggle(value, m.filter.HasSecret)
	case "starred":
		m.filter.Starred = parseToggle(value, m.filter.Starred)
	case "sort":
		m.filter.OrderBy = value
	case "compare":
		parts := strings.SplitN(value, "|", 3)
		if len(parts) != 3 || (parts[0] != "model" && parts[0] != "project") {
			m.errText = "compare requires model|LEFT|RIGHT or project|LEFT|RIGHT"
			return m, nil
		}
		m.query.CompareDimension, m.query.CompareLeft, m.query.CompareRight = parts[0], parts[1], parts[2]
	case "one-shot":
		m.filter.IncludeOneShot = parseToggle(value, m.filter.IncludeOneShot)
		m.query.IncludeOneShot = m.filter.IncludeOneShot
	case "automated":
		m.filter.IncludeAutomated = parseToggle(value, m.filter.IncludeAutomated)
		m.query.IncludeAutomated = m.filter.IncludeAutomated
	case "children":
		m.filter.IncludeChildren = parseToggle(value, m.filter.IncludeChildren)
	case "rename":
		if id := m.selectedSessionID(); id != "" {
			return m, m.mutate(Mutation{Kind: "rename", SessionID: id, Value: value})
		}
	case "star", "unstar", "open-session", "resume-session", "publish-session":
		if id := m.selectedSessionID(); id != "" {
			return m, m.mutate(Mutation{Kind: name, SessionID: id})
		}
	case "publish-secret":
		if id := m.selectedSessionID(); id != "" {
			return m, m.mutate(Mutation{Kind: "publish-session", SessionID: id, Flag: true})
		}
	case "delete":
		if id := m.selectedSessionID(); id != "" {
			mutation := Mutation{Kind: "delete", SessionID: id}
			m.confirm = &mutation
			return m, nil
		}
	case "restore":
		if id := m.selectedSessionID(); id != "" {
			return m, m.mutate(Mutation{Kind: name, SessionID: id})
		}
	case "delete-permanent":
		if id := m.selectedSessionID(); id != "" {
			mutation := Mutation{Kind: name, SessionID: id}
			m.confirm = &mutation
			return m, nil
		}
	case "pin", "unpin":
		if name == "unpin" && m.page == PagePinned && m.selected >= 0 && m.selected < len(m.pageData.Pins) {
			pin := m.pageData.Pins[m.selected]
			return m, m.mutate(Mutation{Kind: name, SessionID: pin.SessionID, MessageID: pin.MessageID})
		}
		if m.detail != nil && len(m.messages) > 0 {
			msg := m.messages[min(m.messageSelected, len(m.messages)-1)]
			return m, m.mutate(Mutation{Kind: name, SessionID: m.detail.ID, MessageID: msg.ID, Value: value})
		}
	case "sync", "resync", "embeddings-build", "worktrees-apply":
		return m, m.mutate(Mutation{Kind: name})
	case "empty-trash":
		mutation := Mutation{Kind: name}
		m.confirm = &mutation
		return m, nil
	case "terminal":
		return m, m.mutate(Mutation{Kind: "terminal-mode", Value: value})
	case "theme":
		if value != "auto" && value != "dark" && value != "light" {
			m.errText = "theme must be auto, dark, or light"
			return m, nil
		}
		m.theme = value
	case "contrast":
		m.highContrast = parseToggle(value, m.highContrast)
	case "layout":
		if value != "default" && value != "compact" && value != "stream" && value != "skim" {
			m.errText = "layout must be default, compact, stream, or skim"
			return m, nil
		}
		m.messageLayout = value
	case "thinking":
		m.showThinking = parseToggle(value, m.showThinking)
	case "tools":
		m.showTools = parseToggle(value, m.showTools)
	case "github-token":
		if value == "" {
			return m, m.beginInput("github-token")
		}
		return m, m.mutate(Mutation{Kind: name, Value: value})
	case "require-auth":
		return m, m.mutate(Mutation{Kind: name, Flag: parseToggle(value, false)})
	case "worktree-add", "worktree-update":
		return m, m.mutate(Mutation{Kind: name, Value: value})
	case "worktree-delete", "embeddings-activate", "embeddings-retire":
		id, force, err := parseIDAndForce(value)
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		mutation := Mutation{Kind: name, InsightID: id, Flag: force}
		if name == "worktree-delete" || name == "embeddings-retire" {
			m.confirm = &mutation
			return m, nil
		}
		return m, m.mutate(mutation)
	case "sync-remote":
		full := false
		if trimmed, ok := strings.CutSuffix(value, " force"); ok {
			value, full = strings.TrimSpace(trimmed), true
		}
		return m, m.mutate(Mutation{Kind: name, Value: value, Flag: full})
	case "import-claude", "import-chatgpt":
		return m, m.mutate(Mutation{Kind: name, Value: value})
	case "export-html", "export-markdown":
		if id := m.selectedSessionID(); id != "" {
			return m, m.mutate(Mutation{Kind: name, SessionID: id, Value: value})
		}
	case "generate-insight":
		return m, m.mutate(Mutation{Kind: name, Value: value})
	case "find":
		if id := m.selectedSessionID(); id != "" && value != "" {
			return m, m.findInSession(id, value)
		}
	case "open-link":
		if !looksLikeLocation(value) {
			m.errText = "open-link requires an http(s) URL or absolute path"
			return m, nil
		}
		if m.open != nil {
			if err := m.open(value); err != nil {
				m.errText = err.Error()
			}
		}
		return m, nil
	case "delete-insight", "publish-insight", "publish-insight-secret":
		if value == "" && m.page == PageInsights && m.selected >= 0 && m.selected < len(m.pageData.Insights) {
			value = strconv.FormatInt(m.pageData.Insights[m.selected].ID, 10)
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			m.errText = "insight ID must be a number"
			return m, nil
		}
		kind := name
		if name == "publish-insight-secret" {
			kind = "publish-insight"
		}
		mutation := Mutation{Kind: kind, InsightID: id, Flag: name == "publish-insight-secret"}
		if name == "delete-insight" {
			m.confirm = &mutation
			return m, nil
		}
		return m, m.mutate(mutation)
	case "export-insight-html", "export-insight-markdown":
		id, target, err := m.parseInsightExport(value)
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		return m, m.mutate(Mutation{Kind: name, InsightID: id, Value: target})
	case "page":
		page := Page(value)
		if validPage(page) {
			return m.switchPage(page)
		}
		m.errText = "unknown page: " + value
		return m, nil
	default:
		m.errText = "unknown command: " + name
		return m, nil
	}
	if isFilterCommand(name) {
		m.resetPagination()
	}
	m.persist()
	return m, m.loadCurrent()
}

func isFilterCommand(name string) bool {
	switch name {
	case "project", "exclude-project", "agent", "exclude-agent", "machine",
		"model", "exclude-model", "branch", "date", "from", "to",
		"active-since", "terms", "termination", "outcome", "health",
		"min-messages", "max-messages", "min-user-messages", "min-failures",
		"has-secret", "starred", "sort", "compare", "one-shot", "automated",
		"children", "activity-preset", "activity-date", "activity-bucket",
		"activity-automation", "granularity", "insight-type":
		return true
	default:
		return false
	}
}

func (m *model) resetPagination() {
	m.filter.Cursor, m.nextCursor = "", ""
	m.searchCursor, m.nextSearchCursor = 0, 0
	m.cursorHistory, m.searchHistory = nil, nil
	m.query.Offset = 0
}

func (m *model) parseInsightExport(value string) (int64, string, error) {
	idValue, target, hasTarget := strings.Cut(value, "|")
	idValue = strings.TrimSpace(idValue)
	if idValue == "" && m.page == PageInsights && m.selected >= 0 && m.selected < len(m.pageData.Insights) {
		return m.pageData.Insights[m.selected].ID, strings.TrimSpace(target), nil
	}
	id, err := strconv.ParseInt(idValue, 10, 64)
	if err != nil || id <= 0 {
		return 0, "", errors.New("insight export requires ID[|PATH]")
	}
	if !hasTarget {
		target = ""
	}
	return id, strings.TrimSpace(target), nil
}

func parseNonNegative(value, label string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, errors.New(label + " must be a non-negative number")
	}
	return parsed, nil
}

func parseToggle(value string, current bool) bool {
	switch strings.ToLower(value) {
	case "on", "true", "1", "yes":
		return true
	case "off", "false", "0", "no":
		return false
	default:
		return !current
	}
}

func parseIDAndForce(value string) (int64, bool, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, false, errors.New("numeric ID is required")
	}
	id, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, false, errors.New("ID must be a positive number")
	}
	return id, len(fields) > 1 && fields[1] == "force", nil
}

func (m *model) move(delta int) (tea.Model, tea.Cmd) {
	if m.focus == 0 {
		m.navIndex = clamp(m.navIndex+delta, 0, len(pages)-1)
		return m, nil
	}
	if m.page == PageSessions && m.focus == 2 {
		m.messageSelected = clamp(m.messageSelected+delta, 0, len(m.messages)-1)
		m.scroll = m.messageSelected
		return m, nil
	}
	if !m.pageHasSelection() {
		m.scroll = clamp(m.scroll+delta, 0, max(0, m.reportLineCount()-1))
		return m, nil
	}
	m.setSelection(m.selected + delta)
	if m.page == PageSessions {
		return m, m.loadSelectedSession()
	}
	m.scroll = m.selected
	return m, nil
}

func (m *model) switchPage(page Page) (tea.Model, tea.Cmd) {
	m.page, m.selected, m.scroll, m.focus = page, 0, 0, 1
	if page != PageSessions {
		m.stopSessionLoad()
	}
	m.pageData = m.pageCache[page]
	for i, candidate := range pages {
		if candidate == page {
			m.navIndex = i
		}
	}
	m.persist()
	return m, m.loadCurrent()
}

func (m *model) nextPage() (tea.Model, tea.Cmd) {
	if m.page == PageSessions {
		if m.query.Search != "" && m.nextSearchCursor > 0 {
			m.searchHistory = append(m.searchHistory, m.searchCursor)
			m.searchCursor = m.nextSearchCursor
			return m, m.loadCurrent()
		}
		if m.query.Search == "" && m.nextCursor != "" {
			m.cursorHistory = append(m.cursorHistory, m.filter.Cursor)
			m.filter.Cursor = m.nextCursor
			return m, m.loadCurrent()
		}
	}
	if m.page == PageRecentEdits && m.pageData.RecentEdits != nil && m.pageData.RecentEdits.HasMore {
		m.query.Offset += 50
		return m, m.loadCurrent()
	}
	return m, nil
}

func (m *model) previousPage() (tea.Model, tea.Cmd) {
	if m.page == PageSessions {
		if m.query.Search != "" && len(m.searchHistory) > 0 {
			last := len(m.searchHistory) - 1
			m.searchCursor = m.searchHistory[last]
			m.searchHistory = m.searchHistory[:last]
			return m, m.loadCurrent()
		}
		if m.query.Search == "" && len(m.cursorHistory) > 0 {
			last := len(m.cursorHistory) - 1
			m.filter.Cursor = m.cursorHistory[last]
			m.cursorHistory = m.cursorHistory[:last]
			return m, m.loadCurrent()
		}
	}
	if m.page == PageRecentEdits && m.query.Offset > 0 {
		m.query.Offset = max(0, m.query.Offset-50)
		return m, m.loadCurrent()
	}
	return m, nil
}

func (m *model) loadCurrent() tea.Cmd {
	m.stopPageLoad()
	loadCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	m.cancelPageLoad = cancel
	m.generation++
	gen := m.generation
	m.loading, m.errText = true, ""
	if m.page == PageSessions {
		filter, query, cursor := m.filter, m.query.Search, m.searchCursor
		return func() tea.Msg {
			if query != "" {
				result, err := m.client.Search(loadCtx, service.SearchRequest{Query: query, Project: filter.Project, Cursor: cursor, Limit: 100})
				return sessionsLoadedMsg{generation: gen, search: result, err: err}
			}
			page, err := m.client.ListSessions(loadCtx, filter)
			return sessionsLoadedMsg{generation: gen, page: page, err: err}
		}
	}
	page, query := m.page, m.query
	if client, ok := m.client.(progressiveDataClient); ok {
		if updates, supported := client.LoadPageUpdates(loadCtx, page, query); supported {
			return waitPageUpdateCmd(gen, page, updates)
		}
	}
	return func() tea.Msg {
		data, err := m.client.LoadPage(loadCtx, page, query)
		return pageLoadedMsg{generation: gen, page: page, data: data, err: err}
	}
}

func waitPageUpdateCmd(
	generation uint64, page Page, updates <-chan pageUpdate,
) tea.Cmd {
	return func() tea.Msg {
		update, ok := <-updates
		if !ok {
			return pageLoadedMsg{
				generation: generation, page: page, done: true,
				err: errors.New("page update stream ended before completion"),
			}
		}
		return pageLoadedMsg{
			generation: generation, page: page, data: update.Data, err: update.Err,
			done: update.Done, updates: updates,
		}
	}
}

func (m *model) stopPageLoad() {
	if m.cancelPageLoad != nil {
		m.cancelPageLoad()
		m.cancelPageLoad = nil
	}
}

const initialMessageLimit = 50

func (m *model) transcriptMessageFilter(
	filter service.MessageFilter,
) service.MessageFilter {
	if !m.showTools {
		include := false
		filter.ToolContent = &include
	}
	return filter
}

func (m *model) loadSelectedSession() tea.Cmd {
	id := m.selectedSessionID()
	if id == "" {
		m.stopSessionLoad()
		m.detail, m.messages, m.extras = nil, nil, SessionExtras{}
		m.nextMessageOrdinal = nil
		m.transcriptLoading = false
		m.sessionLoadFailed = false
		return nil
	}
	return m.loadSession(
		id,
		m.transcriptMessageFilter(service.MessageFilter{
			Limit: initialMessageLimit,
		}),
		nil,
	)
}

func (m *model) loadSession(id string, filter service.MessageFilter, anchor *int) tea.Cmd {
	m.stopSessionLoad()
	m.sessionLoadGeneration++
	load := m.sessionLoadGeneration
	loadCtx, cancel := context.WithCancel(m.ctx)
	m.sessionLoadContext, m.cancelSessionLoad = loadCtx, cancel
	gen := m.generation
	m.prepareSessionPreview(id)

	detail := func() tea.Msg {
		ctx, cancel := context.WithTimeout(loadCtx, 30*time.Second)
		defer cancel()
		result, err := m.client.GetSession(ctx, id)
		return sessionLoadedMsg{
			generation: gen, load: load, sessionID: id, detail: result, err: err,
		}
	}
	messages := func() tea.Msg {
		ctx, cancel := context.WithTimeout(loadCtx, 30*time.Second)
		defer cancel()
		result, err := m.client.Messages(ctx, id, filter)
		return sessionLoadedMsg{
			generation: gen, load: load, sessionID: id, messages: result,
			anchor: anchor, initialMessages: true, pageSize: filter.Limit, err: err,
		}
	}
	return tea.Batch(detail, messages)
}

func (m *model) loadSessionExtras(id string, generation, load uint64) tea.Cmd {
	loadCtx := m.sessionLoadContext
	if loadCtx == nil {
		loadCtx = m.ctx
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(loadCtx, 30*time.Second)
		defer cancel()
		result, err := m.client.SessionExtras(ctx, id)
		return sessionExtrasLoadedMsg{
			generation: generation, load: load, sessionID: id, extras: result, err: err,
		}
	}
}

func (m *model) prepareSessionPreview(id string) {
	preview := db.Session{ID: id}
	if m.query.Search != "" && m.selected >= 0 && m.selected < len(m.searchResults) {
		result := m.searchResults[m.selected]
		name := result.Name
		preview.Project, preview.Agent, preview.DisplayName = result.Project, result.Agent, &name
	} else if m.selected >= 0 && m.selected < len(m.sessions) && m.sessions[m.selected].ID == id {
		preview = m.sessions[m.selected]
	}
	m.detail = &service.SessionDetail{Session: preview}
	m.messages, m.extras = nil, SessionExtras{}
	m.messageCount = preview.MessageCount
	m.messageSelected, m.scroll = 0, 0
	m.nextMessageOrdinal = nil
	m.findMatches, m.findIndex = nil, 0
	m.clearRenderedMessages()
	m.transcriptLoading = true
	m.sessionLoadFailed = false
	m.errText = ""
}

func (m *model) stopSessionLoad() {
	if m.cancelSessionLoad != nil {
		m.cancelSessionLoad()
		m.cancelSessionLoad = nil
	}
	m.sessionLoadContext = nil
}

func (m *model) openReferencedSession(id string, ordinal int) (tea.Model, tea.Cmd) {
	m.page, m.navIndex, m.focus, m.selected = PageSessions, 0, 2, 0
	m.sessions = []db.Session{{ID: id}}
	m.searchResults = nil
	m.query.Search = ""
	m.generation++
	m.persist()
	return m, m.loadSession(id, m.transcriptMessageFilter(
		service.MessageFilter{
			Around: &ordinal, Before: new(50), After: new(50),
		},
	), &ordinal)
}

func (m *model) loadMoreMessages() tea.Cmd {
	if m.detail == nil || m.nextMessageOrdinal == nil {
		return nil
	}
	id, from, gen, load := m.detail.ID, *m.nextMessageOrdinal, m.generation, m.sessionLoadGeneration
	const pageSize = 200
	filter := m.transcriptMessageFilter(service.MessageFilter{
		From: &from, Limit: pageSize,
	})
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		messages, err := m.client.Messages(ctx, id, filter)
		return sessionLoadedMsg{
			generation: gen, load: load, sessionID: id, messages: messages,
			append: true, pageSize: pageSize, err: err,
		}
	}
}

func nextMessageFrom(messages *service.MessageList, pageSize int) *int {
	if messages == nil || messages.LastOrdinal == nil || pageSize <= 0 || len(messages.Messages) < pageSize {
		return nil
	}
	next := *messages.LastOrdinal + 1
	return &next
}

func (m *model) findInSession(id, query string) tea.Cmd {
	gen := m.generation
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		ordinals, err := m.client.FindSession(ctx, id, query)
		return findLoadedMsg{generation: gen, ordinals: ordinals, err: err}
	}
}

func (m *model) loadMessageWindow(ordinal int) tea.Cmd {
	if m.detail == nil {
		return nil
	}
	id, gen, load := m.detail.ID, m.generation, m.sessionLoadGeneration
	filter := m.transcriptMessageFilter(service.MessageFilter{
		Around: &ordinal, Before: new(25), After: new(25),
	})
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		messages, err := m.client.Messages(ctx, id, filter)
		return sessionLoadedMsg{generation: gen, load: load, sessionID: id, messages: messages, anchor: &ordinal, err: err}
	}
}

func (m *model) nextFindMatch(delta int) (tea.Model, tea.Cmd) {
	if len(m.findMatches) == 0 {
		return m, nil
	}
	m.findIndex = (m.findIndex + delta + len(m.findMatches)) % len(m.findMatches)
	m.status = fmt.Sprintf("transcript match %d of %d", m.findIndex+1, len(m.findMatches))
	return m, m.loadMessageWindow(m.findMatches[m.findIndex])
}

func (m *model) mutate(mutation Mutation) tea.Cmd {
	if m.readOnly {
		m.errText = "daemon is read-only"
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		message, err := m.client.Mutate(ctx, mutation)
		return mutationDoneMsg{
			kind: mutation.Kind, message: message, err: err,
		}
	}
}

func (m *model) loadSettings() tea.Cmd {
	client, ok := m.client.(interface {
		Settings(context.Context) (*Settings, error)
	})
	if !ok {
		return func() tea.Msg {
			return settingsLoadedMsg{err: errors.New("daemon settings unavailable")}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		settings, err := client.Settings(ctx)
		return settingsLoadedMsg{settings: settings, err: err}
	}
}

func (m *model) visibleError() string {
	if m.errText != "" {
		return m.errText
	}
	if m.startupSettingsErr != "" {
		return m.startupSettingsErr
	}
	return m.startupSyncErr
}

func connectEventsCmd(ctx context.Context, client DataClient) tea.Cmd {
	return func() tea.Msg {
		events, err := client.WatchEvents(ctx)
		return eventConnectedMsg{events: events, err: err}
	}
}

func waitEventCmd(events <-chan ServerEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return reconnectMsg{}
		}
		return eventMsg{event: event, events: events}
	}
}

func reconnectEventsCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return reconnectMsg{} })
}

func (m *model) selectedSessionID() string {
	if m.page == PageTrash {
		if m.selected >= 0 && m.selected < len(m.pageData.Trash) {
			return m.pageData.Trash[m.selected].ID
		}
		return ""
	}
	if m.page != PageSessions {
		return ""
	}
	if m.query.Search != "" {
		if m.selected >= 0 && m.selected < len(m.searchResults) {
			return m.searchResults[m.selected].SessionID
		}
		return ""
	}
	if m.selected >= 0 && m.selected < len(m.sessions) {
		return m.sessions[m.selected].ID
	}
	return ""
}

func (m *model) itemCount() int {
	switch m.page {
	case PageTrash:
		return len(m.pageData.Trash)
	case PageInsights:
		return len(m.pageData.Insights)
	case PagePinned:
		return len(m.pageData.Pins)
	case PageRecentEdits:
		if m.pageData.RecentEdits != nil {
			return len(m.pageData.RecentEdits.Files)
		}
		return 0
	case PageSessions:
		if m.query.Search != "" {
			return len(m.searchResults)
		}
		return len(m.sessions)
	}
	return 0
}

func (m *model) pageHasSelection() bool {
	switch m.page {
	case PageSessions, PageInsights, PagePinned, PageTrash, PageRecentEdits:
		return true
	default:
		return false
	}
}

func (m *model) reportLineCount() int {
	return len(m.reportLines())
}

func (m *model) reportLines() []string {
	if cached, ok := m.reportCache[m.page]; ok &&
		cached.selected == m.selected && cached.theme == m.theme &&
		cached.highContrast == m.highContrast && cached.messageLayout == m.messageLayout &&
		cached.showThinking == m.showThinking && cached.showTools == m.showTools {
		return cached.lines
	}
	var lines []string
	switch m.page {
	case PageDashboard:
		lines = m.analyticsLines()
	case PageUsage:
		lines = m.usageLines()
	case PageActivity:
		lines = m.activityLines()
	case PageTrends:
		lines = m.trendsLines()
	case PageInsights:
		lines = m.insightLines()
	case PagePinned:
		lines = m.pinLines()
	case PageTrash:
		lines = m.trashLines()
	case PageRecentEdits:
		lines = m.recentEditLines()
	case PageSettings:
		lines = m.settingsLines()
	}
	m.reportCache[m.page] = renderedReport{
		selected: m.selected, theme: m.theme, highContrast: m.highContrast,
		messageLayout: m.messageLayout, showThinking: m.showThinking,
		showTools: m.showTools, lines: lines,
	}
	return lines
}
func (m *model) setSelection(i int) { m.selected = clamp(i, 0, m.itemCount()-1) }
func (m *model) clampSelection()    { m.setSelection(m.selected) }
func (m *model) paneCount() int {
	if m.page == PageSessions {
		return 3
	}
	return 2
}
func (m *model) persist() {
	_ = saveState(m.statePath, persistedState{
		Page: m.page, Project: m.filter.Project, Agent: m.filter.Agent,
		Machine: m.filter.Machine, From: m.query.From, To: m.query.To,
		Date: m.filter.Date, Terms: m.query.Terms,
		ExcludeProject: m.filter.ExcludeProject, ExcludeAgent: m.query.ExcludeAgent,
		Model: m.query.Model, ExcludeModel: m.query.ExcludeModel,
		GitBranch: m.filter.GitBranch, ActiveSince: m.filter.ActiveSince,
		MinMessages: m.filter.MinMessages, MaxMessages: m.filter.MaxMessages,
		MinUserMessages: m.filter.MinUserMessages, Outcome: m.filter.Outcome,
		HealthGrade: m.filter.HealthGrade, Termination: m.filter.Termination,
		MinToolFailures: m.filter.MinToolFailures,
		HasSecret:       m.filter.HasSecret, Starred: m.filter.Starred,
		OrderBy: m.filter.OrderBy, CompareDimension: m.query.CompareDimension,
		CompareLeft: m.query.CompareLeft, CompareRight: m.query.CompareRight,
		IncludeOneShot:   m.filter.IncludeOneShot,
		IncludeAutomated: m.filter.IncludeAutomated,
		IncludeChildren:  m.filter.IncludeChildren, Theme: m.theme,
		ActivityPreset: m.query.ActivityPreset, ActivityDate: m.query.ActivityDate,
		ActivityBucket: m.query.ActivityBucket, ActivityAutomation: m.query.ActivityAutomation,
		TrendGranularity: m.query.TrendGranularity, InsightType: m.query.InsightType,
		HighContrast: m.highContrast, MessageLayout: m.messageLayout,
		HideThinking: !m.showThinking, HideTools: !m.showTools,
	})
}
func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	return min(max(value, low), high)
}
func looksLikeLocation(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "/")
}
