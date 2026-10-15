package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
)

// mirrorSuffix ends every mirror file; the provider discovers only these.
const mirrorSuffix = ".jsonl.zstd"

// threadStateSuffix ends every thread state file.
const threadStateSuffix = ".state.json"

// ListingID names one thread/list listing the fetcher walks.
type ListingID string

const (
	ListDefaultActive    ListingID = "default-active"
	ListDefaultArchived  ListingID = "default-archived"
	ListSubAgentActive   ListingID = "subagent-active"
	ListSubAgentArchived ListingID = "subagent-archived"
)

// TurnState is what the fetcher last stored for one turn.
type TurnState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// FirstSeen and EndedFirstSeen drive the 10-minute save-lag rules.
	FirstSeen      time.Time  `json:"first_seen,omitzero"`
	EndedFirstSeen time.Time  `json:"ended_first_seen,omitzero"`
	StartedAt      *time.Time `json:"started_at,omitzero"`
	CompletedAt    *time.Time `json:"completed_at,omitzero"`
	Digest         string     `json:"digest,omitzero"`
}

// ThreadState is one thread's sync state, kept in <threadId>.state.json.
type ThreadState struct {
	UpdatedAt      int64       `json:"updated_at,omitzero"`
	ResumeTurnID   string      `json:"resume_turn_id,omitzero"`
	ResumeItemID   string      `json:"resume_item_id,omitzero"`
	Offset         int64       `json:"offset,omitzero"`
	ResolvedParent string      `json:"resolved_parent,omitzero"`
	Turns          []TurnState `json:"turns,omitzero"`
	// LastProgress is the last new item or updatedAt change, for the
	// 24-hour stall rule.
	LastProgress time.Time         `json:"last_progress,omitzero"`
	MetaDigest   string            `json:"meta_digest,omitzero"`
	ItemDigests  map[string]string `json:"item_digests,omitzero"`
	CreatedAt    int64             `json:"created_at,omitzero"`
	// LastPlanRecordDigest and LastPlanRecordKind describe the newest plan
	// usage record of any kind in the mirror file.
	LastPlanRecordDigest string `json:"last_plan_record_digest,omitzero"`
	LastPlanRecordKind   string `json:"last_plan_record_kind,omitzero"`
	CountedUnder         string `json:"counted_under,omitzero"`
	// LastPlanRow is the newest plan usage row body in the file, kept so a
	// root that settles after being left out can store it again as final.
	LastPlanRow       jsontext.Value `json:"last_plan_row,omitzero"`
	LastHandledTurnID string         `json:"last_handled_turn_id,omitzero"`
	IsSubAgent        bool           `json:"is_sub_agent,omitzero"`
	ArchivedFirstSeen time.Time      `json:"archived_first_seen,omitzero"`
	// Listing is the newest listing record, so a mirror file rebuilt for a
	// thread outside this pass's listing pages still gets its metadata.
	Listing jsontext.Value `json:"listing,omitzero"`
}

// changedAt is the thread's last change: its newest updatedAt, or the
// last fetch that found something new, whichever is later. It is zero for
// a thread with neither.
func (st ThreadState) changedAt() time.Time {
	changed := st.LastProgress
	if st.UpdatedAt != 0 {
		if updated := time.Unix(st.UpdatedAt, 0); updated.After(changed) {
			changed = updated
		}
	}
	return changed
}

// WaitState is a per-account or per-home wait after a failure.
type WaitState struct {
	Failure  string    `json:"failure,omitzero"`
	Attempts int       `json:"attempts,omitzero"`
	Until    time.Time `json:"until,omitzero"`
}

// UsagePauseState pauses the plan usage step for an account: after a 429,
// or for 24 hours when OpenAI does not offer plan usage to it.
type UsagePauseState struct {
	Until    time.Time `json:"until,omitzero"`
	Reason   string    `json:"reason,omitzero"`
	Attempts int       `json:"attempts,omitzero"`
}

// PendingEntry is a usage root waiting for a usable row.
type PendingEntry struct {
	// Since orders pending roots, oldest first.
	Since       time.Time `json:"since,omitzero"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Attempts counts failed or unresolved attempts in a row.
	Attempts int `json:"attempts,omitzero"`
	// LastChange is the last fetch of the root or a descendant; settling
	// and "could not check" count 7 days from it.
	LastChange time.Time `json:"last_change,omitzero"`
	// LateFailures counts failed requests in a row after the settle point.
	LateFailures int `json:"late_failures,omitzero"`
	// HasRow marks a root that re-entered pending while it had a usable
	// row; Reentries doubles its re-entry wait.
	HasRow    bool `json:"has_row,omitzero"`
	Reentries int  `json:"reentries,omitzero"`
}

// RefreshEntry is a usage root with a usable row, refreshed daily until 7
// days after its last change.
type RefreshEntry struct {
	DataAsOf    time.Time `json:"data_as_of,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastChange  time.Time `json:"last_change,omitzero"`
	Reentries   int       `json:"reentries,omitzero"`
}

