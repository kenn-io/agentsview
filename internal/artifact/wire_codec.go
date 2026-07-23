package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	manifestExtension = ".json.zst"
	segmentExtension  = ".ndjson.zst"

	// zstd.NewWriter documents an 8 MiB maximum default window. Pinning that
	// size keeps existing package-written artifacts readable without accepting
	// attacker-selected large decoder windows.
	zstdMaxWindowSize = uint64(8 << 20)

	wireCopyBufferSize = 32 << 10
)

// WireCodec identifies the transport encoding for an artifact kind.
type WireCodec string

const (
	WireCodecIdentity WireCodec = "identity"
	WireCodecZstd     WireCodec = "zstd"
)

// WireRef is a validated external protocol reference. Name may include a
// transport compression extension and is never a filesystem path.
type WireRef struct {
	Origin string
	Kind   Kind
	Name   string
	Codec  WireCodec
}

// WireLimits bounds the encoded bytes consumed and canonical bytes emitted by
// DecodeWire. Both limits must be positive.
type WireLimits struct {
	MaxEncodedBytes int64
	MaxDecodedBytes int64
}

var wireCopyBufferPool = sync.Pool{
	New: func() any {
		buffer := new([wireCopyBufferSize]byte)
		return buffer
	},
}

// ToWireRef maps one canonical logical reference to its external wire name.
func ToWireRef(ref Ref) (WireRef, error) {
	canonical, err := NewRef(ref.Origin, ref.Kind, ref.Name)
	if err != nil {
		return WireRef{}, err
	}
	wire := WireRef{
		Origin: canonical.Origin,
		Kind:   canonical.Kind,
		Name:   canonical.Name,
		Codec:  WireCodecIdentity,
	}
	switch canonical.Kind {
	case KindManifests, KindSegments:
		wire.Name += ".zst"
		wire.Codec = WireCodecZstd
	case KindCheckpoints, KindMeta, KindRaw:
	default:
		return WireRef{}, fmt.Errorf(
			"%w: unsupported artifact kind %q", ErrArtifactInvalid, canonical.Kind,
		)
	}
	return wire, nil
}

// FromWireRef maps one external protocol name to its canonical logical
// reference. It only removes a transport extension and never joins paths.
func FromWireRef(origin string, kind Kind, name string) (Ref, error) {
	if err := validateOriginID(origin); err != nil {
		return Ref{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateArtifactName(name); err != nil {
		return Ref{}, err
	}
	canonicalName := name
	switch kind {
	case KindManifests:
		if !strings.HasSuffix(name, manifestExtension) {
			return Ref{}, fmt.Errorf(
				"%w: manifest wire name must end in %s", ErrArtifactInvalid, manifestExtension,
			)
		}
		canonicalName = strings.TrimSuffix(name, ".zst")
	case KindSegments:
		if !strings.HasSuffix(name, segmentExtension) {
			return Ref{}, fmt.Errorf(
				"%w: segment wire name must end in %s", ErrArtifactInvalid, segmentExtension,
			)
		}
		canonicalName = strings.TrimSuffix(name, ".zst")
	case KindCheckpoints, KindMeta, KindRaw:
	default:
		return Ref{}, fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, kind)
	}
	return NewRef(origin, kind, canonicalName)
}

// EncodeWire streams canonical artifact bytes to their external wire encoding.
func EncodeWire(ctx context.Context, ref Ref, src io.Reader, dst io.Writer) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrArtifactInvalid)
	}
	if src == nil {
		return fmt.Errorf("%w: canonical source is required", ErrArtifactInvalid)
	}
	if dst == nil {
		return fmt.Errorf("%w: wire destination is required", ErrArtifactInvalid)
	}
	wireRef, err := ToWireRef(ref)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch wireRef.Codec {
	case WireCodecIdentity:
		return copyWireStream(ctx, dst, src)
	case WireCodecZstd:
		return encodeWireZstd(ctx, src, dst)
	default:
		return fmt.Errorf("%w: unsupported wire codec %q", ErrArtifactInvalid, wireRef.Codec)
	}
}

