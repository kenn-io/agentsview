package docbank

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.kenn.io/agentsview/internal/config"
)

// Shared across resolver instances. Workers acquire before starting HTTP work.
var requestPermits = make(chan struct{}, 4)

const (
	maxPending    = 32
	maxWaiters    = 128
	maxCachePages = 512
	maxCacheBytes = 8 << 20
)

type cacheEntry struct {
	key     string
	page    Page
	expires time.Time
	bytes   int
}
type job struct {
	key, generation, credential string
	query                       Lookup
	deadline                    time.Time
	done                        chan struct{}
	started                     bool
	page                        Page
}
type credentialResult struct {
	key string
	err error
}
type credentialRequest struct {
	ctx  context.Context
	done chan credentialResult
}
type resolver struct {
	keys             chan credentialRequest
	readKeyFile      func(context.Context, string) (string, error)
	cfg              config.DocbankIntegration
	client           *client
	ctx              context.Context
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	mu               sync.Mutex
	closed           bool
	waiters, pending int
	generation       string
	cooldown         time.Time
	jobs             map[string]*job
	queue            chan *job
	cache            map[string]*list.Element
	lru              *list.List
	cacheBytes       int
	now              func() time.Time
}

// NewResolver validates offline; unavailable credentials/services do not affect startup.
func NewResolver(ctx context.Context, cfg config.DocbankIntegration) (Resolver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(ctx)
	r := &resolver{cfg: cfg, ctx: root, cancel: cancel, jobs: make(map[string]*job), queue: make(chan *job, maxPending), cache: make(map[string]*list.Element), lru: list.New(), now: time.Now}
	if cfg.Enabled {
		r.client = newClient(cfg)
		if cfg.APIKeyFile != "" {
			r.keys = make(chan credentialRequest, maxPending)
			r.readKeyFile = readKeyFile
			r.workers.Add(1)
			go r.credentialWorker()
		}
		for range 4 {
			r.workers.Add(1)
			go r.worker()
		}
	}
	return r, nil
}

func (r *resolver) Lookup(ctx context.Context, q Lookup) (Page, error) {
	start := time.Now()
	ctx, cancel := context.WithDeadline(ctx, start.Add(3*time.Second))
	defer cancel()
	unavailable := Page{State: "unavailable"}
	if !r.cfg.Enabled {
		return unavailable, nil
	}
	if !digestPattern.MatchString(q.SHA256) || q.Size < 0 || q.ReferenceOffset < 0 {
		return unavailable, nil
	}
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return unavailable, nil
	}
	if r.waiters >= maxWaiters {
		r.mu.Unlock()
		return Page{State: "deferred"}, nil
	}
	r.waiters++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.waiters--; r.mu.Unlock() }()
	// Credential resolution precedes every cache read, even a hit.
	credential, err := r.credential(ctx)
	if err != nil {
		if errors.Is(err, errDeferred) {
			return Page{State: "deferred"}, nil
		}
		if ctx.Err() != nil {
			return unavailable, ctx.Err()
		}
		return unavailable, nil
	}
	cfgBytes, err := json.Marshal(r.cfg)
	if err != nil {
		return unavailable, nil
	}
	sum := sha256.Sum256(append(append(cfgBytes, 0), credential...))
	gen := hex.EncodeToString(sum[:])
	key := gen + ":" + q.SHA256 + ":" + strconv.Itoa(q.ReferenceOffset)
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return unavailable, nil
	}
	if r.generation != gen {
		r.generation = gen
		r.cache = make(map[string]*list.Element)
		r.lru.Init()
		r.cacheBytes = 0
		r.cooldown = time.Time{}
	}
	now := r.now()
	if !q.Refresh {
		if elem := r.cache[key]; elem != nil {
			entry := elem.Value.(*cacheEntry)
			if now.Before(entry.expires) {
				r.lru.MoveToFront(elem)
				p := checkedCopy(entry.page, q.Size)
				r.mu.Unlock()
				return p, nil
			}
			r.evict(elem)
		}
	}
	if now.Before(r.cooldown) {
		r.mu.Unlock()
		return Page{State: "deferred"}, nil
	}
	j := r.jobs[key]
	if j == nil {
		if r.pending >= maxPending {
			r.mu.Unlock()
			return Page{State: "deferred"}, nil
		}
		j = &job{key: key, generation: gen, credential: credential, query: q, deadline: start.Add(3 * time.Second), done: make(chan struct{})}
		r.jobs[key] = j
		r.pending++
		r.queue <- j
	}
	r.mu.Unlock()
	select {
	case <-j.done:
		if err := ctx.Err(); err != nil {
			return checkedCopy(j.page, q.Size), err
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return checkedCopy(j.page, q.Size), context.DeadlineExceeded
		}
		return checkedCopy(j.page, q.Size), nil
	case <-ctx.Done():
		r.mu.Lock()
		started := j.started
		r.mu.Unlock()
		state := "deferred"
		if started {
			state = "unavailable"
		}
		return Page{State: state}, ctx.Err()
	case <-r.ctx.Done():
		return unavailable, nil
	}
}

