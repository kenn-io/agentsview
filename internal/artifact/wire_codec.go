package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	manifestExtension = ".json.zst"
	segmentExtension  = ".ndjson.zst"

	manifestDecodedLimit = int64(16 << 20)
	segmentDecodedLimit  = int64(64 << 20)

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

// PeerWireLimits returns the protocol size limits for one peer artifact kind.
func PeerWireLimits(kind Kind) WireLimits {
	return transportWireLimits(kind)
}

// CanonicalWireSpool owns one private temporary file containing a decoded,
// identity-checked, and semantically validated artifact. Call Close when the
// canonical bytes have been created or repaired in a store.
type CanonicalWireSpool struct {
	ref      Ref
	identity Identity
	file     *os.File
	path     string
}

// Ref returns the canonical logical reference decoded into the spool.
func (s *CanonicalWireSpool) Ref() Ref { return s.ref }

// Identity returns the SHA-256 and size of the canonical decoded bytes.
func (s *CanonicalWireSpool) Identity() Identity { return s.identity }

// Rewind positions the canonical stream at its beginning for one store
// operation. Callers must serialize uses of a spool.
func (s *CanonicalWireSpool) Rewind() (io.Reader, error) {
	if s == nil || s.file == nil {
		return nil, fmt.Errorf("%w: canonical artifact spool is closed", ErrArtifactInvalid)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewinding canonical artifact spool: %w", err)
	}
	return s.file, nil
}

// Create performs immutable creation from the already validated canonical
// bytes without decoding the peer stream a second time.
func (s *CanonicalWireSpool) Create(
	ctx context.Context, store ArtifactStore,
) (CreateResult, error) {
	if store == nil {
		return CreateResult{}, fmt.Errorf("%w: artifact store is required", ErrArtifactInvalid)
	}
	reader, err := s.Rewind()
	if err != nil {
		return CreateResult{}, err
	}
	return store.Create(ctx, s.ref, s.identity,
		canonicalArtifactMediaType(s.ref.Kind), reader)
}

// Close closes and removes the private canonical spool.
func (s *CanonicalWireSpool) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	return errors.Join(file.Close(), removeCanonicalWireSpool(s.path))
}

func removeCanonicalWireSpool(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing canonical artifact spool: %w", err)
	}
	return nil
}

// DecodeWireToCanonicalSpool decodes one inbound wire object exactly once,
// validates its protocol identity and bounded semantics, and returns a private
// canonical stream suitable for immutable creation or an authorized repair.
func DecodeWireToCanonicalSpool(
	ctx context.Context,
	wireRef WireRef,
	src io.Reader,
	limits WireLimits,
) (_ *CanonicalWireSpool, retErr error) {
	ref, err := canonicalRefForWire(wireRef)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "agentsview-artifact-spool-*")
	if err != nil {
		return nil, fmt.Errorf("creating canonical artifact spool: %w", err)
	}
	spool := &CanonicalWireSpool{ref: ref, file: file, path: file.Name()}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, spool.Close())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("securing canonical artifact spool: %w", err)
	}

	hasher := sha256.New()
	canonical := &wireSpoolWriter{file: file, hash: hasher}
	if err := DecodeWire(ctx, wireRef, src, canonical, limits); err != nil {
		return nil, err
	}
	identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), canonical.size)
	if err != nil {
		return nil, err
	}
	if err := validateRefIdentity(ref, identity); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCanonicalArtifactSpool(ctx, ref, file, limits.MaxDecodedBytes); err != nil {
		return nil, err
	}
	spool.identity = identity
	return spool, nil
}

// CreateFromWire decodes one inbound wire object to a private canonical spool,
// validates its content identity and bounded semantics, and creates it in the
// store through CreateImmutable.
func CreateFromWire(
	ctx context.Context,
	store ArtifactStore,
	wireRef WireRef,
	src io.Reader,
	limits WireLimits,
) (result CreateResult, retErr error) {
	if store == nil {
		return CreateResult{}, fmt.Errorf("%w: artifact store is required", ErrArtifactInvalid)
	}
	spool, err := DecodeWireToCanonicalSpool(ctx, wireRef, src, limits)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, spool.Close())
	}()
	return spool.Create(ctx, store)
}

func validateCanonicalArtifactSpool(
	ctx context.Context, ref Ref, spool *os.File, limit int64,
) error {
	if ref.Kind == KindRaw {
		return ctx.Err()
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(
		&wireContextReader{ctx: ctx, reader: spool}, limit+1,
	))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: decoded artifact exceeds limit", ErrArtifactInvalid)
	}
	switch ref.Kind {
	case KindCheckpoints:
		return validateCheckpointData(data, ref.Origin, ref.Name)
	case KindManifests:
		return validateCanonicalManifestArtifactData(data, ref.Origin)
	case KindSegments:
		return validateCanonicalSegmentArtifactData(data)
	case KindMeta:
		_, hash, err := normalizeMetadataName(ref.Name)
		if err != nil {
			return err
		}
		return validateMetadataArtifactData(data, ref.Origin, ref.Name, hash)
	default:
		return fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, ref.Kind)
	}
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

type wireSpoolWriter struct {
	file *os.File
	hash hash.Hash
	size int64
}

func (w *wireSpoolWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.size += int64(n)
	}
	return n, err
}
