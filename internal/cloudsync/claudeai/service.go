package claudeai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/cloudsync/transport"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

const (
	Provider                   = "claude-ai"
	OperationListConversations = "list_conversations"
	OperationGetConversation   = "get_conversation"
)

type ListParams struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}
type DetailParams struct {
	ConversationID string `json:"conversation_id"`
}

type SyncMode string

const (
	SyncIncremental SyncMode = "incremental"
	SyncRepair      SyncMode = "repair"
)

type JobStatus struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Mode     SyncMode `json:"mode"`
	Scanned  int      `json:"scanned"`
	Changed  int      `json:"changed"`
	Fetched  int      `json:"fetched"`
	Imported int      `json:"imported"`
	Updated  int      `json:"updated"`
	Skipped  int      `json:"skipped"`
	Failed   int      `json:"failed"`
	Error    string   `json:"error,omitempty"`
}

type ScheduleConfig struct {
	Enabled         bool      `json:"enabled"`
	IntervalMinutes int       `json:"interval_minutes"`
	LastStartedAt   time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt time.Time `json:"last_completed_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

type Service struct {
	broker             *transport.Broker
	store              db.Store
	cacheRoot, machine string
	mu                 sync.Mutex
	status             JobStatus
	cancel             context.CancelFunc
	schedule           ScheduleConfig
	archiveWrite       func(func() error) error
	stopSchedule       chan struct{}
	closed             bool
	schedulerWG        sync.WaitGroup
	jobsWG             sync.WaitGroup
}

func NewService(broker *transport.Broker, store db.Store, cacheRoot, machine string, archiveWrite ...func(func() error) error) *Service {
	s := &Service{broker: broker, store: store, cacheRoot: cacheRoot, machine: machine, status: JobStatus{Status: "idle"}, schedule: ScheduleConfig{IntervalMinutes: 360}, stopSchedule: make(chan struct{})}
	if len(archiveWrite) > 0 {
		s.archiveWrite = archiveWrite[0]
	}
	if raw, err := os.ReadFile(filepath.Join(cacheRoot, "schedule.json")); err == nil {
		_ = json.Unmarshal(raw, &s.schedule)
	}
	if s.schedule.IntervalMinutes < 15 {
		s.schedule.IntervalMinutes = 360
	}
	s.schedulerWG.Add(1)
	go func() {
		defer s.schedulerWG.Done()
		s.runSchedule()
	}()
	return s
}

func (s *Service) Schedule() ScheduleConfig { s.mu.Lock(); defer s.mu.Unlock(); return s.schedule }
func (s *Service) ConfigureSchedule(config ScheduleConfig) error {
	if config.IntervalMinutes < 15 || config.IntervalMinutes > 24*60 {
		return errors.New("Claude schedule interval must be between 15 minutes and 24 hours")
	}
	s.mu.Lock()
	s.schedule.Enabled = config.Enabled
	s.schedule.IntervalMinutes = config.IntervalMinutes
	config = s.schedule
	s.mu.Unlock()
	return atomicWriteJSON(filepath.Join(s.cacheRoot, "schedule.json"), config)
}
func (s *Service) runSchedule() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopSchedule:
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		cfg := s.schedule
		running := s.status.Status == "running" || s.status.Status == "cancelling"
		due := cfg.LastStartedAt.IsZero() || time.Since(cfg.LastStartedAt) >= time.Duration(cfg.IntervalMinutes)*time.Minute
		s.mu.Unlock()
		if !cfg.Enabled || running || !due {
			continue
		}
		if _, err := s.Start(context.Background(), SyncIncremental); err == nil {
			s.mu.Lock()
			s.schedule.LastStartedAt = time.Now().UTC()
			s.mu.Unlock()
			_ = atomicWriteJSON(filepath.Join(s.cacheRoot, "schedule.json"), s.Schedule())
		}
	}
}

// Close stops the scheduler and joins all Claude work before archive shutdown.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stopSchedule)
	if s.cancel != nil && s.status.Status == "running" {
		s.status.Status = "cancelling"
		s.cancel()
	}
	s.mu.Unlock()

	s.schedulerWG.Wait()
	s.jobsWG.Wait()
}
func (s *Service) Status() JobStatus { s.mu.Lock(); defer s.mu.Unlock(); return s.status }

var ErrSyncAlreadyRunning = errors.New("Claude sync already running")

func (s *Service) Start(ctx context.Context, mode SyncMode) (JobStatus, error) {
	if s.store != nil && s.store.ReadOnly() {
		return JobStatus{}, errors.New("cloud import not available in read-only mode")
	}
	if mode != SyncIncremental && mode != SyncRepair {
		return JobStatus{}, fmt.Errorf("unsupported Claude sync mode %q", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return JobStatus{}, errors.New("Claude sync service is closed")
	}
	if s.status.Status == "running" || s.status.Status == "cancelling" {
		return s.status, ErrSyncAlreadyRunning
	}
	// HTTP request contexts end as soon as the start endpoint returns. The job
	// owns its cancellation lifecycle instead of inheriting that short-lived
	// context; callers still receive immediate start acknowledgement.
	_ = ctx
	jobCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.status = JobStatus{ID: fmt.Sprintf("claude-%d", time.Now().UnixNano()), Status: "running", Mode: mode}
	s.jobsWG.Add(1)
	go func() {
		defer s.jobsWG.Done()
		s.run(jobCtx)
	}()
	return s.status, nil
}
func (s *Service) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil || s.status.Status != "running" {
		return false
	}
	s.status.Status = "cancelling"
	s.cancel()
	return true
}
func (s *Service) update(fn func(*JobStatus)) { s.mu.Lock(); defer s.mu.Unlock(); fn(&s.status) }
func (s *Service) fail(err error) {
	if !errors.Is(err, context.Canceled) {
		_, _ = FailBrowserImport(s.cacheRoot)
		s.persistScheduleResult(err.Error())
	}
	s.update(func(st *JobStatus) {
		if errors.Is(err, context.Canceled) {
			st.Status = "cancelled"
		} else {
			st.Status = "failed"
			st.Error = err.Error()
			st.Failed++
		}
		s.cancel = nil
	})
}
func (s *Service) finish() {
	if _, err := CompleteBrowserImport(s.cacheRoot); err != nil {
		s.fail(err)
		return
	}
	s.update(func(st *JobStatus) { st.Status = "completed"; s.cancel = nil })
	s.persistScheduleResult("")
}

func (s *Service) persistScheduleResult(lastError string) {
	s.mu.Lock()
	s.schedule.LastCompletedAt = time.Now().UTC()
	s.schedule.LastError = lastError
	config := s.schedule
	s.mu.Unlock()
	_ = atomicWriteJSON(filepath.Join(s.cacheRoot, "schedule.json"), config)
}

func (s *Service) run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.fail(fmt.Errorf("Claude sync panic: %v", r))
		}
	}()
	seen := map[string]struct{}{}
	for offset := 0; ; {
		response, err := s.request(ctx, OperationListConversations, ListParams{Offset: offset, Limit: 50})
		if err != nil {
			s.fail(err)
			return
		}
		if response.Status == 401 || response.Status == 403 {
			s.fail(fmt.Errorf("Claude authentication expired (HTTP %d)", response.Status))
			return
		}
		if response.Status < 200 || response.Status >= 300 {
			s.fail(fmt.Errorf("Claude list returned HTTP %d", response.Status))
			return
		}
		items, hasMore, err := decodeBrowserPage(response.Body)
		if err != nil {
			s.fail(err)
			return
		}
		if len(items) == 0 {
			s.finish()
			return
		}
		fresh := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			id, _, e := conversationMarker(item)
			if e != nil {
				s.update(func(st *JobStatus) { st.Failed++ })
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			fresh = append(fresh, item)
		}
		s.update(func(st *JobStatus) { st.Scanned += len(fresh) })
		plan, err := PlanBrowserImportWithForce(s.cacheRoot, fresh, s.Status().Mode == SyncRepair)
		if err != nil {
			s.fail(err)
			return
		}
		s.update(func(st *JobStatus) { st.Changed += len(plan.ChangedIDs); st.Skipped += plan.Unchanged })
		wanted := make(map[string]struct{}, len(plan.ChangedIDs))
		for _, id := range plan.ChangedIDs {
			wanted[id] = struct{}{}
		}
		batch := make([]BrowserConversation, 0, len(wanted))
		for _, summary := range fresh {
			id, _, _ := conversationMarker(summary)
			if _, ok := wanted[id]; !ok {
				continue
			}
			detail, err := s.request(ctx, OperationGetConversation, DetailParams{ConversationID: id})
			if err != nil {
				s.fail(err)
				return
			}
			if detail.Status == 404 {
				s.update(func(st *JobStatus) { st.Skipped++ })
				continue
			}
			if detail.Status < 200 || detail.Status >= 300 {
				s.fail(fmt.Errorf("Claude conversation %s returned HTTP %d", id, detail.Status))
				return
			}
			batch = append(batch, BrowserConversation{Summary: summary, Conversation: detail.Body})
			s.update(func(st *JobStatus) { st.Fetched++ })
		}
		if len(batch) > 0 {
			if err := s.importBatch(ctx, batch, s.Status().Mode == SyncRepair); err != nil {
				s.fail(err)
				return
			}
		}
		offset += len(items)
		if !hasMore {
			s.finish()
			return
		}
	}
}
func (s *Service) request(ctx context.Context, operation string, params any) (transport.Response, error) {
	for attempt := 0; attempt < 5; attempt++ {
		response, err := s.broker.Do(ctx, Provider, operation, params)
		if err != nil {
			return transport.Response{}, err
		}
		if response.Error != "" || response.Status == 429 || response.Status >= 500 {
			if attempt == 4 {
				if response.Error != "" {
					return response, errors.New(response.Error)
				}
				return response, fmt.Errorf("Claude %s returned HTTP %d", operation, response.Status)
			}
			delay := time.Duration(1<<attempt) * time.Second
			if response.RetryAfter != "" {
				if d, e := time.ParseDuration(response.RetryAfter + "s"); e == nil {
					delay = d
				}
			}
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return transport.Response{}, ctx.Err()
			}
		}
		return response, nil
	}
	panic("unreachable")
}
func (s *Service) importBatch(ctx context.Context, conversations []BrowserConversation, repair bool) error {
	// Keep FTS suspension inside this short archive-write critical section. Remote
	// pagination and detail requests continue while search remains available.
	return s.withArchiveWrite(func() (workErr error) {
		fts := importer.NewLazyFTS(s.store, nil)
		defer func() {
			if err := fts.Restore(); err != nil {
				workErr = errors.Join(workErr, err)
			}
		}()
		prepared, err := PrepareBrowserImportWithForce(s.cacheRoot, conversations, repair)
		if err != nil {
			return err
		}
		if repair {
			ids := make([]string, 0, len(conversations))
			for _, c := range conversations {
				id, _, e := conversationMarker(c.Summary)
				if e != nil {
					return e
				}
				ids = append(ids, "claude-ai:"+id)
			}
			if _, err := s.store.RestoreExcludedSessions(ids); err != nil {
				return err
			}
		}
		stats, err := importer.ImportClaudeAIWithFTS(ctx, s.store, bytes.NewReader(prepared.ExportJSON), nil, fts, s.machine)
		if err != nil {
			return err
		}
		if stats.Errors > 0 {
			return fmt.Errorf("Claude import failed for %d conversations", stats.Errors)
		}
		if err := prepared.Commit(); err != nil {
			return err
		}
		if _, err := RecordBrowserImportProgress(s.cacheRoot, prepared.Downloaded, stats.Imported+stats.Updated, 0, false); err != nil {
			return err
		}
		s.update(func(st *JobStatus) {
			st.Imported += stats.Imported
			st.Updated += stats.Updated
			st.Skipped += stats.Skipped
		})
		return nil
	})
}

func (s *Service) withArchiveWrite(work func() error) error {
	if s.archiveWrite != nil {
		return s.archiveWrite(work)
	}
	return work()
}
func decodeBrowserPage(raw json.RawMessage) ([]json.RawMessage, bool, error) {
	var body struct {
		Conversations []json.RawMessage `json:"conversations"`
		Items         []json.RawMessage `json:"items"`
		Data          []json.RawMessage `json:"data"`
		Results       []json.RawMessage `json:"results"`
		HasMore       *bool             `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false, err
	}
	items := body.Conversations
	if items == nil {
		items = body.Items
	}
	if items == nil {
		items = body.Data
	}
	if items == nil {
		items = body.Results
	}
	if items == nil {
		return nil, false, errors.New("Claude list had no conversations")
	}
	return items, body.HasMore == nil || *body.HasMore, nil
}
