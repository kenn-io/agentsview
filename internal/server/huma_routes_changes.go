package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	gosync "sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sessionwatch"
	"go.kenn.io/agentsview/internal/sync"
)

const changeStreamSchemaVersion = 1

// changeStreamSyncInterval is the minimum interval between watcher-triggered
// syncs while a change-stream subscriber is connected. A shorter configured
// sync_interval still applies.
const changeStreamSyncInterval = time.Second

// changeStreamSyncHold sets the sync interval override while at least one
// change-stream subscriber is connected and clears it after the last one
// leaves.
type changeStreamSyncHold struct {
	override    *sync.IntervalOverride
	mu          gosync.Mutex
	subscribers int
}

func (h *changeStreamSyncHold) acquire() (release func()) {
	if h.override == nil {
		return func() {}
	}
	h.mu.Lock()
	h.subscribers++
	if h.subscribers == 1 {
		h.override.Set(changeStreamSyncInterval)
	}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.subscribers--
		if h.subscribers == 0 {
			h.override.Set(0)
		}
	}
}

func (s *Server) registerChangeStreamRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Changes")

	s.stream(group, http.MethodGet, "/changes/stream",
		"Follow archive changes", s.humaChangeStream, changeStreamDescription)
}

func changeStreamDescription(op *huma.Operation) {
	op.Description = "Streams content-free session and message changes. ready names the archive, its newest cursor, " +
		"and this installation's machine identity; each session and message carries the identity of the machine it originated on. " +
		"changes carries every change from `from` to `to`; each event's `from` equals the previous event's `to`, " +
		"and reset is true when the stream restarted from the beginning of the archive. heartbeat keeps the connection open. " +
		"error reports a failed read before the stream closes; reconnect with the last `to` received."
}

type changeStreamInput struct {
	After string `query:"after" doc:"Cursor from the last changes event's to field. Empty, foreign, or invalid cursors restart from the beginning of the archive."`
}

type changeStreamReadyEvent struct {
	SchemaVersion  int     `json:"schema_version"`
	ArchiveID      string  `json:"archive_id"`
	DatabaseID     string  `json:"database_id"`
	Head           string  `json:"head"`
	LocalMachineID *string `json:"local_machine_id"`
}

type changeStreamChangesEvent struct {
	SchemaVersion int                      `json:"schema_version"`
	ArchiveID     string                   `json:"archive_id"`
	DatabaseID    string                   `json:"database_id"`
	From          string                   `json:"from"`
	To            string                   `json:"to"`
	Reset         bool                     `json:"reset"`
	Sessions      []db.ChangeStreamSession `json:"sessions"`
	Messages      []db.ChangeStreamMessage `json:"messages"`
}

// humaChangeStream replays changes after the caller's cursor, then follows
// new ones. It subscribes before its first read, and every read asks for all
// changes after the last one sent, so a write committed at any point between
// reads is delivered by the next one. Reads never write the archive.
func (s *Server) humaChangeStream(
	ctx context.Context, in *changeStreamInput,
) (*huma.StreamResponse, error) {
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	if s.broadcaster == nil {
		return nil, huma.ErrorWithHeaders(
			apiError(http.StatusServiceUnavailable, "events not available in this mode"),
			http.Header{"Retry-After": []string{"300"}},
		)
	}
	if _, err := local.ReadChangeStreamHead(ctx); err != nil {
		return nil, internalError("read change stream head", err)
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		stream, ok := newHumaSSEStream(hctx)
		if !ok {
			writeHumaJSON(hctx, http.StatusInternalServerError,
				apiResponseError{Message: "streaming not supported"})
			return
		}
		streamCtx := hctx.Context()
		sub, unsub := s.broadcaster.SubscribeWakes()
		defer unsub()
		releaseSync := s.changeStreamSync.acquire()
		defer releaseSync()
		head, err := local.ReadChangeStreamHead(streamCtx)
		if err != nil {
			sendChangeStreamError(stream, err)
			return
		}
		ready := changeStreamReadyEvent{
			SchemaVersion: changeStreamSchemaVersion,
			ArchiveID:     head.ArchiveID,
			DatabaseID:    head.DatabaseID,
			Head:          head.Head,
		}
		if head.LocalMachineID != "" {
			ready.LocalMachineID = &head.LocalMachineID
		}
		if !stream.SendJSON("ready", ready) {
			return
		}
		heartbeat := time.NewTicker(sessionwatch.PollInterval * sessionwatch.HeartbeatTicks)
		defer heartbeat.Stop()
		if !s.initializeChangeStreamMessages(streamCtx, local, stream, heartbeat.C) {
			return
		}
		follower := changeStreamFollower{db: local, stream: stream, sent: in.After, cursor: in.After}
		if !follower.catchUp(streamCtx) {
			return
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					return
				}
				if !follower.catchUp(streamCtx) {
					return
				}
			case <-heartbeat.C:
				if !stream.Send("heartbeat", time.Now().Format(time.RFC3339)) {
					return
				}
				// Not every archive write announces itself; the heartbeat
				// bounds how long such a change waits.
				if !follower.catchUp(streamCtx) {
					return
				}
			}
		}
	}}, nil
}

