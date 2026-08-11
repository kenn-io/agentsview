// Package transport brokers credential-free provider requests between the
// archive server and an authenticated desktop adapter. Providers define typed
// operations; adapters never receive arbitrary remote URLs.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("authenticated transport unavailable")

var leaseTimeout = 45 * time.Second

type Request struct {
	ID        string          `json:"id"`
	Provider  string          `json:"provider"`
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params"`
	Lease     string          `json:"lease"`
}

type Response struct {
	ID         string          `json:"id"`
	Lease      string          `json:"lease"`
	Status     int             `json:"status"`
	Body       json.RawMessage `json:"body"`
	RetryAfter string          `json:"retry_after,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type Broker struct {
	mu      sync.Mutex
	queue   chan *pending
	pending map[string]*pending
}

type pending struct {
	request Request
	result  chan Response
	timer   *time.Timer
}

func NewBroker() *Broker {
	return &Broker{queue: make(chan *pending, 128), pending: make(map[string]*pending)}
}

func (b *Broker) Do(ctx context.Context, provider, operation string, params any) (Response, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return Response{}, fmt.Errorf("encode transport params: %w", err)
	}
	p := &pending{request: Request{ID: randomID(), Provider: provider, Operation: operation, Params: data, Lease: randomID()}, result: make(chan Response, 1)}
	b.mu.Lock()
	b.pending[p.request.ID] = p
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, p.request.ID)
		if p.timer != nil {
			p.timer.Stop()
		}
		b.mu.Unlock()
	}()
	select {
	case b.queue <- p:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	select {
	case result := <-p.result:
		return result, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Claim waits for a request. A desktop adapter calls this only after an
// authenticated webview is available.
func (b *Broker) Claim(ctx context.Context) (Request, error) {
	select {
	case p := <-b.queue:
		b.mu.Lock()
		if _, ok := b.pending[p.request.ID]; !ok {
			b.mu.Unlock()
			return Request{}, ErrUnavailable
		}
		p.request.Lease = randomID()
		if p.timer != nil {
			p.timer.Stop()
		}
		lease := p.request.Lease
		p.timer = time.AfterFunc(leaseTimeout, func() { b.requeueExpired(p, lease) })
		request := p.request
		b.mu.Unlock()
		return request, nil
	case <-ctx.Done():
		return Request{}, ctx.Err()
	}
}

func (b *Broker) Complete(response Response) error {
	b.mu.Lock()
	p, ok := b.pending[response.ID]
	b.mu.Unlock()
	if !ok {
		return errors.New("unknown or expired transport request")
	}
	if response.Lease == "" || response.Lease != p.request.Lease {
		return errors.New("invalid transport request lease")
	}
	b.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	b.mu.Unlock()
	select {
	case p.result <- response:
		return nil
	default:
		return errors.New("transport request already completed")
	}
}

func (b *Broker) requeueExpired(p *pending, lease string) {
	b.mu.Lock()
	if current, ok := b.pending[p.request.ID]; !ok || current != p || p.request.Lease != lease {
		b.mu.Unlock()
		return
	}
	p.request.Lease = ""
	p.timer = nil
	b.mu.Unlock()
	select {
	case b.queue <- p:
	default:
		go func() { b.queue <- p }()
	}
}

func (b *Broker) Available() bool {
	// Availability is intentionally inferred by a claimed request completing;
	// a connected adapter need not expose credentials to prove its state.
	return b != nil
}

func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}
