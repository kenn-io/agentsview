package docbank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestDocbankTTLAndRotation(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status int
		ttl    time.Duration
	}{{"found", 200, 60 * time.Second}, {"not_found", 200, 30 * time.Second}, {"unavailable", 503, 5 * time.Second}} {
		t.Run(tc.state, func(t *testing.T) {
			var calls atomic.Int32
			var fail atomic.Bool
			rr, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if fail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(tc.status)
				p := rawPage()
				if tc.state == "found" {
					p = rawPage(rawItem(1, testVersion))
				}
				assert.NoError(t, json.NewEncoder(w).Encode(p))
			})
			r := rr.(*resolver)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			r.now = func() time.Time { return time.Unix(0, clock.Load()) }
			q := Lookup{SHA256: testHash, Size: 7}
			p, e := r.Lookup(t.Context(), q)
			require.NoError(t, e)
			assert.Equal(t, tc.state, p.State)
			clock.Add(int64(tc.ttl - time.Nanosecond))
			_, e = r.Lookup(t.Context(), q)
			require.NoError(t, e)
			assert.EqualValues(t, 1, calls.Load())
			clock.Add(1)
			_, e = r.Lookup(t.Context(), q)
			require.NoError(t, e)
			assert.EqualValues(t, 2, calls.Load())
			t.Setenv("DOCBANK_TEST_KEY", "rotated-test-key")
			fail.Store(true)
			p, e = r.Lookup(t.Context(), q)
			require.NoError(t, e)
			assert.Equal(t, "unavailable", p.State)
			assert.Empty(t, p.Matches)
			assert.EqualValues(t, 3, calls.Load())
			t.Setenv("DOCBANK_TEST_KEY", "")
			p, e = r.Lookup(t.Context(), q)
			require.NoError(t, e)
			assert.Equal(t, "unavailable", p.State)
			assert.EqualValues(t, 3, calls.Load())
		})
	}
}

func TestDocbankRefreshAndCachedSize(t *testing.T) {
	var calls atomic.Int32
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage(rawItem(1, testVersion))))
	})
	q := Lookup{SHA256: testHash, Size: 7}
	p, e := r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "found", p.State)
	q.Size = 8
	p, e = r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	assert.EqualValues(t, 1, calls.Load())
	q.Size = 7
	q.Refresh = true
	p, e = r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	q.Refresh = false
	p, e = r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	assert.Empty(t, p.Matches)
	assert.EqualValues(t, 2, calls.Load())
}

func TestDocbankCredentials(t *testing.T) {
	_, srv := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "file-test-key", r.Header.Get("X-Api-Key"))
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage()))
	})
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("file-test-key\n"), 0o600))
	rr, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: srv.URL, APIKeyFile: path})
	require.NoError(t, e)
	defer rr.Close()
	q := Lookup{SHA256: testHash, Size: 7}
	p, e := rr.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "not_found", p.State)
	for _, data := range []string{"", strings.Repeat("x", 16385), "bad\nkey"} {
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		p, e = rr.Lookup(t.Context(), q)
		require.NoError(t, e)
		assert.Equal(t, "unavailable", p.State)
	}
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	p, e = rr.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	if binary, e := exec.LookPath("mkfifo"); e == nil {
		fifo := filepath.Join(t.TempDir(), "fifo")
		require.NoError(t, exec.CommandContext(t.Context(), binary, fifo).Run())
		rr, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: srv.URL, APIKeyFile: fifo})
		require.NoError(t, e)
		defer rr.Close()
		p, e = rr.Lookup(t.Context(), q)
		require.NoError(t, e)
		assert.Equal(t, "unavailable", p.State)
	}
}

func TestDocbankRateLimit(t *testing.T) {
	var calls atomic.Int32
	rr, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "999999")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	r := rr.(*resolver)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	q := Lookup{SHA256: testHash, Size: 7, Refresh: true}
	p, e := r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	clock.Add(int64(59 * time.Second))
	p, e = r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.Equal(t, "deferred", p.State)
	assert.EqualValues(t, 1, calls.Load())
	clock.Add(int64(time.Second))
	_, e = r.Lookup(t.Context(), q)
	require.NoError(t, e)
	assert.EqualValues(t, 2, calls.Load())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func bubbleResolver(t *testing.T, handler roundTripFunc) *resolver {
	t.Helper()
	rr, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: "http://127.0.0.1:1", APIKeyEnv: "DOCBANK_TEST_KEY"})
	require.NoError(t, e)
	r := rr.(*resolver)
	r.client.http.Transport = handler
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

func emptyResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[],"total":0,"limit":100,"offset":0}`)), Header: make(http.Header)}
}

func TestDocbankAdmission(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	synctest.Test(t, func(t *testing.T) {
		saved := requestPermits
		requestPermits = make(chan struct{}, 4)
		t.Cleanup(func() { requestPermits = saved })
		release := make(chan struct{})
		var active, peak atomic.Int32
		handler := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			select {
			case <-release:
				return emptyResponse(), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		a, b := bubbleResolver(t, handler), bubbleResolver(t, handler)
		var wg sync.WaitGroup
		lookup := func(r *resolver, i int) {
			defer wg.Done()
			_, _ = r.Lookup(t.Context(), Lookup{SHA256: fmt.Sprintf("%064x", i), Size: 7})
		}
		for i := 1; i <= 4; i++ {
			wg.Add(1)
			go lookup(a, i)
		}
		synctest.Wait()
		assert.EqualValues(t, 4, active.Load())
		// All four process-wide permits are owned by a. b can queue 32 jobs only.
		for i := 5; i <= 36; i++ {
			wg.Add(1)
			go lookup(b, i)
		}
		synctest.Wait()
		p, e := b.Lookup(t.Context(), Lookup{SHA256: fmt.Sprintf("%064x", 37), Size: 7})
		require.NoError(t, e)
		assert.Equal(t, "deferred", p.State)
		close(release)
		wg.Wait()
		assert.LessOrEqual(t, peak.Load(), int32(4))
	})
}

func TestDocbankCoalescingAndWaiters(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	synctest.Test(t, func(t *testing.T) {
		saved := requestPermits
		requestPermits = make(chan struct{}, 4)
		t.Cleanup(func() { requestPermits = saved })
		release := make(chan struct{})
		var calls atomic.Int32
		r := bubbleResolver(t, func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-release:
				return emptyResponse(), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		var wg sync.WaitGroup
		results := make(chan struct {
			page Page
			err  error
		}, 128)
		for range 128 {
			wg.Go(func() {
				p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
				results <- struct {
					page Page
					err  error
				}{p, e}
			})
		}
		synctest.Wait()
		p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
		require.NoError(t, e)
		assert.Equal(t, "deferred", p.State)
		assert.EqualValues(t, 1, calls.Load())
		close(release)
		wg.Wait()
		close(results)
		for result := range results {
			require.NoError(t, result.err)
			assert.Equal(t, "not_found", result.page.State)
		}
	})
}

func TestDocbankCancellation(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	synctest.Test(t, func(t *testing.T) {
		saved := requestPermits
		requestPermits = make(chan struct{}, 4)
		t.Cleanup(func() { requestPermits = saved })
		release := make(chan struct{})
		r := bubbleResolver(t, func(req *http.Request) (*http.Response, error) {
			select {
			case <-release:
				return emptyResponse(), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, e := r.Lookup(ctx, Lookup{SHA256: testHash, Size: 7}); done <- e }()
		synctest.Wait()
		other := make(chan Page, 1)
		go func() { p, _ := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7}); other <- p }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		close(release)
		assert.Equal(t, "not_found", (<-other).State)
		// Close cancels active work and drains its workers, including blocked requests.
		held := bubbleResolver(t, func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		finished := make(chan Page, 1)
		started := time.Now()
		go func() { p, _ := held.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7}); finished <- p }()
		assert.Equal(t, "unavailable", (<-finished).State)
		assert.Equal(t, 2*time.Second, time.Since(started), "HTTP work times out before the operation budget")
		go func() { _, _ = held.Lookup(t.Context(), Lookup{SHA256: strings.Repeat("b", 64), Size: 7}) }()
		synctest.Wait()
		require.NoError(t, held.Close())
		p, e := held.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
		require.NoError(t, e)
		assert.Equal(t, "unavailable", p.State)
	})
}

func TestDocbankCacheBytes(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(strconv.FormatBool(large), func(t *testing.T) {
			var calls atomic.Int32
			rr, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				hash := r.URL.Query().Get("sha256")
				p := rawPage()
				if large {
					items := make([]map[string]any, 100)
					for i := range items {
						items[i] = rawItem(i+1, testVersion)
						items[i]["version"].(map[string]any)["blob_hash"] = hash
						items[i]["node"].(map[string]any)["name"] = strings.Repeat("n", 4096)
						items[i]["path"] = "/" + strings.Repeat("p", 4095)
					}
					p = rawPage(items...)
				}
				assert.NoError(t, json.NewEncoder(w).Encode(p))
			})
			count := 513
			if large {
				count = 11
			}
			for i := 1; i <= count; i++ {
				p, e := rr.Lookup(t.Context(), Lookup{SHA256: fmt.Sprintf("%064x", i), Size: 7})
				require.NoError(t, e)
				if large {
					require.Equal(t, "found", p.State)
				} else {
					require.Equal(t, "not_found", p.State)
				}
			}
			_, e := rr.Lookup(t.Context(), Lookup{SHA256: fmt.Sprintf("%064x", count), Size: 7})
			require.NoError(t, e)
			assert.EqualValues(t, count, calls.Load())
			_, e = rr.Lookup(t.Context(), Lookup{SHA256: fmt.Sprintf("%064x", 1), Size: 7})
			require.NoError(t, e)
			assert.EqualValues(t, count+1, calls.Load(), "oldest page must have been evicted")
		})
	}
}

func TestDocbankQueueDeadline(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	synctest.Test(t, func(t *testing.T) {
		saved := requestPermits
		requestPermits = make(chan struct{}, 4)
		t.Cleanup(func() { requestPermits = saved })
		// Model another resolver holding all process permits while this request queues.
		for range 4 {
			requestPermits <- struct{}{}
		}
		var calls atomic.Int32
		r := bubbleResolver(t, func(*http.Request) (*http.Response, error) { calls.Add(1); return emptyResponse(), nil })
		started := time.Now()
		p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
		require.ErrorIs(t, e, context.DeadlineExceeded)
		assert.Equal(t, "deferred", p.State)
		assert.Equal(t, 3*time.Second, time.Since(started))
		assert.Zero(t, calls.Load())
		for range 4 {
			<-requestPermits
		}
	})
}

func TestDocbankOfflineAndNoBrowserURL(t *testing.T) {
	var calls atomic.Int32
	_, srv := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage(rawItem(1, testVersion))))
	})
	assert.Zero(t, calls.Load(), "constructor must not probe the remote service")
	rr, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: srv.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})
	require.NoError(t, e)
	defer rr.Close()
	p, e := rr.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, e)
	require.Len(t, p.Matches, 1)
	assert.Empty(t, p.Matches[0].OpenURL)
	disabled, e := NewResolver(t.Context(), config.DocbankIntegration{URL: "invalid", APIKeyFile: "missing"})
	require.NoError(t, e)
	defer disabled.Close()
	p, e = disabled.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, e)
	assert.Equal(t, "unavailable", p.State)
	assert.EqualValues(t, 1, calls.Load())
}

func TestDocbankRotationIsolatesCooldown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r, _ := resolverFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "test-key" {
			close(entered)
			<-release
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(rawPage()))
	})
	old := make(chan Page, 1)
	go func() { p, _ := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7}); old <- p }()
	<-entered
	t.Setenv("DOCBANK_TEST_KEY", "rotated-test-key")
	p, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7})
	require.NoError(t, e)
	assert.Equal(t, "not_found", p.State)
	close(release)
	assert.Equal(t, "unavailable", (<-old).State)
	p, e = r.Lookup(t.Context(), Lookup{SHA256: strings.Repeat("b", 64), Size: 7})
	require.NoError(t, e)
	assert.Equal(t, "not_found", p.State)
}

func TestDocbankCredentialDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("test-key"), 0o600))
	synctest.Test(t, func(t *testing.T) {
		saved := requestPermits
		requestPermits = make(chan struct{}, 4)
		t.Cleanup(func() { requestPermits = saved })
		rr, e := NewResolver(t.Context(), config.DocbankIntegration{Enabled: true, URL: "http://127.0.0.1:1", APIKeyFile: path})
		require.NoError(t, e)
		r := rr.(*resolver)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		held := make(chan struct{})
		r.readKeyFile = func(context.Context, string) (string, error) { <-held; return "test-key", nil }
		done := make(chan error, 1)
		go func() { _, e := r.Lookup(t.Context(), Lookup{SHA256: testHash, Size: 7}); done <- e }()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		// Caller cancellation must not wait for a stalled regular-file boundary.
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		close(held)
	})
}
