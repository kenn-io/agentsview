package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireCodecTestOrigin is a valid artifact origin ID used across this file's
// fixtures. It is intentionally not named contractOrigin: that name belongs to
// the shared store contract test suite landing in a later task, and reusing
// it here would collide once that file is added to the package.
const wireCodecTestOrigin = "contract-a1b2c3"

func TestWireRefMappings(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name          string
		kind          Kind
		canonicalName string
		wireName      string
		codec         WireCodec
	}{
		{
			name: "checkpoint", kind: KindCheckpoints,
			canonicalName: "cp-0000000001.json",
			wireName:      "cp-0000000001.json",
			codec:         WireCodecIdentity,
		},
		{
			name: "manifest", kind: KindManifests,
			canonicalName: hash + ".json",
			wireName:      hash + ".json.zst",
			codec:         WireCodecZstd,
		},
		{
			name: "segment", kind: KindSegments,
			canonicalName: hash + ".ndjson",
			wireName:      hash + ".ndjson.zst",
			codec:         WireCodecZstd,
		},
		{
			name: "metadata", kind: KindMeta,
			canonicalName: "20260721T010203.000000000Z-0-" + hash + ".json",
			wireName:      "20260721T010203.000000000Z-0-" + hash + ".json",
			codec:         WireCodecIdentity,
		},
		{
			name: "raw", kind: KindRaw,
			canonicalName: hash,
			wireName:      hash,
			codec:         WireCodecIdentity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical, err := NewRef(wireCodecTestOrigin, tt.kind, tt.canonicalName)
			require.NoError(t, err)

			wire, err := ToWireRef(canonical)
			require.NoError(t, err)
			assert.Equal(t, WireRef{
				Origin: wireCodecTestOrigin,
				Kind:   tt.kind,
				Name:   tt.wireName,
				Codec:  tt.codec,
			}, wire)

			roundTrip, err := FromWireRef(wireCodecTestOrigin, tt.kind, tt.wireName)
			require.NoError(t, err)
			assert.Equal(t, canonical, roundTrip)
		})
	}
}

func TestWireRefRejectsInvalidOrNonWireNames(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		origin string
		kind   Kind
		wire   string
	}{
		{name: "invalid origin", origin: "../peer", kind: KindRaw, wire: hash},
		{name: "unknown kind", origin: wireCodecTestOrigin, kind: "future", wire: hash},
		{name: "manifest missing zstd extension", origin: wireCodecTestOrigin, kind: KindManifests, wire: hash + ".json"},
		{name: "segment missing zstd extension", origin: wireCodecTestOrigin, kind: KindSegments, wire: hash + ".ndjson"},
		{name: "raw gains extension", origin: wireCodecTestOrigin, kind: KindRaw, wire: hash + ".zst"},
		{name: "path separator", origin: wireCodecTestOrigin, kind: KindManifests, wire: "../" + hash + ".json.zst"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromWireRef(tt.origin, tt.kind, tt.wire)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}

	_, err := ToWireRef(Ref{Origin: wireCodecTestOrigin, Kind: KindManifests, Name: hash + ".json.zst"})
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestWireCodecRoundTripsIdentityAndZstd(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		ref  Ref
		body []byte
	}{
		{
			name: "checkpoint",
			ref:  Ref{Origin: wireCodecTestOrigin, Kind: KindCheckpoints, Name: "cp-0000000001.json"},
			body: []byte("canonical checkpoint bytes\n"),
		},
		{
			name: "manifest",
			ref:  Ref{Origin: wireCodecTestOrigin, Kind: KindManifests, Name: hash + ".json"},
			body: []byte("{\"canonical\":\"manifest\"}\n"),
		},
		{
			name: "segment",
			ref:  Ref{Origin: wireCodecTestOrigin, Kind: KindSegments, Name: hash + ".ndjson"},
			body: []byte("{\"canonical\":\"segment\"}\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wireRef, err := ToWireRef(tt.ref)
			require.NoError(t, err)
			var encoded bytes.Buffer
			require.NoError(t, EncodeWire(t.Context(), tt.ref, bytes.NewReader(tt.body), &encoded))

			var decoded bytes.Buffer
			err = DecodeWire(t.Context(), wireRef, bytes.NewReader(encoded.Bytes()), &decoded, WireLimits{
				MaxEncodedBytes: int64(encoded.Len()),
				MaxDecodedBytes: int64(len(tt.body)),
			})
			require.NoError(t, err)
			assert.Equal(t, tt.body, decoded.Bytes())
		})
	}
}

