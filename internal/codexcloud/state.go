package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
)

// mirrorSuffix ends every mirror file; the provider discovers only these.
const mirrorSuffix = ".jsonl.zstd"

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
	LastPlanRecordDigest string    `json:"last_plan_record_digest,omitzero"`
	LastPlanRecordKind   string    `json:"last_plan_record_kind,omitzero"`
	CountedUnder         string    `json:"counted_under,omitzero"`
	LastHandledTurnID    string    `json:"last_handled_turn_id,omitzero"`
	IsSubAgent           bool      `json:"is_sub_agent,omitzero"`
	ArchivedFirstSeen    time.Time `json:"archived_first_seen,omitzero"`
}

// WaitState is a per-account or per-home wait after a failure.
type WaitState struct {
	Failure  string    `json:"failure,omitzero"`
	Attempts int       `json:"attempts,omitzero"`
	Until    time.Time `json:"until,omitzero"`
}

// UsagePauseState pauses the plan usage step for an account.
type UsagePauseState struct {
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitzero"`
}

// PendingEntry is a usage root waiting for its first usable row.
type PendingEntry struct {
	Since       time.Time `json:"since,omitzero"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	Attempts    int       `json:"attempts,omitzero"`
}

// RefreshEntry is a usage root with a usable row that is still refreshed.
type RefreshEntry struct {
	DataAsOf    time.Time `json:"data_as_of,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	Settled     bool      `json:"settled,omitzero"`
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
	Watermarks   map[ListingID]int64        `json:"watermarks,omitzero"`
	Wait         WaitState                  `json:"wait,omitzero"`
	UsagePause   UsagePauseState            `json:"usage_pause,omitzero"`
	Retry        map[string]RetryEntry      `json:"retry,omitzero"`
	CheckAgain   map[string]bool            `json:"check_again,omitzero"`
	Unfinished   map[string]UnfinishedEntry `json:"unfinished,omitzero"`
	UsagePending map[string]PendingEntry    `json:"usage_pending,omitzero"`
	Refresh      map[string]RefreshEntry    `json:"refresh,omitzero"`
	AccountID    string                     `json:"account_id,omitzero"`
	UserID       string                     `json:"user_id,omitzero"`
	Backfilled   map[ListingID]bool         `json:"backfilled,omitzero"`
}

// Store reads and writes fetcher state under the codex-cloud mirror root.
type Store struct {
	root string

	mu    sync.Mutex
	locks map[AccountKey]*sync.Mutex
}

// NewStore opens the state under root, {dataDir}/sources/codex-cloud.
func NewStore(root string) *Store {
	return &Store{root: root, locks: map[AccountKey]*sync.Mutex{}}
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

// MirrorPath is the mirror file of one thread.
func (s *Store) MirrorPath(key AccountKey, threadID string) (string, error) {
	return s.path(key, threadID, mirrorSuffix)
}

// LoadThread reads one thread's state. A missing file is a never-synced
// thread: offset zero makes the next append recover every stored frame.
func (s *Store) LoadThread(key AccountKey, threadID string) (ThreadState, error) {
	var st ThreadState
	path, err := s.path(key, threadID, ".state.json")
	if err != nil {
		return st, err
	}
	return st, load(path, &st)
}

// SaveThread writes one thread's state.
func (s *Store) SaveThread(key AccountKey, threadID string, st ThreadState) error {
	path, err := s.path(key, threadID, ".state.json")
	if err != nil {
		return err
	}
	return save(path, st)
}

// LoadAccount reads one account's state, with every set allocated.
func (s *Store) LoadAccount(key AccountKey) (AccountState, error) {
	var st AccountState
	path, err := s.path(key, "account", ".json")
	if err != nil {
		return st, err
	}
	if err := load(path, &st); err != nil {
		return st, err
	}
	st.Watermarks = allocated(st.Watermarks)
	st.Retry = allocated(st.Retry)
	st.CheckAgain = allocated(st.CheckAgain)
	st.Unfinished = allocated(st.Unfinished)
	st.UsagePending = allocated(st.UsagePending)
	st.Refresh = allocated(st.Refresh)
	st.Backfilled = allocated(st.Backfilled)
	return st, nil
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
	return ids, load(path, &ids)
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

func load(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("codex-cloud: read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("codex-cloud: decode %s: %w", filepath.Base(path), err)
	}
	return nil
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