func (r *resolver) credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var key string
	if r.cfg.APIKeyEnv != "" {
		key = os.Getenv(r.cfg.APIKeyEnv)
	} else {
		req := credentialRequest{ctx: ctx, done: make(chan credentialResult, 1)}
		select {
		case r.keys <- req:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.ctx.Done():
			return "", errRemote
		default:
			return "", errDeferred
		}
		select {
		case result := <-req.done:
			if result.err != nil {
				return "", result.err
			}
			key = result.key
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.ctx.Done():
			return "", errRemote
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(key) > 16384 {
		return "", errRemote
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errRemote
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return "", errRemote
		}
	}
	return key, nil
}

// File I/O has a fixed worker and queue. A caller can leave at its deadline
// even if an underlying regular-file operation is slow.
func (r *resolver) credentialWorker() {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case req := <-r.keys:
			if err := req.ctx.Err(); err != nil {
				req.done <- credentialResult{err: err}
				continue
			}
			ctx, cancel := context.WithCancel(req.ctx)
			stop := context.AfterFunc(r.ctx, cancel)
			key, err := r.readKeyFile(ctx, r.cfg.APIKeyFile)
			stop()
			cancel()
			req.done <- credentialResult{key: key, err: err}
		}
	}
}

func readKeyFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Reject special files before opening; nonblocking open also closes the
	// stat/open replacement race for FIFOs. Validate the opened handle again.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return "", errRemote
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errRemote
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return "", errRemote
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return "", errRemote
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *resolver) worker() {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case j := <-r.queue:
			r.run(j)
		}
	}
}

func (r *resolver) run(j *job) {
	ctx, cancel := context.WithDeadline(r.ctx, j.deadline)
	defer cancel()
	acquired := false
	select {
	case requestPermits <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.pending--
	j.started = acquired
	// A rate limit may have arrived after this job was queued.
	cooling := r.now().Before(r.cooldown)
	r.mu.Unlock()
	p := Page{State: "deferred"}
	cooldown := time.Duration(0)
	if acquired {
		if !cooling && ctx.Err() == nil {
			p, cooldown = r.client.fetch(ctx, j.query, j.credential, r.now())
		}
		<-requestPermits
	}
	// Erase the retained raw key once the owned request finishes.
	j.credential = ""
	r.mu.Lock()
	defer r.mu.Unlock()
	if cooldown > 0 && r.generation == j.generation {
		until := r.now().Add(cooldown)
		if until.After(r.cooldown) {
			r.cooldown = until
		}
	}
	j.page = p
	delete(r.jobs, j.key)
	if r.generation == j.generation && !r.closed && p.State != "deferred" {
		r.store(j.key, p)
	}
	close(j.done)
}

func (r *resolver) store(key string, p Page) {
	if old := r.cache[key]; old != nil {
		r.evict(old)
	}
	ttl := 5 * time.Second
	switch p.State {
	case "found":
		ttl = 60 * time.Second
	case "not_found":
		ttl = 30 * time.Second
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return
	}
	// Include retained object/slice/key overhead as well as serialized strings.
	size := len(encoded) + len(key) + 256 + len(p.Matches)*128
	if size > maxCacheBytes {
		return
	}
	entry := &cacheEntry{key: key, page: p, expires: r.now().Add(ttl), bytes: size}
	r.cache[key] = r.lru.PushFront(entry)
	r.cacheBytes += size
	for r.lru.Len() > maxCachePages || r.cacheBytes > maxCacheBytes {
		r.evict(r.lru.Back())
	}
}

func (r *resolver) evict(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	r.cacheBytes -= entry.bytes
	delete(r.cache, entry.key)
	r.lru.Remove(elem)
}

func checkedCopy(p Page, size int64) Page {
	for _, m := range p.Matches {
		if m.Size != size {
			return Page{State: "unavailable", CheckedAt: copyValue(p.CheckedAt)}
		}
	}
	p.Matches = append([]Match(nil), p.Matches...)
	p.MatchOffsets = append([]int(nil), p.MatchOffsets...)
	p.CheckedAt = copyValue(p.CheckedAt)
	p.ReferenceTotal = copyValue(p.ReferenceTotal)
	p.NextReferenceOffset = copyValue(p.NextReferenceOffset)
	return p
}

func copyValue[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (r *resolver) Close() error {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.workers.Wait()
	if r.client != nil {
		r.client.http.CloseIdleConnections()
	}
	return nil
}