// initializeChangeStreamMessages builds the conversation index when the
// archive has never had one, so a subscriber receives message changes without
// a separate initialization step. The daemon owns the writer, so it runs the
// same one-time build as the conversation export. Heartbeats continue while it
// runs. It returns false when the stream must end.
func (s *Server) initializeChangeStreamMessages(
	ctx context.Context, local *db.DB, stream *SSEStream, heartbeat <-chan time.Time,
) bool {
	if local.ReadOnly() {
		// A read-only daemon cannot build the index; it streams sessions only.
		return true
	}
	active, err := local.ConversationExportActive(ctx)
	if err != nil {
		sendChangeStreamError(stream, err)
		return false
	}
	if active {
		return true
	}
	// The build serves every subscriber and later exports, so a subscriber
	// that disconnects must not cancel it; the server's lifetime bounds it.
	buildCtx := context.Background()
	if s.baseCtx != nil {
		buildCtx = s.baseCtx
	}
	done := make(chan error, 1)
	go func() {
		_, err := local.EnsureConversationExportInitialized(buildCtx)
		done <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return false
		case err := <-done:
			if err != nil {
				sendChangeStreamError(stream, err)
				return false
			}
			return true
		case <-heartbeat:
			if !stream.Send("heartbeat", time.Now().Format(time.RFC3339)) {
				return false
			}
		}
	}
}

type changeStreamFollower struct {
	db     *db.DB
	stream *SSEStream
	// sent is the to cursor of the last event sent; cursor is the position
	// read through, which moves past ranges that held no changes.
	sent   string
	cursor string
	usage  db.ChangeStreamUsageCache
}

// catchUp sends every pending change, one bounded event per page. It returns
// false when the stream must end.
func (f *changeStreamFollower) catchUp(ctx context.Context) bool {
	for {
		batch, err := f.db.ReadChangeStream(ctx, f.cursor, &f.usage)
		if err != nil {
			if ctx.Err() == nil {
				sendChangeStreamError(f.stream, err)
			}
			return false
		}
		f.cursor = batch.To
		if batch.Reset || len(batch.Sessions) > 0 || len(batch.Messages) > 0 {
			from := f.sent
			if batch.Reset {
				from = ""
			}
			if !f.stream.SendJSON("changes", changeStreamChangesEvent{
				SchemaVersion: changeStreamSchemaVersion,
				ArchiveID:     batch.ArchiveID,
				DatabaseID:    batch.DatabaseID,
				From:          from,
				To:            batch.To,
				Reset:         batch.Reset,
				Sessions:      batch.Sessions,
				Messages:      batch.Messages,
			}) {
				return false
			}
			f.sent = batch.To
		}
		if !batch.More {
			return true
		}
	}
}

func sendChangeStreamError(stream *SSEStream, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	log.Printf("change stream: %v", err)
	stream.SendJSON("error", map[string]string{"error": "failed to read archive changes"})
}