type failingWireWriter struct {
	err error
}

func (w *failingWireWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestWireDecodePreservesDestinationWriteErrors(t *testing.T) {
	hash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		ref  Ref
	}{
		{
			name: "identity",
			ref:  Ref{Origin: wireCodecTestOrigin, Kind: KindCheckpoints, Name: "cp-0000000001.json"},
		},
		{
			name: "zstd",
			ref:  Ref{Origin: wireCodecTestOrigin, Kind: KindManifests, Name: hash + ".json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wireRef, err := ToWireRef(tt.ref)
			require.NoError(t, err)
			var encoded bytes.Buffer
			body := []byte("valid artifact body")
			require.NoError(t, EncodeWire(t.Context(), tt.ref, bytes.NewReader(body), &encoded))

			diskFull := errors.New("disk full")
			err = DecodeWire(t.Context(), wireRef, bytes.NewReader(encoded.Bytes()),
				&failingWireWriter{err: diskFull}, WireLimits{
					MaxEncodedBytes: int64(encoded.Len()),
					MaxDecodedBytes: int64(len(body)),
				})
			require.Error(t, err)
			assert.ErrorIs(t, err, diskFull)
			assert.NotErrorIs(t, err, ErrArtifactCorrupt)
		})
	}
}

func TestWireZstdEncodingIsDeterministic(t *testing.T) {
	ref := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("a", 64) + ".ndjson",
	}
	body := bytes.Repeat([]byte("deterministic canonical record\n"), 1024)
	var first, second bytes.Buffer

	require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &first))
	require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &second))
	assert.Equal(t, first.Bytes(), second.Bytes())
	assert.NotEqual(t, body, first.Bytes())
}

func TestWireDecodeEnforcesEncodedAndDecodedLimits(t *testing.T) {
	rawRef := Ref{Origin: wireCodecTestOrigin, Kind: KindRaw, Name: strings.Repeat("a", 64)}
	rawWire, err := ToWireRef(rawRef)
	require.NoError(t, err)

	t.Run("encoded exact ceiling", func(t *testing.T) {
		var decoded bytes.Buffer
		err := DecodeWire(t.Context(), rawWire, strings.NewReader("12345"), &decoded, WireLimits{
			MaxEncodedBytes: 5,
			MaxDecodedBytes: 5,
		})
		require.NoError(t, err)
		assert.Equal(t, "12345", decoded.String())
	})

	t.Run("encoded ceiling exceeded", func(t *testing.T) {
		var decoded bytes.Buffer
		err := DecodeWire(t.Context(), rawWire, strings.NewReader("123456"), &decoded, WireLimits{
			MaxEncodedBytes: 5,
			MaxDecodedBytes: 100,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrArtifactCorrupt)
		assert.Contains(t, err.Error(), "encoded wire input exceeds 5-byte limit")
	})

	t.Run("decoded ceiling exceeded", func(t *testing.T) {
		ref := Ref{
			Origin: wireCodecTestOrigin,
			Kind:   KindSegments,
			Name:   strings.Repeat("b", 64) + ".ndjson",
		}
		wireRef, err := ToWireRef(ref)
		require.NoError(t, err)
		body := bytes.Repeat([]byte("x"), 1024)
		var encoded bytes.Buffer
		require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &encoded))

		var decoded bytes.Buffer
		err = DecodeWire(t.Context(), wireRef, bytes.NewReader(encoded.Bytes()), &decoded, WireLimits{
			MaxEncodedBytes: int64(encoded.Len()),
			MaxDecodedBytes: int64(len(body) - 1),
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrArtifactCorrupt)
		assert.Contains(t, err.Error(), "decoded canonical output exceeds 1023-byte limit")
	})
}

