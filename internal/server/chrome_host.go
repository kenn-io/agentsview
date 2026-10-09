package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

const chromeFrameLimit = 64 << 20

type chromeConnection struct {
	net.Conn
	mu      sync.Mutex
	pending map[string]chan claudeAISyncResult
}

type chromeHost struct {
	mu         sync.Mutex
	connection *chromeConnection
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
				_ = s.chrome.connection.Close()
			}
			s.chrome.connection = current
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

func (h *chromeHost) read(conn *chromeConnection) {
	defer func() {
		_ = conn.Close()
		h.mu.Lock()
		if h.connection == conn {
			h.connection = nil
		}
		h.mu.Unlock()
		conn.mu.Lock()
		defer conn.mu.Unlock()
		for id, answer := range conn.pending {
			answer <- claudeAISyncResult{err: errors.New("Chrome host disconnected")}
			delete(conn.pending, id)
		}
	}()
	for {
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		size := binary.NativeEndian.Uint32(header[:])
		if size > chromeFrameLimit {
			return
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		var reply struct {
			ID         string `json:"id"`
			Status     int    `json:"status"`
			Body       string `json:"body"`
			RetryAfter string `json:"retryAfter"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(body, &reply); err != nil {
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
		return importer.ClaudeAIResponse{}, errors.New("Chrome host not connected")
	}
	id := rand.Text()
	answer := make(chan claudeAISyncResult, 1)
	body, err := json.Marshal(map[string]string{"id": id, "path": path})
	if err != nil {
		return importer.ClaudeAIResponse{}, err
	}
	var header [4]byte
	binary.NativeEndian.PutUint32(header[:], uint32(len(body)))
	conn.mu.Lock()
	conn.pending[id] = answer
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	_, err = conn.Write(append(header[:], body...))
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
