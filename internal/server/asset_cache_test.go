package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99,
	0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func writeTestAsset(
	t *testing.T, dataDir, contentType string, body []byte,
) (string, string) {
	t.Helper()
	ref, err := assets.Reference(contentType, body)
	require.NoError(t, err)
	filename := strings.TrimPrefix(ref, "asset://")
	assetsDir := filepath.Join(dataDir, "assets")
	require.NoError(t, os.MkdirAll(assetsDir, 0o755))
	filePath := filepath.Join(assetsDir, filename)
	require.NoError(t, os.WriteFile(filePath, body, 0o644))
	return filename, filePath
}

func cacheAsset(t *testing.T, contentType string, body []byte) (string, int64, time.Time) {
	t.Helper()
	ref, err := assets.Reference(contentType, body)
	require.NoError(t, err)
	info := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	return strings.TrimPrefix(ref, "asset://"), int64(len(body)), info
}

func assetResponse(
	t *testing.T, srv *Server, filename string,
) *bytesOutput {
	t.Helper()
	response, err := srv.humaGetAsset(
		context.Background(), &assetInput{Filename: filename},
	)
	require.NoError(t, err)
	return response
}

func assetErrorStatus(t *testing.T, srv *Server, filename string) int {
	t.Helper()
	_, err := srv.humaGetAsset(
		context.Background(), &assetInput{Filename: filename},
	)
	require.Error(t, err)
	statusErr, ok := err.(interface{ GetStatus() int })
	require.True(t, ok)
	return statusErr.GetStatus()
}