func TestWireDecodeRejectsLargeZstdWindow(t *testing.T) {
	body := bytes.Repeat([]byte("windowed-record\n"), 700_000)
	var encoded bytes.Buffer
	enc, err := zstd.NewWriter(
		&encoded,
		zstd.WithWindowSize(16<<20),
		zstd.WithEncoderConcurrency(1),
	)
	require.NoError(t, err)
	_, err = enc.Write(body)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	wireRef := requireWireRef(t, Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("a", 64) + ".ndjson",
	})

	err = DecodeWire(t.Context(), wireRef, bytes.NewReader(encoded.Bytes()), io.Discard, WireLimits{
		MaxEncodedBytes: int64(encoded.Len()),
		MaxDecodedBytes: int64(len(body)),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactCorrupt)
	assert.Contains(t, err.Error(), "window size exceeded")
}

func TestWireDecodeRejectsCorruptAndTruncatedZstd(t *testing.T) {
	ref := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindManifests,
		Name:   strings.Repeat("a", 64) + ".json",
	}
	wireRef := requireWireRef(t, ref)
	body := bytes.Repeat([]byte("canonical manifest bytes\n"), 128)
	var valid bytes.Buffer
	require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &valid))
	require.Greater(t, valid.Len(), 4)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "corrupt", data: []byte("this is not a zstd frame")},
		{name: "truncated", data: append([]byte(nil), valid.Bytes()[:valid.Len()-1]...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeWire(t.Context(), wireRef, bytes.NewReader(tt.data), io.Discard, WireLimits{
				MaxEncodedBytes: int64(len(tt.data)),
				MaxDecodedBytes: int64(len(body)),
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactCorrupt)
		})
	}
}

func TestWireDecodeRejectsNonPositiveLimits(t *testing.T) {
	wireRef := requireWireRef(t, Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindRaw,
		Name:   strings.Repeat("a", 64),
	})
	tests := []struct {
		name   string
		limits WireLimits
	}{
		{
			name: "zero encoded",
			limits: WireLimits{
				MaxEncodedBytes: 0,
				MaxDecodedBytes: 1,
			},
		},
		{
			name: "negative encoded",
			limits: WireLimits{
				MaxEncodedBytes: -1,
				MaxDecodedBytes: 1,
			},
		},
		{
			name: "zero decoded",
			limits: WireLimits{
				MaxEncodedBytes: 1,
				MaxDecodedBytes: 0,
			},
		},
		{
			name: "negative decoded",
			limits: WireLimits{
				MaxEncodedBytes: 1,
				MaxDecodedBytes: -1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeWire(
				t.Context(), wireRef, strings.NewReader("x"), io.Discard, tt.limits,
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
			assert.Contains(t, err.Error(), "wire limits must be positive")
		})
	}
}

