package codexcloud

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// State is one row's sync state on the status board.
type State string

const (
	StateOffDisabledAgents State = "off_disabled_agents"
	StateOffDisabled       State = "off_disabled"
	StateOffNoSync         State = "off_no_sync"
	StateConfigError       State = "config_error"
	StateWaiting           State = "waiting"
	StateImporting         State = "importing"
	StateUpToDate          State = "up_to_date"
	StateRetrying          State = "retrying"
	StateError             State = "error"
)

// HomeStatus is one status row: a ChatGPT login with the Codex homes signed
// in to it, or one home with no usable login. Key is the account key, or
// the home path for a home with none.
type HomeStatus struct {
	Key           string     `json:"key"`
	Homes         []string   `json:"homes"`
	State         State      `json:"state"`
	Message       string     `json:"message,omitzero"`
	LastSuccess   *time.Time `json:"last_success,omitzero"`
	Imported      int        `json:"imported,omitzero"`
	Total         int        `json:"total,omitzero"`
	Found         int        `json:"found,omitzero"`
	RetryCount    int        `json:"retry_count,omitzero"`
	OldestFailure *time.Time `json:"oldest_failure,omitzero"`
	Live          string     `json:"live,omitzero"`
	PlanUsage     string     `json:"plan_usage,omitzero"`
}

// StatusBoard holds the latest row for each key. It is safe for
// concurrent use.
type StatusBoard struct {
	mu    sync.Mutex
	order []string
	rows  map[string]HomeStatus
	// live and plan hold each account's live token and plan usage lines
	// apart from its row, since sync replaces whole rows and these lines
	// are set on their own.
	live map[string]string
	plan map[string]string
}

// NewStatusBoard returns an empty board.
func NewStatusBoard() *StatusBoard {
	return &StatusBoard{rows: map[string]HomeStatus{}, live: map[string]string{}, plan: map[string]string{}}
}

// SetLive sets the live token line shown with the row for key. It shows
// once sync has set that row.
func (b *StatusBoard) SetLive(key, text string) { b.setLine(b.live, key, text) }

// SetPlanUsage sets the plan usage line shown with the row for key.
func (b *StatusBoard) SetPlanUsage(key, text string) { b.setLine(b.plan, key, text) }

func (b *StatusBoard) setLine(lines map[string]string, key, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if text == "" {
		delete(lines, key)
		return
	}
	lines[key] = text
}

// Set replaces the row with row.Key.
func (b *StatusBoard) Set(row HomeStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.rows[row.Key]; !ok {
		b.order = append(b.order, row.Key)
	}
	// A source row has no homes; the API still sends a list.
	row.Homes = append([]string{}, row.Homes...)
	b.rows[row.Key] = row
}

// Row returns the row with key.
func (b *StatusBoard) Row(key string) (HomeStatus, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	row, ok := b.rows[key]
	row.Homes = slices.Clone(row.Homes)
	if ok {
		row.Live, row.PlanUsage = b.live[key], b.plan[key]
	}
	return row, ok
}

// Snapshot returns every row in the order first set.
func (b *StatusBoard) Snapshot() []HomeStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := make([]HomeStatus, 0, len(b.order))
	for _, key := range b.order {
		row := b.rows[key]
		row.Homes = slices.Clone(row.Homes)
		row.Live, row.PlanUsage = b.live[key], b.plan[key]
		rows = append(rows, row)
	}
	return rows
}

// Message is the user-facing text for a failure. retryAt is used only for
// a rate limit.
func Message(f Failure, retryAt time.Time) string {
	switch f {
	case FailSignedOut:
		return "Signed out of ChatGPT in this Codex home. Run `codex login`, then AgentsView picks up the new login on its next sync."
	case FailTooLarge:
		return "A reply from OpenAI was larger than AgentsView accepts."
	case FailRefreshRequested:
		return "OpenAI asked for a fresh ChatGPT login in this Codex home. Open Codex or run `codex login`; AgentsView retries when the login changes."
	case FailRefused:
		return "OpenAI refused this request. AgentsView retries automatically. If this continues, check that this ChatGPT account can use Codex."
	case FailNetwork:
		return "Can't reach OpenAI. AgentsView retries automatically."
	case FailRateLimited:
		return "OpenAI is limiting requests from this account. AgentsView retries at " +
			retryAt.Local().Format("Jan 2 15:04") + "."
	case FailServer:
		return "OpenAI's service had an error. AgentsView retries automatically."
	case FailThread:
		return "OpenAI returned an error for this thread. It may have been deleted."
	case FailProtocol:
		return "OpenAI changed its service in a way this version of AgentsView can't read. Update AgentsView. Sessions already imported are safe."
	case FailKeyring:
		return "This Codex home keeps its ChatGPT login in the system keyring, which AgentsView does not read."
	case FailEphemeral:
		return "This Codex home does not save its ChatGPT login, so AgentsView cannot use it."
	default:
		return ""
	}
}

// Delete drops the row with key.
func (b *StatusBoard) Delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.rows[key]; ok {
		delete(b.rows, key)
		b.order = slices.DeleteFunc(b.order, func(k string) bool { return k == key })
	}
	delete(b.live, key)
	delete(b.plan, key)
}

// Keep drops every row whose key is not in keys.
func (b *StatusBoard) Keep(keys map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = slices.DeleteFunc(b.order, func(key string) bool {
		if keys[key] {
			return false
		}
		delete(b.rows, key)
		return true
	})
	for _, lines := range []map[string]string{b.live, b.plan} {
		for key := range lines {
			if !keys[key] {
				delete(lines, key)
			}
		}
	}
}

// StatusLines renders a row for `serve status`: one line for the state,
// then the live token and plan usage lines when set. Times are local.
func StatusLines(row HomeStatus) []string {
	label := strings.Join(row.Homes, ", ")
	if label == "" {
		label = row.Key
	}
	lines := []string{"codex-cloud " + label + ": " + stateText(row)}
	if row.Live != "" {
		lines = append(lines, "  "+row.Live)
	}
	if row.PlanUsage != "" {
		lines = append(lines, "  "+row.PlanUsage)
	}
	return lines
}

func stateText(row HomeStatus) string {
	switch row.State {
	case StateWaiting:
		return "Waiting for first sync"
	case StateImporting:
		if row.Total == 0 {
			return fmt.Sprintf("Importing, found %d threads so far", row.Found)
		}
		return fmt.Sprintf("Importing %d of %d threads", row.Imported, row.Total)
	case StateUpToDate:
		return "Up to date as of " + localTime(row.LastSuccess)
	case StateRetrying:
		return fmt.Sprintf("%d threads could not be imported (oldest failure %s). %s",
			row.RetryCount, localTime(row.OldestFailure), row.Message)
	case StateError:
		if row.LastSuccess == nil {
			return row.Message
		}
		return row.Message + " Last successful sync: " + localTime(row.LastSuccess) + "."
	case StateOffDisabledAgents, StateOffDisabled, StateOffNoSync, StateConfigError:
		return row.Message
	default:
		return row.Message
	}
}

func localTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("Jan 2 15:04")
}
