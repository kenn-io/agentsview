package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/requestsign"
)

const defaultMachineSigningListen = "127.0.0.1:18081"

// RestrictedListener is a private machine listener with its own resource limits.
// It is never registered with managed browser proxy/routing setup.
type RestrictedListener struct {
	server     *http.Server
	listener   net.Listener
	closeState func() error
	once       sync.Once
}

// StartRestrictedListener starts only after explicit policy configuration. Plain
// HTTP is supported solely for an explicitly trusted loopback TLS terminator.
func (s *Server) StartRestrictedListener(ctx context.Context, policyFile string) (*RestrictedListener, error) {
	policy, _, err := requestsign.ReadPolicy(policyFile)
	if err != nil {
		return nil, err
	}
	address := policy.Listen
	if address == "" {
		address = defaultMachineSigningListen
	}
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return nil, errors.New("machine signing listener must bind a loopback IP")
	}
	if !policy.StripPrefix {
		return nil, errors.New("plain machine listener requires explicit strip_prefix TLS termination; use RestrictedHandler with a native TLS listener for preserved-prefix transport")
	}
	handler, closeState, err := s.RestrictedHandler(policyFile)
	if err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		_ = closeState()
		return nil, fmt.Errorf("opening machine signing listener at %q failed: %w", address, err)
	}
	srv := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		MaxHeaderBytes: requestsign.MaxHeaderBytes, IdleTimeout: 60 * time.Second,
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	ingress := &RestrictedListener{server: srv, listener: ln, closeState: closeState}
	s.mu.Lock()
	if s.restrictedListener != nil {
		s.mu.Unlock()
		_ = ln.Close()
		_ = closeState()
		return nil, errors.New("machine signing listener already running")
	}
	s.restrictedListener = ingress
	s.mu.Unlock()
	go ingress.serve()
	return ingress, nil
}

func (l *RestrictedListener) serve() {
	err := l.server.Serve(l.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return
	}
	if err != nil {
		logger := l.server.ErrorLog
		if logger == nil {
			logger = log.Default()
		}
		logger.Printf("machine signing listener stopped: %v", err)
	}
	l.close()
}

func (l *RestrictedListener) close() { l.once.Do(func() { _ = l.server.Close(); _ = l.closeState() }) }

func (l *RestrictedListener) shutdown(ctx context.Context) error {
	err := l.server.Shutdown(ctx)
	l.close()
	return err
}