func TestWireCodecHonorsCancellation(t *testing.T) {
	ref := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("a", 64) + ".ndjson",
	}

	t.Run("encode identity", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		src := &cancelAfterReader{
			cancel:    cancel,
			remaining: 1 << 20,
			perRead:   1024,
		}
		rawRef := Ref{
			Origin: wireCodecTestOrigin,
			Kind:   KindRaw,
			Name:   strings.Repeat("b", 64),
		}
		err := EncodeWire(ctx, rawRef, src, io.Discard)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, src.read, int64(1<<20))
	})

	t.Run("encode identity canceled by final read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		src := &cancelOnFinalRead{
			cancel: cancel,
			body:   []byte("complete but canceled input"),
		}
		rawRef := Ref{
			Origin: wireCodecTestOrigin,
			Kind:   KindRaw,
			Name:   strings.Repeat("c", 64),
		}
		err := EncodeWire(ctx, rawRef, src, io.Discard)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("encode", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		src := &cancelAfterReader{
			cancel:    cancel,
			remaining: 1 << 20,
			perRead:   1024,
		}
		err := EncodeWire(ctx, ref, src, io.Discard)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, src.read, int64(1<<20))
	})

	t.Run("decode", func(t *testing.T) {
		body := bytes.Repeat([]byte("incompressible-ish-0123456789abcdef\n"), 4096)
		var encoded bytes.Buffer
		require.NoError(t, EncodeWire(t.Context(), ref, bytes.NewReader(body), &encoded))
		ctx, cancel := context.WithCancel(t.Context())
		src := &cancelAfterReader{
			reader:    bytes.NewReader(encoded.Bytes()),
			cancel:    cancel,
			perRead:   1,
			remaining: int64(encoded.Len()),
		}
		err := DecodeWire(ctx, requireWireRef(t, ref), src, io.Discard, WireLimits{
			MaxEncodedBytes: int64(encoded.Len()),
			MaxDecodedBytes: int64(len(body)),
		})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, src.read, int64(encoded.Len()))
	})
}

func TestWireCodecDetectsCancellationDuringFinalSuccessfulWrite(t *testing.T) {
	body := []byte("one final successful destination write")
	rawRef := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindRaw,
		Name:   strings.Repeat("a", 64),
	}
	zstdRef := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("b", 64) + ".ndjson",
	}
	var encoded bytes.Buffer
	require.NoError(t, EncodeWire(t.Context(), zstdRef, bytes.NewReader(body), &encoded))
	zstdWire := requireWireRef(t, zstdRef)
	rawWire := requireWireRef(t, rawRef)

	tests := []struct {
		name string
		run  func(context.Context, io.Writer) error
	}{
		{
			name: "encode identity",
			run: func(ctx context.Context, dst io.Writer) error {
				return EncodeWire(ctx, rawRef, &singleReadEOF{body: body}, dst)
			},
		},
		{
			name: "encode zstd",
			run: func(ctx context.Context, dst io.Writer) error {
				return EncodeWire(ctx, zstdRef, &singleReadEOF{body: body}, dst)
			},
		},
		{
			name: "decode identity",
			run: func(ctx context.Context, dst io.Writer) error {
				return DecodeWire(ctx, rawWire, &singleReadEOF{body: body}, dst, WireLimits{
					MaxEncodedBytes: int64(len(body)),
					MaxDecodedBytes: int64(len(body)),
				})
			},
		},
		{
			name: "decode zstd",
			run: func(ctx context.Context, dst io.Writer) error {
				return DecodeWire(ctx, zstdWire, bytes.NewReader(encoded.Bytes()), dst, WireLimits{
					MaxEncodedBytes: int64(encoded.Len()),
					MaxDecodedBytes: int64(len(body)),
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := &cancelOnSuccessfulWrite{writer: io.Discard}
			require.NoError(t, tt.run(t.Context(), count))
			require.Positive(t, count.writes)

			ctx, cancel := context.WithCancel(t.Context())
			dst := &cancelOnSuccessfulWrite{
				writer:   io.Discard,
				cancel:   cancel,
				cancelOn: count.writes,
			}
			err := tt.run(ctx, dst)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, count.writes, dst.writes,
				"cancellation must occur during the operation's final full write")
		})
	}
}

