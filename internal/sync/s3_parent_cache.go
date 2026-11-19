package sync

import (
	"container/list"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	gosync "sync"

	"go.kenn.io/agentsview/internal/parser"
)

// s3CodexParentCacheMaxBytes bounds the disk one sync pass spends keeping
// downloaded Codex fork parents for later children. The most recent download
// stays cached even when it alone exceeds the bound, so the cache never holds
// more than this plus one object.
const s3CodexParentCacheMaxBytes = int64(512 << 20)

type s3CodexParentCacheContextKey struct{}

// s3CodexParentCache keeps the Codex fork-parent rollouts downloaded during
// one sync pass, so every child of a parent hard-links (or copies) one
// download instead of fetching the object again. The files live in a temp
// directory that is removed when the pass ends; nothing persists across
// passes, so S3 roots still have no local mirror.
//
// Entries are keyed by the object version each child's parent listing
// reported. A parent that grew after it was cached lists with a new size and
// ETag, so a child that forked from the newer turns downloads the current
// parent instead of a copy that ends before its fork point.
type s3CodexParentCache struct {
	maxBytes int64
	tempRoot string // parent of the cache directory; empty means os.TempDir

	mu      gosync.Mutex
	dir     string
	seq     int
	bytes   int64
	entries map[s3CodexParentKey]*s3CodexParentEntry
	recent  *list.List // cached entries, most recently used first
	closed  bool
}

// s3CodexParentKey names one version of a parent object, as listed.
type s3CodexParentKey struct {
	uri         string
	size        int64
	fingerprint string
}

type s3CodexParentEntry struct {
	key   s3CodexParentKey
	ready chan struct{} // closed once the download finished
	err   error         // set before ready closes; never changed after
	path  string        // guarded by mu; empty once evicted
	size  int64
	elem  *list.Element
}

// withS3CodexParentCache scopes a parent cache to ctx and returns the func
// that removes its files. A ctx that already carries a cache keeps it, so a
// pass that processes files in several batches shares one cache.
func withS3CodexParentCache(ctx context.Context) (context.Context, func()) {
	if s3CodexParentCacheFor(ctx) != nil {
		return ctx, func() {}
	}
	cache := newS3CodexParentCache(s3CodexParentCacheMaxBytes)
	return context.WithValue(ctx, s3CodexParentCacheContextKey{}, cache), cache.close
}

func s3CodexParentCacheFor(ctx context.Context) *s3CodexParentCache {
	cache, _ := ctx.Value(s3CodexParentCacheContextKey{}).(*s3CodexParentCache)
	return cache
}

func newS3CodexParentCache(maxBytes int64) *s3CodexParentCache {
	return &s3CodexParentCache{
		maxBytes: maxBytes,
		entries:  make(map[s3CodexParentKey]*s3CodexParentEntry),
		recent:   list.New(),
	}
}

// materialize writes the listed object to dst. With a cache, concurrent and
// later callers for the same object version share one download; without one
// (or after the pass closed it) the object is fetched straight to dst.
func (c *s3CodexParentCache) materialize(
	ctx context.Context, obj parser.S3Object, dst string,
) error {
	if c == nil {
		return downloadS3Object(obj.URI, dst)
	}
	key := s3CodexParentKey{uri: obj.URI, size: obj.Size, fingerprint: obj.Fingerprint}
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return downloadS3Object(obj.URI, dst)
		}
		entry := c.entries[key]
		if entry == nil {
			entry = &s3CodexParentEntry{key: key, ready: make(chan struct{})}
			c.entries[key] = entry
			c.mu.Unlock()
			c.fill(entry)
		} else {
			c.mu.Unlock()
		}
		select {
		case <-entry.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if entry.err != nil {
			return entry.err
		}
		src, linked, err := c.link(entry, dst)
		if err != nil {
			return err
		}
		if linked {
			return nil
		}
		if src == nil {
			// Evicted by another download before this caller linked it.
			continue
		}
		err = copyToNewFile(src, dst)
		_ = src.Close()
		return err
	}
}

// link hard-links the cached file to dst. When the filesystem refuses the
// link it opens the cached file instead so the caller can copy it; the open
// handle keeps the bytes readable even if the entry is evicted meanwhile.
// A nil file with linked false means the entry was already evicted.
func (c *s3CodexParentCache) link(
	entry *s3CodexParentEntry, dst string,
) (src *os.File, linked bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.path == "" {
		return nil, false, nil
	}
	c.recent.MoveToFront(entry.elem)
	// Never write through an existing name: it could share the cached inode.
	_ = os.Remove(dst)
	if os.Link(entry.path, dst) == nil {
		return nil, true, nil
	}
	src, err = os.Open(entry.path)
	return src, false, err
}

func (c *s3CodexParentCache) fill(entry *s3CodexParentEntry) {
	defer close(entry.ready)
	path, err := c.nextPath()
	if err == nil {
		err = downloadS3Object(entry.key.uri, path)
	}
	var size int64
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(path); err == nil {
			size = info.Size()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.closed {
		err = os.ErrClosed
	}
	if err != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		delete(c.entries, entry.key)
		entry.err = err
		return
	}
	entry.path, entry.size = path, size
	entry.elem = c.recent.PushFront(entry)
	c.bytes += size
	for c.bytes > c.maxBytes && c.recent.Back() != entry.elem {
		c.evict(c.recent.Back().Value.(*s3CodexParentEntry))
	}
}

func (c *s3CodexParentCache) nextPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", os.ErrClosed
	}
	if c.dir == "" {
		dir, err := os.MkdirTemp(c.tempRoot, "avs3-parents-")
		if err != nil {
			return "", err
		}
		c.dir = dir
	}
	c.seq++
	return filepath.Join(c.dir, strconv.Itoa(c.seq)), nil
}

// evict drops a cached entry. Children that already linked or opened the
// file keep their copy. Callers hold mu.
func (c *s3CodexParentCache) evict(entry *s3CodexParentEntry) {
	_ = os.Remove(entry.path)
	c.recent.Remove(entry.elem)
	delete(c.entries, entry.key)
	c.bytes -= entry.size
	entry.path = ""
}

// close removes every cached file. Later materialize calls fetch directly.
func (c *s3CodexParentCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
	c.entries = nil
	c.recent.Init()
	c.bytes = 0
}

// downloadS3Object streams the object at uri into a new file at dst and
// removes the partial file on failure.
func downloadS3Object(uri, dst string) error {
	rc, err := fetchS3Object(uri)
	if err != nil {
		return err
	}
	defer rc.Close()
	return copyToNewFile(rc, dst)
}

func copyToNewFile(src io.Reader, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(dst)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return nil
}
