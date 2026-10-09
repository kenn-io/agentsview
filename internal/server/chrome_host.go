package server

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

type chromeConnection struct {
	net.Conn
	mu      sync.Mutex
	pending map[string]chan claudeAISyncResult
}

type chromeHost struct {
	mu             sync.Mutex
	connection     *chromeConnection
	refusedProfile bool
}

// ServeChromeHost binds the private endpoint before starting its accept loop.
func (s *Server) ServeChromeHost(ctx context.Context, socketPath string) error {
	if _, local := s.db.(*db.DB); !local || s.db.ReadOnly() {
		return nil
	}
	if err := safefileio.EnsurePrivateDir(filepath.Dir(socketPath)); err != nil {
		return err
	}
	listener, err := daemon.Listen(ctx, daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		s.chrome.mu.Lock()
		if s.chrome.connection != nil {
			_ = s.chrome.connection.Close()
		}
		s.chrome.mu.Unlock()
	}()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			current := &chromeConnection{Conn: conn, pending: make(map[string]chan claudeAISyncResult)}
			s.chrome.mu.Lock()
			if ctx.Err() != nil {
				s.chrome.mu.Unlock()
				_ = conn.Close()
				return
			}
			if s.chrome.connection != nil {
				s.chrome.refusedProfile = true
				s.chrome.mu.Unlock()
				_ = conn.Close()
				continue
			}
			s.chrome.connection = current
			s.chrome.refusedProfile = false
			s.chrome.mu.Unlock()
			go s.chrome.read(current)
		}
	}()
	return nil
}

func (h *chromeHost) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connection != nil
}

func (h *chromeHost) signInError() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refusedProfile {
		return "Another Chrome profile also has the extension. Only the first connected profile is used. Sign in to Claude.ai there or disable the extension in the other profile, then Sync again"
	}
	return "Sign in to Claude.ai, then Sync again"
}

func (h *chromeHost) read(conn *chromeConnection) {
	disconnectErr := errors.New("chrome host disconnected")
	defer func() {
		_ = conn.Close()
		h.mu.Lock()
		if h.connection == conn {
			h.connection = nil
			h.refusedProfile = false
		}
		h.mu.Unlock()
		conn.mu.Lock()
		defer conn.mu.Unlock()
		for id, answer := range conn.pending {
			answer <- claudeAISyncResult{err: disconnectErr}
			delete(conn.pending, id)
		}
	}()
	for {
		body, err := chromehost.ReadFrame(conn)
		if err != nil {
			return
		}
		var reply struct {
			Version    int    `json:"version"`
			ID         string `json:"id"`
			Status     int    `json:"status"`
			Body       string `json:"body"`
			RetryAfter string `json:"retryAfter"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
			return
		}
		if reply.Version != chromehost.Version {
			disconnectErr = chromehost.VersionError{Version: reply.Version}
			return
		}
		if reply.Status < 0 || reply.Status > 599 {
			disconnectErr = errors.New("invalid Chrome reply status")
			return
		}
		response := claudeAISyncResult{status: reply.Status, body: []byte(reply.Body), retryAfter: reply.RetryAfter}
		if reply.Error != "" {
			response.err = errors.New(reply.Error)
		}
		conn.mu.Lock()
		if answer := conn.pending[reply.ID]; answer != nil {
			delete(conn.pending, reply.ID)
			answer <- response
		}
		conn.mu.Unlock()
	}
}

func (h *chromeHost) fetch(ctx context.Context, path string) (importer.ClaudeAIResponse, error) {
	h.mu.Lock()
	conn := h.connection
	h.mu.Unlock()
	if conn == nil {
		return importer.ClaudeAIResponse{}, errors.New("chrome host not connected")
	}
	id := rand.Text()
	answer := make(chan claudeAISyncResult, 1)
	body, err := json.Marshal(map[string]any{"version": chromehost.Version, "id": id, "path": path})
	if err != nil {
		return importer.ClaudeAIResponse{}, err
	}
	conn.mu.Lock()
	conn.pending[id] = answer
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	err = chromehost.WriteFrame(conn, body)
	conn.mu.Unlock()
	defer func() { conn.mu.Lock(); delete(conn.pending, id); conn.mu.Unlock() }()
	if err != nil {
		_ = conn.Close()
		return importer.ClaudeAIResponse{}, err
	}
	return awaitClaudeAIResponse(ctx, answer)
}

func awaitClaudeAIResponse(ctx context.Context, answer <-chan claudeAISyncResult) (importer.ClaudeAIResponse, error) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return importer.ClaudeAIResponse{}, ctx.Err()
	case <-timer.C:
		return importer.ClaudeAIResponse{}, errors.New("claude browser fetch timed out")
	case response := <-answer:
		return importer.ClaudeAIResponse{Status: response.status, Body: response.body, RetryAfter: response.retryAfter}, response.err
	}
}