// DecodeWire streams an external wire object to canonical bytes while
// enforcing independent encoded and decoded size ceilings.
func DecodeWire(
	ctx context.Context,
	wireRef WireRef,
	src io.Reader,
	dst io.Writer,
	limits WireLimits,
) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", ErrArtifactInvalid)
	}
	if src == nil {
		return fmt.Errorf("%w: wire source is required", ErrArtifactInvalid)
	}
	if dst == nil {
		return fmt.Errorf("%w: canonical destination is required", ErrArtifactInvalid)
	}
	if limits.MaxEncodedBytes <= 0 || limits.MaxDecodedBytes <= 0 {
		return fmt.Errorf("%w: wire limits must be positive", ErrArtifactInvalid)
	}
	if _, err := canonicalRefForWire(wireRef); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	encoded := &wireLimitedReader{
		reader: &wireContextReader{ctx: ctx, reader: src},
		limit:  limits.MaxEncodedBytes,
		label:  "encoded wire input",
	}
	decoded := &wireLimitedWriter{
		writer: &wireContextWriter{ctx: ctx, writer: dst},
		limit:  limits.MaxDecodedBytes,
		label:  "decoded canonical output",
	}
	var err error
	switch wireRef.Codec {
	case WireCodecIdentity:
		err = copyWireStream(ctx, decoded, encoded)
	case WireCodecZstd:
		err = decodeWireZstd(ctx, encoded, decoded, limits.MaxDecodedBytes)
	default:
		err = fmt.Errorf("unsupported wire codec %q", wireRef.Codec)
	}
	if err == nil {
		err = encoded.requireEOF()
	}
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%w: decoding %s: %w", ErrArtifactCorrupt, wireRef.Name, err)
}

func canonicalRefForWire(wireRef WireRef) (Ref, error) {
	ref, err := FromWireRef(wireRef.Origin, wireRef.Kind, wireRef.Name)
	if err != nil {
		return Ref{}, err
	}
	want, err := ToWireRef(ref)
	if err != nil {
		return Ref{}, err
	}
	if wireRef != want {
		return Ref{}, fmt.Errorf(
			"%w: wire codec %q does not match %s artifacts",
			ErrArtifactInvalid, wireRef.Codec, wireRef.Kind,
		)
	}
	return ref, nil
}

func encodeWireZstd(ctx context.Context, src io.Reader, dst io.Writer) error {
	writer, err := zstd.NewWriter(
		&wireContextWriter{ctx: ctx, writer: dst},
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(int(zstdMaxWindowSize)),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return err
	}
	copyErr := copyWireStream(ctx, writer, src)
	closeErr := writer.Close()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func decodeWireZstd(
	ctx context.Context,
	src io.Reader,
	dst io.Writer,
	maxDecoded int64,
) error {
	maxMemory := max(uint64(maxDecoded), zstdMaxWindowSize)
	reader, err := zstd.NewReader(
		src,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxMemory),
		zstd.WithDecoderMaxWindow(zstdMaxWindowSize),
	)
	if err != nil {
		return err
	}
	defer reader.Close()
	return copyWireStream(ctx, dst, struct{ io.Reader }{Reader: reader})
}

func copyWireStream(ctx context.Context, dst io.Writer, src io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pooled := wireCopyBufferPool.Get().(*[wireCopyBufferSize]byte)
	defer wireCopyBufferPool.Put(pooled)
	_, err := io.CopyBuffer(
		&wireContextWriter{ctx: ctx, writer: dst},
		&wireContextReader{ctx: ctx, reader: src},
		pooled[:],
	)
	if err != nil {
		return err
	}
	// Cancellation is cooperative at I/O boundaries: the wrappers check before
	// every underlying Read and Write, and this final check catches cancellation
	// during the last successful call. An arbitrary Reader or Writer that is
	// already blocked must provide its own context-aware unblocking mechanism;
	// this codec does not spawn per-I/O goroutines that could leak behind it.
	return ctx.Err()
}

type wireContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *wireContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type wireContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w *wireContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

type wireLimitedReader struct {
	reader io.Reader
	limit  int64
	read   int64
	label  string
}

func (r *wireLimitedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.limit - r.read
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%s exceeds %d-byte limit", r.label, r.limit)
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (r *wireLimitedReader) requireEOF() error {
	var probe [1]byte
	n, err := r.Read(probe[:])
	if n > 0 {
		return fmt.Errorf("%s exceeds %d-byte limit", r.label, r.limit)
	}
	if err == nil {
		return io.ErrNoProgress
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type wireLimitedWriter struct {
	writer io.Writer
	limit  int64
	wrote  int64
	label  string
}

func (w *wireLimitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.wrote
	if remaining <= 0 && len(p) > 0 {
		return 0, fmt.Errorf("%s exceeds %d-byte limit", w.label, w.limit)
	}
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
		n, err := w.writer.Write(p)
		w.wrote += int64(n)
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("%s exceeds %d-byte limit", w.label, w.limit)
	}
	n, err := w.writer.Write(p)
	w.wrote += int64(n)
	return n, err
}