func waitForAssetCacheServer(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			require.NoError(t, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestImageRenderCacheRepeatedRequestReadsOriginalOnce(t *testing.T) {
	dataDir := t.TempDir()
	filename, _ := writeTestAsset(t, dataDir, "image/png", testPNG)
	var fullBodyReads atomic.Int32
	previousReadAssetFile := readAssetFile
	readAssetFile = func(path string) ([]byte, error) {
		fullBodyReads.Add(1)
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readAssetFile = previousReadAssetFile })

	base := &Server{
		cfg:        config.Config{DataDir: dataDir},
		assetCache: newAssetCache(),
	}
	base.assetCache.maxEntries = 0
	firstBase := assetResponse(t, base, filename)
	secondBase := assetResponse(t, base, filename)
	assert.Equal(t, testPNG, firstBase.Body)
	assert.Equal(t, testPNG, secondBase.Body)
	assert.Equal(t, int32(2), fullBodyReads.Load())

	fullBodyReads.Store(0)
	head := &Server{
		cfg:        config.Config{DataDir: dataDir},
		assetCache: newAssetCache(),
	}
	firstHead := assetResponse(t, head, filename)
	secondHead := assetResponse(t, head, filename)
	assert.Equal(t, testPNG, firstHead.Body)
	assert.Equal(t, testPNG, secondHead.Body)
	assert.Equal(t, int32(1), fullBodyReads.Load())
	assert.Equal(t, firstHead.ContentType, secondHead.ContentType)
	t.Logf("base: full-body reads = 2; head: full-body reads = 1")
}

func TestImageRenderCacheAgeBoundary(t *testing.T) {
	body := append([]byte(nil), testPNG...)
	filename, size, modTime := cacheAsset(t, "image/png", body)
	start := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cache := newAssetCache()
	cache.now = func() time.Time { return start }
	require.True(t, cache.put(filename, "image/png", body, size, modTime))
	generation := cache.entries[filename].generatedAt

	cache.now = func() time.Time {
		return start.Add(assetCacheMaxAge - time.Nanosecond)
	}
	got, ok := cache.get(filename, "image/png", size, modTime)
	require.True(t, ok)
	assert.Equal(t, body, got)
	assert.Equal(t, generation, cache.entries[filename].generatedAt)

	cache.now = func() time.Time { return start.Add(assetCacheMaxAge) }
	_, ok = cache.get(filename, "image/png", size, modTime)
	assert.False(t, ok, "an entry at the seven-day cutoff is expired")

	cache.now = func() time.Time { return start }
	require.True(t, cache.put(filename, "image/png", body, size, modTime))
	cache.now = func() time.Time {
		return start.Add(assetCacheMaxAge + time.Nanosecond)
	}
	_, ok = cache.get(filename, "image/png", size, modTime)
	assert.False(t, ok, "an entry beyond the seven-day cutoff is expired")
	t.Logf("age: below=%t; equal=false; above=false; cutoff=7 days; generation=%s", true, generation.Format(time.RFC3339))
}

func TestImageRenderCacheCapacity(t *testing.T) {
	first := append([]byte(nil), testPNG...)
	second := append([]byte(nil), testPNG...)
	second[len(second)-1] = 0x81
	third := append([]byte(nil), testPNG...)
	third[len(third)-1] = 0x83
	firstName, firstSize, modTime := cacheAsset(t, "image/png", first)
	secondName, secondSize, _ := cacheAsset(t, "image/png", second)
	thirdName, thirdSize, _ := cacheAsset(t, "image/png", third)

	cache := newAssetCache()
	cache.maxEntries = 2
	cache.maxBytes = firstSize + secondSize
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	require.True(t, cache.put(firstName, "image/png", first, firstSize, modTime))
	now = now.Add(time.Second)
	require.True(t, cache.put(secondName, "image/png", second, secondSize, modTime))
	now = now.Add(time.Second)
	require.True(t, cache.put(thirdName, "image/png", third, thirdSize, modTime))
	_, firstPresent := cache.get(firstName, "image/png", firstSize, modTime)
	assert.False(t, firstPresent)
	assert.Len(t, cache.entries, 2)
	assert.LessOrEqual(t, cache.bytes, cache.maxBytes)

	oversize := newAssetCache()
	oversize.maxBytes = int64(len(first) - 1)
	assert.False(t, oversize.put(firstName, "image/png", first, firstSize, modTime))
	assert.Empty(t, oversize.entries)
	t.Logf("capacity: entries=%d; bytes=%d/%d", len(cache.entries), cache.bytes, cache.maxBytes)
}

func TestImageRenderCachePreservesDurableAssets(t *testing.T) {
	dataDir := t.TempDir()
	body := append([]byte(nil), testPNG...)
	filename, filePath := writeTestAsset(t, dataDir, "image/png", body)
	unrelatedBody := []byte("unrelated durable file")
	unrelatedPath := filepath.Join(dataDir, "assets", "unrelated.bin")
	require.NoError(t, os.WriteFile(unrelatedPath, unrelatedBody, 0o644))
	modTime := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filePath, modTime, modTime))
	require.NoError(t, os.Chtimes(unrelatedPath, modTime, modTime))

	beforeBody, err := os.ReadFile(filePath)
	require.NoError(t, err)
	beforeInfo, err := os.Stat(filePath)
	require.NoError(t, err)
	beforeUnrelated, err := os.Stat(unrelatedPath)
	require.NoError(t, err)

	cache := newAssetCache()
	cache.maxEntries = 1
	cache.now = func() time.Time {
		return modTime
	}
	got, err := cache.read(filename, filePath, "image/png")
	require.NoError(t, err)
	assert.Equal(t, body, got)

	secondBody := append([]byte(nil), testPNG...)
	secondBody[len(secondBody)-1] = 0x83
	secondName, secondPath := writeTestAsset(t, dataDir, "image/png", secondBody)
	require.NoError(t, os.Chtimes(secondPath, modTime, modTime))
	_, err = cache.read(secondName, secondPath, "image/png")
	require.NoError(t, err)

	cache.now = func() time.Time { return modTime.Add(assetCacheMaxAge) }
	cache.expireAndNextWait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache did not stop after cancellation")
	}

	afterBody, err := os.ReadFile(filePath)
	require.NoError(t, err)
	afterInfo, err := os.Stat(filePath)
	require.NoError(t, err)
	afterUnrelated, err := os.Stat(unrelatedPath)
	require.NoError(t, err)
	assert.Equal(t, beforeBody, afterBody)
	assert.True(t, beforeInfo.ModTime().Equal(afterInfo.ModTime()))
	assert.True(t, beforeUnrelated.ModTime().Equal(afterUnrelated.ModTime()))
	assert.Equal(t, unrelatedBody, func() []byte {
		data, readErr := os.ReadFile(unrelatedPath)
		require.NoError(t, readErr)
		return data
	}())
	t.Logf("durable: body=%d bytes; mtime=%s; unrelated=unchanged", len(afterBody), afterInfo.ModTime().Format(time.RFC3339Nano))
}

