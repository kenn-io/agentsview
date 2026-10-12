package codexcloudtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Item is one scripted thread item. Zero times are absent.
type Item struct {
	ID            string
	Type          string
	Status        string
	Text          string
	StartedAtMs   int64
	CompletedAtMs int64
}

// Turn is one scripted turn, its items oldest first. Zero times are
// absent; times are Unix seconds.
type Turn struct {
	ID          string
	Status      string
	StartedAt   int64
	CompletedAt int64
	Items       []Item
}

// Thread is one scripted thread, its turns oldest first. The listing
// fields drive thread/list.
type Thread struct {
	ID        string
	Name      string
	CreatedAt int64
	UpdatedAt int64
	// Status is the listing status type; empty means "idle".
	Status   string
	Archived bool
	SubAgent bool
	Turns    []Turn
}

// Threads answers thread/turns/list and thread/items/list from scripted
// threads. Turn listings come newest first; an item anchor includes the
// anchor item, so callers must discard it themselves.
type Threads struct {
	mu           sync.Mutex
	threads      map[string]Thread
	rejectAnchor bool
}

// NewThreads returns an empty script.
func NewThreads() *Threads { return &Threads{threads: map[string]Thread{}} }

// Set adds or replaces a thread.
func (s *Threads) Set(thread Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[thread.ID] = cloneThread(thread)
}

// Update changes a thread in place.
func (s *Threads) Update(id string, change func(*Thread)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.threads[id]
	change(&thread)
	s.threads[id] = cloneThread(thread)
}

// RejectAnchor makes later anchored item reads fail with -32602.
func (s *Threads) RejectAnchor() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectAnchor = true
}

// ServeThreads answers thread/list and the thread history methods from s.
func (h *Host) ServeThreads(s *Threads) {
	h.Handle("thread/list", s.ThreadList)
	h.Handle("thread/turns/list", s.TurnsList)
	h.Handle("thread/items/list", s.ItemsList)
}

// ThreadList answers thread/list newest updatedAt first. A request with
// sourceKinds lists sub-agent threads; one without lists the others.
func (s *Threads) ThreadList(params jsontext.Value) (any, *Error) {
	var p struct {
		Cursor      jsontext.Value `json:"cursor"`
		Limit       int            `json:"limit"`
		Archived    bool           `json:"archived"`
		SourceKinds []string       `json:"sourceKinds"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &Error{Code: -32602, Message: err.Error()}
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	s.mu.Lock()
	var rows []Thread
	for _, thread := range s.threads {
		if thread.Archived == p.Archived && thread.SubAgent == (len(p.SourceKinds) > 0) {
			rows = append(rows, thread)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(rows, func(a, b Thread) int {
		if a.UpdatedAt != b.UpdatedAt {
			return int(b.UpdatedAt - a.UpdatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	start := offsetCursor(p.Cursor)
	end := min(start+p.Limit, len(rows))
	data := []map[string]any{}
	for _, thread := range rows[start:end] {
		status := thread.Status
		if status == "" {
			status = "idle"
		}
		data = append(data, map[string]any{
			"id": thread.ID, "name": thread.Name, "createdAt": thread.CreatedAt, "updatedAt": thread.UpdatedAt,
			"status": map[string]string{"type": status}, "originator": "codex_cloud",
			"environments": []any{},
		})
	}
	return page(data, end, len(rows)), nil
}

type pageParams struct {
	ThreadID  string         `json:"threadId"`
	TurnID    string         `json:"turnId"`
	Cursor    jsontext.Value `json:"cursor"`
	Limit     int            `json:"limit"`
	ItemsView string         `json:"itemsView"`
}

func (s *Threads) lookup(params jsontext.Value) (pageParams, Thread, *Error) {
	var p pageParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, Thread{}, &Error{Code: -32602, Message: err.Error()}
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.threads[p.ThreadID]
	if !ok {
		return p, Thread{}, &Error{Code: -32600, Message: "no rollout found for thread id " + p.ThreadID}
	}
	return p, cloneThread(thread), nil
}

// TurnsList answers thread/turns/list; tests wrap it to act between calls.
func (s *Threads) TurnsList(params jsontext.Value) (any, *Error) {
	p, thread, rpcErr := s.lookup(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	start := offsetCursor(p.Cursor)
	turns := slices.Clone(thread.Turns)
	slices.Reverse(turns)
	end := min(start+p.Limit, len(turns))
	data := []map[string]any{}
	for _, turn := range turns[start:end] {
		obj := map[string]any{
			"id": turn.ID, "rootTurnId": turn.ID, "status": turn.Status,
			"startedAt": optional(turn.StartedAt), "completedAt": optional(turn.CompletedAt),
			"durationMs": nil, "error": nil, "itemsView": p.ItemsView, "items": []any{},
		}
		if p.ItemsView == "full" || p.ItemsView == "" {
			items := []any{}
			for _, item := range turn.Items {
				items = append(items, itemObject(item))
			}
			obj["items"], obj["itemsView"] = items, "full"
		}
		data = append(data, obj)
	}
	return page(data, end, len(turns)), nil
}

// ItemsList answers thread/items/list.
func (s *Threads) ItemsList(params jsontext.Value) (any, *Error) {
	p, thread, rpcErr := s.lookup(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var rows []map[string]any
	for _, turn := range thread.Turns {
		if p.TurnID != "" && turn.ID != p.TurnID {
			continue
		}
		for _, item := range turn.Items {
			rows = append(rows, map[string]any{
				"turnId": turn.ID, "item": itemObject(item),
				"startedAtMs": optional(item.StartedAtMs), "completedAtMs": optional(item.CompletedAtMs),
			})
		}
	}
	var start int
	var anchor struct {
		Type   string `json:"type"`
		ItemID string `json:"itemId"`
	}
	if p.Cursor.Kind() == '{' && json.Unmarshal(p.Cursor, &anchor) == nil && anchor.Type == "item" {
		s.mu.Lock()
		reject := s.rejectAnchor
		s.mu.Unlock()
		if reject || p.TurnID == "" {
			return nil, &Error{Code: -32602, Message: "invalid item anchor"}
		}
		start = -1
		for i, row := range rows {
			if row["item"].(map[string]any)["id"] == anchor.ItemID {
				start = i
			}
		}
		if start < 0 {
			return nil, &Error{Code: -32602, Message: "unknown anchor item " + anchor.ItemID}
		}
	} else {
		start = offsetCursor(p.Cursor)
	}
	end := min(start+p.Limit, len(rows))
	return page(rows[start:end], end, len(rows)), nil
}

func page[T any](data []T, end, total int) map[string]any {
	var next any
	if end < total {
		next = "offset-" + strconv.Itoa(end)
	}
	if data == nil {
		data = []T{}
	}
	return map[string]any{"data": data, "nextCursor": next}
}

func offsetCursor(cursor jsontext.Value) int {
	var s string
	if json.Unmarshal(cursor, &s) != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "offset-"))
	if err != nil {
		return 0
	}
	return n
}

func itemObject(item Item) map[string]any {
	obj := map[string]any{"type": item.Type, "id": item.ID}
	if item.Status != "" {
		obj["status"] = item.Status
	}
	if item.Text != "" {
		obj["text"] = item.Text
	}
	return obj
}

func optional(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func cloneThread(thread Thread) Thread {
	thread.Turns = slices.Clone(thread.Turns)
	for i := range thread.Turns {
		thread.Turns[i].Items = slices.Clone(thread.Turns[i].Items)
	}
	return thread
}