func TestWireCodecStreamsMultiMegabyteArtifactsWithBoundedBuffers(t *testing.T) {
	const size = int64(6 << 20)
	ref := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("a", 64) + ".ndjson",
	}
	canonicalHash := sha256.New()
	src := &boundedPatternReader{
		remaining:  size,
		maxRequest: 128 << 10,
	}
	encoded, err := os.CreateTemp(t.TempDir(), "wire-*.zst")
	require.NoError(t, err)
	t.Cleanup(func() { _ = encoded.Close() })

	err = EncodeWire(t.Context(), ref, io.TeeReader(src, canonicalHash), encoded)
	require.NoError(t, err)
	assert.LessOrEqual(t, src.largestRequest, 128<<10)
	encodedSize, err := encoded.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Positive(t, encodedSize)
	_, err = encoded.Seek(0, io.SeekStart)
	require.NoError(t, err)

	decodedHash := sha256.New()
	dst := &boundedWriteObserver{writer: decodedHash, maxWrite: 128 << 10}
	err = DecodeWire(t.Context(), requireWireRef(t, ref), encoded, dst, WireLimits{
		MaxEncodedBytes: encodedSize,
		MaxDecodedBytes: size,
	})
	require.NoError(t, err)
	assert.Equal(t, size, dst.written)
	assert.LessOrEqual(t, dst.largestWrite, 128<<10)
	assert.Equal(t, canonicalHash.Sum(nil), decodedHash.Sum(nil))
}

func TestWireCodecAllocatedBytesStayBoundedAsArtifactsGrow(t *testing.T) {
	const (
		smallSize = int64(32 << 10)
		largeSize = int64(12 << 20)
		// Identity has no codec window, so growth above this allowance is an
		// artifact-sized buffer rather than fixed streaming overhead.
		identityMaxAllocationGrowth = int64(1 << 20)
		// The zstd encoder may grow to two fixed 8 MiB working windows plus
		// bookkeeping. Deterministically incompressible input makes either a
		// canonical or compressed-wire artifact buffer cross this allowance.
		zstdMaxAllocationGrowth = int64(20 << 20)
	)
	rawRef := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindRaw,
		Name:   strings.Repeat("a", 64),
	}
	zstdRef := Ref{
		Origin: wireCodecTestOrigin,
		Kind:   KindSegments,
		Name:   strings.Repeat("b", 64) + ".ndjson",
	}
	rawWire := requireWireRef(t, rawRef)
	zstdWire := requireWireRef(t, zstdRef)

	tests := []struct {
		name      string
		maxGrowth int64
		factory   func(t *testing.T, size int64) func() error
	}{
		{
			name:      "encode identity",
			maxGrowth: identityMaxAllocationGrowth,
			factory: func(_ *testing.T, size int64) func() error {
				return func() error {
					return EncodeWire(context.Background(), rawRef, newWireBenchmarkReader(size), io.Discard)
				}
			},
		},
		{
			name:      "encode zstd",
			maxGrowth: zstdMaxAllocationGrowth,
			factory: func(t *testing.T, size int64) func() error {
				requireIncompressibleWireFixture(t, zstdRef, size)
				return func() error {
					return EncodeWire(context.Background(), zstdRef, newWireBenchmarkReader(size), io.Discard)
				}
			},
		},
		{
			name:      "decode identity",
			maxGrowth: identityMaxAllocationGrowth,
			factory: func(_ *testing.T, size int64) func() error {
				return func() error {
					return DecodeWire(
						context.Background(), rawWire, newWireBenchmarkReader(size), io.Discard,
						WireLimits{MaxEncodedBytes: size, MaxDecodedBytes: size},
					)
				}
			},
		},
		{
			name:      "decode zstd",
			maxGrowth: zstdMaxAllocationGrowth,
			factory: func(t *testing.T, size int64) func() error {
				encoded := requireIncompressibleWireFixture(t, zstdRef, size)
				return func() error {
					return DecodeWire(
						context.Background(), zstdWire, bytes.NewReader(encoded), io.Discard,
						WireLimits{
							MaxEncodedBytes: int64(len(encoded)),
							MaxDecodedBytes: size,
						},
					)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			small := wireCodecAllocatedBytes(t, tt.factory(t, smallSize))
			large := wireCodecAllocatedBytes(t, tt.factory(t, largeSize))
			t.Logf("allocated bytes/op: small=%d large=%d max_growth=%d",
				small, large, tt.maxGrowth)
			assert.LessOrEqual(t, large, small+tt.maxGrowth,
				"allocation bytes must stay bounded as the artifact grows from %d to %d bytes; small=%d large=%d max_growth=%d",
				smallSize, largeSize, small, large, tt.maxGrowth)
		})
	}
}