func TestImageRenderCacheRouteBoundaries(t *testing.T) {
	dataDir := t.TempDir()
	body := append([]byte(nil), testPNG...)
	filename, filePath := writeTestAsset(t, dataDir, "image/png", body)
	srv := &Server{
		cfg:        config.Config{DataDir: dataDir},
		assetCache: newAssetCache(),
	}
	response := assetResponse(t, srv, filename)
	assert.Equal(t, "image/png", response.ContentType)
	assert.Equal(t, "nosniff", response.NoSniff)
	assert.Equal(t, "public, max-age=31536000, immutable", response.CacheControl)
	assert.Equal(t, body, response.Body)

	assert.Equal(t, http.StatusBadRequest, assetErrorStatus(t, srv, "../"+filename))
	assert.Equal(t, http.StatusBadRequest, assetErrorStatus(t, srv, "nested/"+filename))
	assert.Equal(t, http.StatusForbidden, assetErrorStatus(t, srv, "image.svg"))

	assert.NoError(t, os.Remove(filePath))
	assert.Equal(t, http.StatusNotFound, assetErrorStatus(t, srv, filename))

	changed := append([]byte(nil), testPNG...)
	changed[len(changed)-1] = 0x84
	filename, filePath = writeTestAsset(t, dataDir, "image/png", body)
	assetResponse(t, srv, filename)
	require.NoError(t, os.WriteFile(filePath, changed, 0o644))
	changedModTime := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filePath, changedModTime, changedModTime))
	changedResponse := assetResponse(t, srv, filename)
	assert.Equal(t, changed, changedResponse.Body)

	resized := append(append([]byte(nil), changed...), 0x01)
	require.NoError(t, os.WriteFile(filePath, resized, 0o644))
	resizedModTime := changedModTime.Add(time.Second)
	require.NoError(t, os.Chtimes(filePath, resizedModTime, resizedModTime))
	resizedResponse := assetResponse(t, srv, filename)
	assert.Equal(t, resized, resizedResponse.Body)

	nonregular := filepath.Join(dataDir, "assets", "nonregular.png")
	require.NoError(t, os.Mkdir(nonregular, 0o755))
	assert.Equal(t, http.StatusNotFound, assetErrorStatus(t, srv, "nonregular.png"))
}

func TestImageRenderCacheFallbackAndLegacy(t *testing.T) {
	dataDir := t.TempDir()
	body := append([]byte(nil), testPNG...)
	canonical, _ := writeTestAsset(t, dataDir, "image/png", body)
	legacyPath := filepath.Join(dataDir, "assets", "legacy.png")
	require.NoError(t, os.WriteFile(legacyPath, body, 0o644))

	cache := newAssetCache()
	cache.maxBytes = int64(len(body) - 1)
	srv := &Server{cfg: config.Config{DataDir: dataDir}, assetCache: cache}
	assert.Equal(t, body, assetResponse(t, srv, canonical).Body)
	assert.Empty(t, cache.entries, "an oversize original remains servable")
	assert.Equal(t, body, assetResponse(t, srv, "legacy.png").Body)
	assert.Empty(t, cache.entries, "legacy filenames bypass admission")

	nilCache := &Server{cfg: config.Config{DataDir: dataDir}}
	assert.Equal(t, body, assetResponse(t, nilCache, canonical).Body)
	t.Logf("fallback: canonical oversize and legacy responses remained %d bytes", len(body))
}