// RetryEntry is a thread whose last fetch failed. Listing keeps the thread
// record from the listing so the thread imports with its metadata even
// while its history cannot be fetched.
type RetryEntry struct {
	Attempts     int            `json:"attempts,omitzero"`
	NextAttempt  time.Time      `json:"next_attempt,omitzero"`
	FirstFailure time.Time      `json:"first_failure,omitzero"`
	Message      string         `json:"message,omitzero"`
	Listing      jsontext.Value `json:"listing,omitzero"`
}

// UnfinishedEntry is a thread whose newest turn may still be running.
type UnfinishedEntry struct {
	InProgress bool `json:"in_progress,omitzero"`
	Stalled    bool `json:"stalled,omitzero"`
	SubAgent   bool `json:"sub_agent,omitzero"`
}

// AccountState is one account's state, kept in account.json. Its sets hold
// only threads with outstanding work.
type AccountState struct {
	Watermarks map[ListingID]int64        `json:"watermarks,omitzero"`
	Wait       WaitState                  `json:"wait,omitzero"`
	UsagePause UsagePauseState            `json:"usage_pause,omitzero"`
	Retry      map[string]RetryEntry      `json:"retry,omitzero"`
	CheckAgain map[string]bool            `json:"check_again,omitzero"`
	Unfinished map[string]UnfinishedEntry `json:"unfinished,omitzero"`
	// Retired holds threads no longer retried after failing for 7 days.
	// The listing showing one as active does not select it again; only a
	// new updatedAt does.
	Retired      map[string]bool         `json:"retired,omitzero"`
	UsagePending map[string]PendingEntry `json:"usage_pending,omitzero"`
	Refresh      map[string]RefreshEntry `json:"refresh,omitzero"`
	AccountID    string                  `json:"account_id,omitzero"`
	UserID       string                  `json:"user_id,omitzero"`
	Backfilled   map[ListingID]bool      `json:"backfilled,omitzero"`
	// UsageAsOf is the data_as_of of the newest answered plan usage
	// request, so the status line can date stored reports after a restart.
	UsageAsOf time.Time `json:"usage_as_of,omitzero"`
}

// Store reads and writes fetcher state under the codex-cloud mirror root.
type Store struct {
	root string
	// Log records state files set aside because they could not be decoded.
	Log *log.Logger

	mu    sync.Mutex
	locks map[AccountKey]*sync.Mutex
}

// NewStore opens the state under root, {dataDir}/sources/codex-cloud.
func NewStore(root string) *Store {
	return &Store{root: root, Log: log.Default(), locks: map[AccountKey]*sync.Mutex{}}
}

