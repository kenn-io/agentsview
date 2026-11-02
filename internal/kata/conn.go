package kata

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const connTTL = 30 * time.Second

// Conn shares one cached probe between the status route, the version
// capability, the CLI and the filer. A delegated call that fails with a
// transport or auth error invalidates the cache so the next Ready re-probes.
type Conn struct {
	cfg   Config
	probe func(context.Context, Config) (Status, *Client)
	now   func() time.Time

	mu       sync.Mutex
	status   Status
	client   *Client
	checked  time.Time
	tokenSet bool
	located  string
	// probes counts stored probe results; a caller that waited compares it
	// with the count it saw before waiting.
	probes atomic.Uint64
	// lastReady records whether the last completed check found Kata ready,
	// so LastReady never waits behind a probe that holds mu.
	lastReady atomic.Bool
}

func NewConn(cfg Config) *Conn {
	return &Conn{cfg: cfg, probe: Probe, now: time.Now}
}

func (c *Conn) Config() Config { return c.cfg }

func (c *Conn) Status(ctx context.Context, fresh bool) Status {
	seen := c.probes.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusAfter(ctx, fresh, seen)
}

// statusAfter returns a probe that finished after the caller saw seen
// probes; it is fresh enough for a caller that waited for the lock. A
// canceled probe clears checked and is not shared.
func (c *Conn) statusAfter(ctx context.Context, fresh bool, seen uint64) Status {
	if fresh && c.probes.Load() != seen && !c.checked.IsZero() {
		return c.status
	}
	return c.statusLocked(ctx, fresh)
}

// LastReady reports whether the most recent completed check found Kata
// ready. It never probes: /api/v1/version calls it on every request, and a
// probe can run `kata daemon locate`, which may start a Kata daemon. Status
// checks refresh it and a failed Kata call clears it.
func (c *Conn) LastReady() bool {
	if c.cfg.TokenEnv != "" && c.cfg.currentToken() == "" {
		return false
	}
	return c.lastReady.Load()
}

// statusLocked probes as needed while the caller holds mu.
func (c *Conn) statusLocked(ctx context.Context, fresh bool) Status {
	if !fresh && c.status.State == StateReady && !c.checked.IsZero() && c.now().Sub(c.checked) < connTTL &&
		(c.cfg.TokenEnv == "" || c.cfg.currentToken() != "") {
		return c.status
	}
	cfg := c.cfg
	if cfg.Enabled && cfg.Hub && (cfg.TokenRequired || cfg.TokenEnv != "") && cfg.currentToken() == "" {
		st, client := c.probe(ctx, cfg)
		return c.storeStatus(ctx, st, client)
	}
	if cfg.Enabled && cfg.Hub && cfg.Endpoint == "" {
		if c.located == "" {
			ep, err := resolveEndpoint(ctx, "")
			if err != nil {
				st := failed(Status{Project: cfg.Project}, StateUnavailable,
					fmt.Errorf("%w: daemon discovery failed", ErrUnavailable), cfg.currentToken())
				return c.storeStatus(ctx, st, nil)
			}
			c.located = ep
		}
		cfg.Endpoint = c.located
	}
	st, client := c.probe(ctx, cfg)
	if st.State == StateUnavailable {
		c.located = ""
	}
	return c.storeStatus(ctx, st, client)
}

// storeStatus records a probe result while the caller holds mu.
func (c *Conn) storeStatus(ctx context.Context, st Status, client *Client) Status {
	if st.State != c.status.State || c.checked.IsZero() {
		if st.Message != "" {
			log.Printf("kata: status %s: %s", st.State, st.Message)
		} else {
			log.Printf("kata: status %s", st.State)
		}
	}
	c.status, c.client, c.checked, c.tokenSet = st, client, c.now(), c.cfg.currentToken() != ""
	c.probes.Add(1)
	if st.State == StateUnavailable && ctx.Err() != nil {
		// A canceled probe cannot establish that Kata is unavailable. Let
		// the next check probe again, and keep the last known readiness.
		c.checked = time.Time{}
		return st
	}
	c.lastReady.Store(st.State == StateReady)
	return st
}

func (c *Conn) Ready(ctx context.Context) bool { return c.Status(ctx, false).State == StateReady }

func (c *Conn) Client(ctx context.Context) (*Client, error) {
	st := c.Status(ctx, false)
	c.mu.Lock()
	defer c.mu.Unlock()
	if st.State != StateReady || c.client == nil {
		return nil, fmt.Errorf("%w: %s: %s", ErrNotReady, st.State, st.Message)
	}
	return c.client, nil
}

func (c *Conn) observe(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotReady) || StatusOf(err) == http.StatusUnauthorized || StatusOf(err) == http.StatusForbidden || StatusOf(err) >= 500 {
		c.mu.Lock()
		c.checked = time.Time{}
		c.client = nil
		c.lastReady.Store(false)
		c.mu.Unlock()
	}
	return err
}

func (c *Conn) InstanceUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return ""
	}
	return c.client.InstanceUID()
}

func (c *Conn) ProjectUID(ctx context.Context) (string, error) {
	cl, err := c.Client(ctx)
	if err != nil {
		return "", err
	}
	uid, err := cl.ProjectUID(ctx)
	return uid, c.observe(err)
}

func (c *Conn) FindByMetadata(ctx context.Context, key, value string) ([]Issue, error) {
	cl, err := c.Client(ctx)
	if err != nil {
		return nil, err
	}
	out, err := cl.FindByMetadata(ctx, key, value)
	return out, c.observe(err)
}

func (c *Conn) CreateIssue(ctx context.Context, key string, req CreateIssue) (CreateResult, error) {
	cl, err := c.Client(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	out, err := cl.CreateIssue(ctx, key, req)
	return out, c.observe(err)
}

func (c *Conn) GetIssue(ctx context.Context, ref string) (Issue, error) {
	cl, err := c.Client(ctx)
	if err != nil {
		return Issue{}, err
	}
	out, err := cl.GetIssue(ctx, ref)
	return out, c.observe(err)
}

func (c *Conn) Reopen(ctx context.Context, ref string) (bool, error) {
	cl, err := c.Client(ctx)
	if err != nil {
		return false, err
	}
	out, err := cl.Reopen(ctx, ref)
	return out, c.observe(err)
}

func (c *Conn) Comment(ctx context.Context, ref, key, body string) error {
	cl, err := c.Client(ctx)
	if err != nil {
		return err
	}
	return c.observe(cl.Comment(ctx, ref, key, body))
}

func (c *Conn) AddLabel(ctx context.Context, ref, label string) error {
	cl, err := c.Client(ctx)
	if err != nil {
		return err
	}
	return c.observe(cl.AddLabel(ctx, ref, label))
}