func TestImageRenderCacheLifecycle(t *testing.T) {
	cache := newAssetCache()
	assert.Empty(t, cache.entries)
	assert.NotNil(t, cache.notify)

	body := append([]byte(nil), testPNG...)
	filename, size, modTime := cacheAsset(t, "image/png", body)
	cache.maxAge = 20 * time.Millisecond
	cache.now = time.Now
	require.True(t, cache.put(filename, "image/png", body, size, modTime))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return len(cache.entries) == 0
	}, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache did not stop after cancellation")
	}

	srv := New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}, dbtest.OpenTestDB(t), nil)
	assert.NotNil(t, srv.assetCache)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	waitForAssetCacheServer(t, listener.Addr().String())
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, srv.Shutdown(ctx))
	cancel()
	assert.ErrorIs(t, <-serveDone, http.ErrServerClosed)
	t.Logf("lifecycle: construction is idle; Run expires entries and Serve shuts it down")
}

func TestImageRenderCacheExpiryWorkBound(t *testing.T) {
	previousReadAssetFile := readAssetFile
	readAssetFile = func(string) ([]byte, error) {
		return nil, fmt.Errorf("expiry must not read durable files")
	}
	t.Cleanup(func() { readAssetFile = previousReadAssetFile })

	for _, residentCount := range []int{1, assetCacheMaxEntries} {
		cache := newAssetCache()
		cache.maxAge = time.Hour
		cache.now = func() time.Time {
			return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
		}
		for index := range residentCount {
			body := append([]byte(nil), testPNG...)
			body[len(body)-1] = byte(index + residentCount)
			filename, size, modTime := cacheAsset(t, "image/png", body)
			require.True(t, cache.put(filename, "image/png", body, size, modTime))
		}

		cache.maxAge = 0
		wait, ok := cache.expireAndNextWait()
		assert.False(t, ok)
		assert.Zero(t, wait)
		assert.Empty(t, cache.entries)
		assert.Zero(t, cache.bytes)
		t.Logf("expiry: resident entries checked=%d; durable reads=0", residentCount)
	}
}

func TestImageRenderCacheConcurrent(t *testing.T) {
	dataDir := t.TempDir()
	type assetFile struct {
		filename string
		path     string
		body     []byte
	}
	files := make([]assetFile, 4)
	for index := range files {
		body := append([]byte(nil), testPNG...)
		body[len(body)-1] = byte(0x90 + index)
		filename, path := writeTestAsset(t, dataDir, "image/png", body)
		files[index] = assetFile{filename: filename, path: path, body: body}
	}
	cache := newAssetCache()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(runDone)
	}()
	var wg sync.WaitGroup
	errs := make(chan error, len(files)*8)
	for _, file := range files {
		file := file
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					got, err := cache.read(file.filename, file.path, "image/png")
					if err != nil {
						errs <- err
						return
					}
					if !bytes.Equal(file.body, got) {
						errs <- fmt.Errorf("body mismatch for %s", file.filename)
						return
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("cache expiry loop did not stop")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	assert.LessOrEqual(t, len(cache.entries), cache.maxEntries)
	assert.LessOrEqual(t, cache.bytes, cache.maxBytes)
	t.Logf("concurrent: entries=%d; bytes=%d", len(cache.entries), cache.bytes)
}

func TestImageRenderCacheImmutableIdentityAndMediaType(t *testing.T) {
	body := append([]byte(nil), testPNG...)
	filename, size, modTime := cacheAsset(t, "image/png", body)
	cache := newAssetCache()
	require.True(t, cache.put(filename, "image/png", body, size, modTime))
	got, ok := cache.get(filename, "image/png", size, modTime)
	require.True(t, ok)
	got[0] ^= 0xff
	gotAgain, ok := cache.get(filename, "image/png", size, modTime)
	require.True(t, ok)
	assert.Equal(t, body, gotAgain)
	_, ok = cache.get(filename, "image/jpeg", size, modTime)
	assert.False(t, ok)
	_, ok = cache.get(filename, "image/png", size+1, modTime)
	assert.False(t, ok)
	_, ok = cache.get(filename, "image/png", size, modTime.Add(time.Second))
	assert.False(t, ok)
	assert.False(t, cache.put("legacy.png", "image/png", body, size, modTime))
	t.Logf("identity: %s remains bound to %s", filename, "image/png")
}
