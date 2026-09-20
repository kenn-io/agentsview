package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// variantPNG returns a copy of testPNG whose last byte is replaced so the
// body hashes to a different asset filename.
func variantPNG(last byte) []byte {
	body := append([]byte(nil), testPNG...)
	body[len(body)-1] = last
	return body
}

func assetResponse(
	t *testing.T, srv *Server, filename string,
) *bytesOutput {
	t.Helper()
	response, err := srv.humaGetAsset(
		t.Context(), &assetInput{Filename: filename},
	)
	require.NoError(t, err)
	return response
}

func assetErrorStatus(t *testing.T, srv *Server, filename string) int {
	t.Helper()
	_, err := srv.humaGetAsset(
		t.Context(), &assetInput{Filename: filename},
	)
	require.Error(t, err)
	statusErr, ok := err.(interface{ GetStatus() int })
	require.True(t, ok)
	return statusErr.GetStatus()
}

func runAssetCache(t *testing.T, cache *assetCache) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			require.FailNow(t, "cache sweep loop did not stop after cancellation")
		}
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

	srv := &Server{
		cfg:        config.Config{DataDir: dataDir},
		assetCache: newAssetCache(),
	}
	first := assetResponse(t, srv, filename)
	second := assetResponse(t, srv, filename)
	assert.Equal(t, testPNG, first.Body)
	assert.Equal(t, testPNG, second.Body)
	assert.Equal(t, int32(1), fullBodyReads.Load())
	assert.Equal(t, first.ContentType, second.ContentType)
}

func TestImageRenderCacheWarmedEntryOpenFailure(t *testing.T) {
	dataDir := t.TempDir()
	body := append([]byte(nil), testPNG...)
	filename, filePath := writeTestAsset(t, dataDir, "image/png", body)
	srv := &Server{
		cfg:        config.Config{DataDir: dataDir},
		assetCache: newAssetCache(),
	}
	assetResponse(t, srv, filename)
	beforeBody, err := os.ReadFile(filePath)
	require.NoError(t, err)
	beforeInfo, err := os.Stat(filePath)
	require.NoError(t, err)

	var fullBodyReads atomic.Int32
	previousReadAssetFile := readAssetFile
	readAssetFile = func(path string) ([]byte, error) {
		fullBodyReads.Add(1)
		return os.ReadFile(path)
	}
	previousOpenAssetReadOnly := openAssetReadOnly
	openAssetReadOnly = func(string) (*os.File, error) {
		return nil, errors.New("read-only eligibility denied")
	}
	t.Cleanup(func() {
		readAssetFile = previousReadAssetFile
		openAssetReadOnly = previousOpenAssetReadOnly
	})

	assert.Equal(t, http.StatusNotFound, assetErrorStatus(t, srv, filename))
	assert.Zero(t, fullBodyReads.Load(), "an open failure must not fall through to a body read")
	afterBody, err := os.ReadFile(filePath)
	require.NoError(t, err)
	afterInfo, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, beforeBody, afterBody)
	assert.True(t, beforeInfo.ModTime().Equal(afterInfo.ModTime()))
	assert.Equal(t, beforeInfo.Size(), afterInfo.Size())
}

func TestImageRenderCacheAgeBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := append([]byte(nil), testPNG...)
		filename, size, modTime := cacheAsset(t, "image/png", body)
		cache := newAssetCache()
		require.True(t, cache.put(filename, "image/png", body, size, modTime))

		time.Sleep(assetCacheMaxAge - time.Nanosecond)
		got, ok := cache.get(filename, "image/png", size, modTime)
		require.True(t, ok, "an entry below the seven-day cutoff is served")
		assert.Equal(t, body, got)

		time.Sleep(2 * time.Nanosecond)
		_, ok = cache.get(filename, "image/png", size, modTime)
		assert.False(t, ok, "an entry beyond the seven-day cutoff is expired")

		require.True(t, cache.put(filename, "image/png", body, size, modTime))
		_, ok = cache.get(filename, "image/png", size, modTime)
		assert.True(t, ok, "a re-read after expiry stores a fresh entry")
		time.Sleep(assetCacheMaxAge - time.Nanosecond)
		_, ok = cache.get(filename, "image/png", size, modTime)
		assert.True(t, ok, "the fresh entry gets its own seven-day clock")
	})
}