// Lock serializes every state write for one account. The sync job and the
// live token monitor share it, so neither overwrites the other's change.
func (s *Store) Lock(key AccountKey) (unlock func()) {
	s.mu.Lock()
	lock, ok := s.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// accountDir is the directory holding one account's state and mirror
// files.
func (s *Store) accountDir(key AccountKey) (string, error) {
	if !plainName(string(key)) {
		return "", fmt.Errorf("codex-cloud: %q is not a usable file name", key)
	}
	return filepath.Join(s.root, string(key)), nil
}

// MirrorPath is the mirror file of one thread.
func (s *Store) MirrorPath(key AccountKey, threadID string) (string, error) {
	return s.path(key, threadID, mirrorSuffix)
}

// LoadThread reads one thread's state. A missing file is a never-synced
// thread: offset zero makes the next append recover every stored frame.
func (s *Store) LoadThread(key AccountKey, threadID string) (ThreadState, error) {
	var st ThreadState
	path, err := s.path(key, threadID, threadStateSuffix)
	if err != nil {
		return st, err
	}
	return st, load(s, path, &st)
}

// SaveThread writes one thread's state.
func (s *Store) SaveThread(key AccountKey, threadID string, st ThreadState) error {
	path, err := s.path(key, threadID, threadStateSuffix)
	if err != nil {
		return err
	}
	return save(path, st)
}

// LoadAccount reads one account's state, with every set allocated. When
// account.json is missing or damaged while thread state files remain, it
// rebuilds the plan usage queue and saves the result. The caller holds the
// account lock.
func (s *Store) LoadAccount(key AccountKey) (AccountState, error) {
	var st AccountState
	path, err := s.path(key, "account", ".json")
	if err != nil {
		return st, err
	}
	found, err := loadFound(s, path, &st)
	if err != nil {
		return st, err
	}
	st.Watermarks = allocated(st.Watermarks)
	st.Retry = allocated(st.Retry)
	st.CheckAgain = allocated(st.CheckAgain)
	st.Unfinished = allocated(st.Unfinished)
	st.Retired = allocated(st.Retired)
	st.UsagePending = allocated(st.UsagePending)
	st.Refresh = allocated(st.Refresh)
	st.Backfilled = allocated(st.Backfilled)
	if found {
		return st, nil
	}
	// A lost or damaged account.json drops the plan usage queue, and the
	// chats it named look unchanged to the next listing, so nothing would
	// queue them again. Saving the rebuilt state here makes it happen once.
	threads, err := s.rebuildUsageQueue(key, &st)
	if err != nil || !threads {
		return st, err
	}
	return st, s.SaveAccount(key, st)
}

// SaveAccount writes one account's state.
func (s *Store) SaveAccount(key AccountKey, st AccountState) error {
	path, err := s.path(key, "account", ".json")
	if err != nil {
		return err
	}
	return save(path, st)
}

// LoadDescendants reads a usage root's known descendants.
func (s *Store) LoadDescendants(key AccountKey, rootID string) ([]string, error) {
	return s.loadList(key, rootID, ".descendants.json")
}

// SaveDescendants writes a usage root's descendants; an empty list removes
// the file.
func (s *Store) SaveDescendants(key AccountKey, rootID string, ids []string) error {
	return s.saveList(key, rootID, ".descendants.json", ids)
}

// LoadWaiting reads the threads waiting for parentID to be mirrored.
func (s *Store) LoadWaiting(key AccountKey, parentID string) ([]string, error) {
	return s.loadList(key, parentID, ".waiting.json")
}

// SaveWaiting writes the threads waiting for parentID; an empty list
// removes the file.
func (s *Store) SaveWaiting(key AccountKey, parentID string, ids []string) error {
	return s.saveList(key, parentID, ".waiting.json", ids)
}

func (s *Store) loadList(key AccountKey, id, suffix string) ([]string, error) {
	path, err := s.path(key, id, suffix)
	if err != nil {
		return nil, err
	}
	var ids []string
	return ids, load(s, path, &ids)
}

func (s *Store) saveList(key AccountKey, id, suffix string, ids []string) error {
	path, err := s.path(key, id, suffix)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("codex-cloud: remove %s: %w", filepath.Base(path), err)
		}
		return nil
	}
	return save(path, ids)
}

// path joins one state or mirror file name. Thread IDs come from the
// server, so each must be a plain file name before it touches a path.
func (s *Store) path(key AccountKey, id, suffix string) (string, error) {
	for _, part := range []string{string(key), id} {
		if !plainName(part) {
			return "", fmt.Errorf("codex-cloud: %q is not a usable file name", part)
		}
	}
	return filepath.Join(s.root, string(key), id+suffix), nil
}

func plainName(name string) bool {
	return name != "" && len(name) <= 200 && !strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, `/\:`+"\x00") && filepath.IsLocal(name)
}

// load reads one state file; a missing file leaves v zero. A file that
// does not decode is moved aside to <name>.corrupt and v is left zero, so
// the thread or account counts as never synced and is fetched again, as
// for a lost state file. Without this, one damaged file would stop the
// account's sync on every pass.
func load[T any](s *Store, path string, v *T) error {
	_, err := loadFound(s, path, v)
	return err
}

// loadFound is load that also reports whether the file was there and
// decoded.
func loadFound[T any](s *Store, path string, v *T) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("codex-cloud: read %s: %w", path, err)
	}
	decodeErr := json.Unmarshal(data, v)
	if decodeErr == nil {
		return true, nil
	}
	if err := atomicfile.Replace(path, path+".corrupt"); err != nil {
		return false, fmt.Errorf("codex-cloud: decode %s: %w; set it aside: %w", path, decodeErr, err)
	}
	s.Log.Printf("codex-cloud: %s could not be decoded (%v); moved it to %s.corrupt and will fetch again",
		path, decodeErr, filepath.Base(path))
	var zero T
	*v = zero
	return false, nil
}

func save(path string, v any) error {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("codex-cloud: encode %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex-cloud: create state directory: %w", err)
	}
	if err := atomicfile.WriteFile(path, data); err != nil {
		return fmt.Errorf("codex-cloud: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func allocated[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}