func requireWireRef(t *testing.T, ref Ref) WireRef {
	t.Helper()
	wireRef, err := ToWireRef(ref)
	require.NoError(t, err)
	return wireRef
}

type cancelAfterReader struct {
	reader    io.Reader
	cancel    context.CancelFunc
	remaining int64
	perRead   int
	read      int64
	canceled  bool
}

type cancelOnFinalRead struct {
	cancel context.CancelFunc
	body   []byte
}

type singleReadEOF struct {
	body []byte
}

func (r *singleReadEOF) Read(p []byte) (int, error) {
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	if len(r.body) == 0 {
		return n, io.EOF
	}
	return n, nil
}

type cancelOnSuccessfulWrite struct {
	writer   io.Writer
	cancel   context.CancelFunc
	cancelOn int
	writes   int
}

func (w *cancelOnSuccessfulWrite) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n == len(p) {
		w.writes++
		if w.cancel != nil && w.writes == w.cancelOn {
			w.cancel()
		}
	}
	return n, err
}

func (r *cancelOnFinalRead) Read(p []byte) (int, error) {
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	if len(r.body) == 0 {
		r.cancel()
		return n, io.EOF
	}
	return n, nil
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) > r.perRead {
		p = p[:r.perRead]
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	var n int
	var err error
	if r.reader != nil {
		n, err = r.reader.Read(p)
	} else {
		for i := range p {
			p[i] = byte(i)
		}
		n = len(p)
	}
	r.remaining -= int64(n)
	r.read += int64(n)
	if !r.canceled && n > 0 {
		r.canceled = true
		r.cancel()
	}
	return n, err
}

type boundedPatternReader struct {
	remaining      int64
	offset         int64
	maxRequest     int
	largestRequest int
}

type deterministicNoiseReader struct {
	remaining int64
	state     uint64
}

func newWireBenchmarkReader(size int64) *deterministicNoiseReader {
	return &deterministicNoiseReader{
		remaining: size,
		state:     0x9e3779b97f4a7c15,
	}
}

func (r *deterministicNoiseReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	state := r.state
	for i := range p {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		p[i] = byte(state >> 56)
	}
	r.state = state
	r.remaining -= int64(len(p))
	return len(p), nil
}

func requireIncompressibleWireFixture(t *testing.T, ref Ref, size int64) []byte {
	t.Helper()
	var wire bytes.Buffer
	require.NoError(t, EncodeWire(
		t.Context(), ref, newWireBenchmarkReader(size), &wire,
	))
	encoded := append([]byte(nil), wire.Bytes()...)
	require.Greater(t, int64(len(encoded)), size*9/10,
		"fixture wire bytes must remain proportional to canonical size")
	return encoded
}

func wireCodecAllocatedBytes(t *testing.T, run func() error) int64 {
	t.Helper()
	require.NoError(t, run())
	var runErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := run(); err != nil {
				runErr = err
				b.StopTimer()
				return
			}
		}
	})
	require.NoError(t, runErr)
	return result.AllocedBytesPerOp()
}

func (r *boundedPatternReader) Read(p []byte) (int, error) {
	if len(p) > r.largestRequest {
		r.largestRequest = len(p)
	}
	if len(p) > r.maxRequest {
		return 0, fmt.Errorf("artifact-sized read buffer: %d", len(p))
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = byte((r.offset + int64(i)*31) % 251)
	}
	n := len(p)
	r.offset += int64(n)
	r.remaining -= int64(n)
	return n, nil
}

type boundedWriteObserver struct {
	writer       io.Writer
	maxWrite     int
	largestWrite int
	written      int64
}

func (w *boundedWriteObserver) Write(p []byte) (int, error) {
	if len(p) > w.largestWrite {
		w.largestWrite = len(p)
	}
	if len(p) > w.maxWrite {
		return 0, fmt.Errorf("artifact-sized write buffer: %d", len(p))
	}
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
}