func TestImageRenderCacheHitDoesNotExtendAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := append([]byte(nil), testPNG...)
		filename, size, modTime := cacheAsset(t, "image/png", body)
		cache := newAssetCache()
		require.True(t, cache.put(filename, "image/png", body, size, modTime))

		time.Sleep(assetCacheMaxAge / 2)
		_, ok := cache.get(filename, "image/png", size, modTime)
		require.True(t, ok)

		time.Sleep(assetCacheMaxAge/2 + time.Nanosecond)
		_, ok = cache.get(filename, "image/png", size, modTime)
		assert.False(t, ok, "a hit must not extend the entry lifetime")
	})
}

func TestImageRenderCacheCapacity(t *testing.T) {
	first := append([]byte(nil), testPNG...)
	second := variantPNG(0x81)
	third := variantPNG(0x83)
	firstName, firstSize, modTime := cacheAsset(t, "image/png", first)
	secondName, secondSize, _ := cacheAsset(t, "image/png", second)
	thirdName, thirdSize, _ := cacheAsset(t, "image/png", third)

	entryCache := newAssetCacheWithLimits(
		assetCacheMaxAge, 2, uint64(firstSize*4),
	)
	require.True(t, entryCache.put(firstName, "image/png", first, firstSize, modTime))
	require.True(t, entryCache.put(secondName, "image/png", second, secondSize, modTime))
	require.True(t, entryCache.put(thirdName, "image/png", third, thirdSize, modTime))
	_, firstPresent := entryCache.get(firstName, "image/png", firstSize, modTime)
	assert.False(t, firstPresent, "the entry limit evicts the oldest entry")
	_, secondPresent := entryCache.get(secondName, "image/png", secondSize, modTime)
	assert.True(t, secondPresent)
	_, thirdPresent := entryCache.get(thirdName, "image/png", thirdSize, modTime)
	assert.True(t, thirdPresent)
	assert.Equal(t, 2, entryCache.len())
	assert.Equal(t, uint64(secondSize+thirdSize), entryCache.bytes())

	byteCache := newAssetCacheWithLimits(
		assetCacheMaxAge, 4, uint64(firstSize+secondSize),
	)
	require.True(t, byteCache.put(firstName, "image/png", first, firstSize, modTime))
	require.True(t, byteCache.put(secondName, "image/png", second, secondSize, modTime))
	require.True(t, byteCache.put(thirdName, "image/png", third, thirdSize, modTime))
	_, firstPresent = byteCache.get(firstName, "image/png", firstSize, modTime)
	assert.False(t, firstPresent, "the byte limit evicts the oldest entry")
	_, secondPresent = byteCache.get(secondName, "image/png", secondSize, modTime)
	assert.True(t, secondPresent)
	_, thirdPresent = byteCache.get(thirdName, "image/png", thirdSize, modTime)
	assert.True(t, thirdPresent)
	assert.Equal(t, 2, byteCache.len())
	assert.Equal(t, uint64(firstSize+secondSize), byteCache.bytes())

	oversize := newAssetCacheWithLimits(
		assetCacheMaxAge, 4, uint64(len(first)-1),
	)
	assert.False(t, oversize.put(firstName, "image/png", first, firstSize, modTime))
	assert.Zero(t, oversize.len())
	assert.Zero(t, oversize.bytes())
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

	cache := newAssetCacheWithLimits(assetCacheMaxAge, 1, assetCacheMaxBytes)
	stop := runAssetCache(t, cache)
	got, err := cache.read(filename, filePath, "image/png")
	require.NoError(t, err)
	assert.Equal(t, body, got)

	secondName, secondPath := writeTestAsset(t, dataDir, "image/png", variantPNG(0x83))
	require.NoError(t, os.Chtimes(secondPath, modTime, modTime))
	_, err = cache.read(secondName, secondPath, "image/png")
	require.NoError(t, err)
	assert.Equal(t, 1, cache.len(), "eviction only touches memory")
	stop()

	afterBody, err := os.ReadFile(filePath)
	require.NoError(t, err)
	afterInfo, err := os.Stat(filePath)
	require.NoError(t, err)
	afterUnrelated, err := os.Stat(unrelatedPath)
	require.NoError(t, err)
	assert.Equal(t, beforeBody, afterBody)
	assert.True(t, beforeInfo.ModTime().Equal(afterInfo.ModTime()))
	assert.True(t, beforeUnrelated.ModTime().Equal(afterUnrelated.ModTime()))
	afterUnrelatedBody, err := os.ReadFile(unrelatedPath)
	require.NoError(t, err)
	assert.Equal(t, unrelatedBody, afterUnrelatedBody)
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

	require.NoError(t, os.Remove(filePath))
	assert.Equal(t, http.StatusNotFound, assetErrorStatus(t, srv, filename))

	changed := variantPNG(0x84)
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

	cache := newAssetCacheWithLimits(
		assetCacheMaxAge, assetCacheMaxEntries, uint64(len(body)-1),
	)
	srv := &Server{cfg: config.Config{DataDir: dataDir}, assetCache: cache}
	assert.Equal(t, body, assetResponse(t, srv, canonical).Body)
	assert.Zero(t, cache.len(), "an oversize original remains servable")
	assert.Equal(t, body, assetResponse(t, srv, "legacy.png").Body)
	assert.Zero(t, cache.len(), "legacy filenames bypass admission")

	nilCache := &Server{cfg: config.Config{DataDir: dataDir}}
	assert.Equal(t, body, assetResponse(t, nilCache, canonical).Body)
}

func TestImageRenderCacheLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	body := append([]byte(nil), testPNG...)
	filename, filePath := writeTestAsset(t, dataDir, "image/png", body)
	info, err := os.Stat(filePath)
	require.NoError(t, err)
	srv := New(config.Config{Host: "127.0.0.1", DataDir: dataDir}, dbtest.OpenTestDB(t), nil)
	require.NotNil(t, srv.assetCache)
	assert.Zero(t, srv.assetCache.len())
	cache := newAssetCacheWithLimits(
		20*time.Millisecond, assetCacheMaxEntries, assetCacheMaxBytes,
	)
	cache.sweepInterval = 5 * time.Millisecond
	srv.assetCache = cache
	require.True(t, cache.put(filename, "image/png", body, info.Size(), info.ModTime()))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(listener)
	}()
	serveDoneReceived := false
	shutdown := func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	t.Cleanup(func() {
		if serveDoneReceived {
			return
		}
		_ = shutdown()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			assert.Failf(t, "test failed", "Serve did not stop during cleanup")
		}
	})
	require.Eventually(t, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		return srv.httpSrv != nil
	}, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return cache.len() == 0
	}, time.Second, 5*time.Millisecond, "the server's sweep loop releases the expired entry")
	require.NoError(t, shutdown())
	select {
	case err := <-serveDone:
		serveDoneReceived = true
		require.ErrorIs(t, err, http.ErrServerClosed)
	case <-time.After(time.Second):
		require.FailNow(t, "Serve did not stop after Shutdown")
	}
}

func TestImageRenderCacheExpiryWorkBound(t *testing.T) {
	previousReadAssetFile := readAssetFile
	readAssetFile = func(string) ([]byte, error) {
		return nil, errors.New("expiry must not read durable files")
	}
	t.Cleanup(func() { readAssetFile = previousReadAssetFile })

	for _, residentCount := range []int{1, int(assetCacheMaxEntries)} {
		synctest.Test(t, func(t *testing.T) {
			cache := newAssetCacheWithLimits(
				time.Hour, assetCacheMaxEntries, assetCacheMaxBytes,
			)
			for index := range residentCount {
				body := variantPNG(byte(index + residentCount))
				filename, size, modTime := cacheAsset(t, "image/png", body)
				require.True(t, cache.put(filename, "image/png", body, size, modTime))
			}
			require.Equal(t, residentCount, cache.len())

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				cache.Run(ctx)
				close(done)
			}()
			time.Sleep(time.Hour + cache.sweepInterval)
			synctest.Wait()
			assert.Zero(t, cache.len())
			assert.Zero(t, cache.bytes())
			cancel()
			<-done
		})
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
		body := variantPNG(byte(0x90 + index))
		filename, path := writeTestAsset(t, dataDir, "image/png", body)
		files[index] = assetFile{filename: filename, path: path, body: body}
	}
	cache := newAssetCache()
	stop := runAssetCache(t, cache)
	var wg sync.WaitGroup
	errs := make(chan error, len(files)*8)
	for _, file := range files {
		for range 8 {
			wg.Go(func() {
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
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.Fail(t, fmt.Sprint(err))
	}
	stop()
	assert.Equal(t, len(files), cache.len())
	assert.LessOrEqual(t, cache.bytes(), assetCacheMaxBytes)
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
}
