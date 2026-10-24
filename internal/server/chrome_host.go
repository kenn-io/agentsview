package server

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
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
	syncMu     sync.Mutex
	mu         sync.Mutex
	connection *chromeConnection
	// stop closes the endpoint and removes its files; nil until served.
	stop   func()
	closed bool
}

// ServeChromeHost binds the private endpoint before starting its accept loop.
// The endpoint closes when ctx ends or the server shuts down. Closing removes
// the socket, its lock file, and the socket directory if this call created it.
func (s *Server) ServeChromeHost(ctx context.Context, socketPath string) error {
	if _, local := s.db.(*db.DB); !local || s.db.ReadOnly() {
		return nil
	}
	dir := filepath.Dir(socketPath)
	_, err := os.Lstat(dir)
	createdDir := errors.Is(err, fs.ErrNotExist)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return err
	}
	listener, err := daemon.Listen(ctx, daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath})
	if err != nil {
		return err
	}
	stop := sync.OnceFunc(func() {
		s.chrome.mu.Lock()
		s.chrome.closed = true
		if s.chrome.connection != nil {
			_ = s.chrome.connection.Close()
		}
		s.chrome.mu.Unlock()
		// Closing a listener that net.Listen created unlinks its socket.
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("Chrome host close: %v", err)
		}
		removeChromeHostFiles(socketPath, createdDir)
	})
	s.chrome.mu.Lock()
	s.chrome.stop = stop
	s.chrome.mu.Unlock()
	context.AfterFunc(ctx, stop)
	go s.acceptChromeHost(ctx, listener)
	return nil
}

func (h *chromeHost) shutdown() {
	h.mu.Lock()
	stop := h.stop
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func removeChromeHostFiles(socketPath string, removeDir bool) {
	if err := os.Remove(socketPath + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("Chrome host cleanup: %v", err)
	}
	if !removeDir {
		return
	}
	dir := filepath.Dir(socketPath)
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// chrome setup may have written into a directory this server created.
		if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) == 0 {
			log.Printf("Chrome host cleanup: %v", err)
		}
	}
}

func (s *Server) acceptChromeHost(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("Chrome host accept: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		current := &chromeConnection{Conn: conn, pending: make(map[string]chan claudeAISyncResult)}
		s.chrome.mu.Lock()
		if s.chrome.closed {
			s.chrome.mu.Unlock()
			_ = conn.Close()
			return
		}
		if s.chrome.connection != nil {
			s.chrome.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.chrome.connection = current
		s.chrome.mu.Unlock()
		go s.chrome.read(current)
	}
}

func (h *chromeHost) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connection != nil
}

func (h *chromeHost) read(conn *chromeConnection) {
	disconnectErr := errors.New("chrome host disconnected")
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
			disconnectErr = chromehost.ErrCompatibility
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

func (h *chromeHost) fetch(ctx context.Context, platform chromehost.Platform, path string) (importer.ClaudeAIResponse, error) {
	h.mu.Lock()
	conn := h.connection
	h.mu.Unlock()
	if conn == nil {
		return importer.ClaudeAIResponse{}, errors.New("chrome host not connected")
	}
	id := rand.Text()
	answer := make(chan claudeAISyncResult, 1)
	body, err := json.Marshal(map[string]any{"version": chromehost.Version, "id": id, "platform": platform, "path": path})
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
		return importer.ClaudeAIResponse{}, fmt.Errorf("writing Chrome request: %w", err)
	}
	return awaitBrowserResponse(ctx, answer)
}

func awaitBrowserResponse(ctx context.Context, answer <-chan claudeAISyncResult) (importer.ClaudeAIResponse, error) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return importer.ClaudeAIResponse{}, ctx.Err()
	case <-timer.C:
		return importer.ClaudeAIResponse{}, errors.New("browser fetch timed out")
	case response := <-answer:
		return importer.ClaudeAIResponse{Status: response.status, Body: response.body, RetryAfter: response.retryAfter}, response.err
	}
}
