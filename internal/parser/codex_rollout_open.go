package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/ctxio"
)

// Codex compresses a rollout whose file has not changed for a week to
// rollout-<ts>-<uuid>.jsonl.zst and removes the plain file, keeping the
// file's modification time (codex-rs/rollout/src/compression.rs). Codex reads
// prefer the plain file while both exist, and a resumed thread is unpacked
// back to the plain file in place before Codex appends to it. The compressed
// file is a zstd stream of the plain file's bytes, so it is always parsed in
// full: it offers no byte offsets to resume from, and no checkpoint or cursor
// is taken into it.
const (
	codexRolloutSuffix           = ".jsonl"
	codexCompressedRolloutSuffix = ".jsonl.zst"

	// codexRolloutDecoderMaxWindow bounds the zstd window a rollout may ask
	// for. Codex encodes at level 3, whose window is far smaller; the bound
	// only keeps a corrupt header from reserving memory.
	codexRolloutDecoderMaxWindow = 64 << 20
	codexRolloutDecoderMemory    = 128 << 20
)

// hasCodexRolloutSuffix reports whether path names a rollout in either of
// the forms Codex writes.
func hasCodexRolloutSuffix(path string) bool {
	return strings.HasSuffix(path, codexRolloutSuffix) ||
		strings.HasSuffix(path, codexCompressedRolloutSuffix)
}

// isCompressedCodexRolloutPath reports whether path is the compressed form.
func isCompressedCodexRolloutPath(path string) bool {
	return strings.HasSuffix(path, codexCompressedRolloutSuffix)
}

// codexPlainRolloutPath returns the plain .jsonl path or name of a rollout
// given either of its forms.
func codexPlainRolloutPath(path string) string {
	return codexRolloutStem(path) + codexRolloutSuffix
}

// codexRolloutStem strips the plain or compressed rollout suffix from a
// filename, so both forms of one rollout share a stem.
func codexRolloutStem(name string) string {
	name = strings.TrimSuffix(name, codexCompressedRolloutSuffix)
	return strings.TrimSuffix(name, codexRolloutSuffix)
}

// codexRolloutSnapshot is one Codex rollout opened for a full read.
type codexRolloutSnapshot struct {
	// File holds the bytes to parse: the rollout itself, or for a
	// compressed rollout a temporary plain copy that Close removes.
	File *os.File
	// Info describes File; its size bounds the read.
	Info os.FileInfo
	// Source describes the rollout on disk: the identity, size and
	// modification time the session records, which are what Fingerprint
	// stats on later passes.
	Source os.FileInfo
	// Compressed marks a snapshot unpacked from a .jsonl.zst file. The
	// stored session cannot resume into it by byte offset, so callers take
	// no checkpoint and no cursor from such a snapshot.
	Compressed bool
	// Hash is the content hash of the compressed file's own bytes, the
	// value Fingerprint computes for the path, so the session records it
	// instead of a hash of the unpacked bytes. Empty for a plain rollout.
	Hash     string
	tempPath string
}

// plainCodexRolloutSnapshot wraps an already open plain rollout.
func plainCodexRolloutSnapshot(f *os.File, info os.FileInfo) codexRolloutSnapshot {
	return codexRolloutSnapshot{File: f, Info: info, Source: info}
}

// openCodexRollout opens a rollout for a full read, unpacking a compressed
// one to a temporary file first. A compressed file that does not decode in
// full is a read failure of that rollout; nothing partial is returned.
func openCodexRollout(ctx context.Context, path string) (*codexRolloutSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !isCompressedCodexRolloutPath(path) {
		return &codexRolloutSnapshot{File: f, Info: info, Source: info}, nil
	}
	defer f.Close()
	temp, err := os.CreateTemp("", "agentsview-codex-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", path, err)
	}
	snapshot := &codexRolloutSnapshot{
		File: temp, Source: info, Compressed: true, tempPath: temp.Name(),
	}
	hash := sha256.New()
	if err := decodeCodexRollout(ctx, io.TeeReader(f, hash), temp); err != nil {
		snapshot.Close()
		return nil, fmt.Errorf("unpack %s: %w", path, err)
	}
	// The hash must cover the whole file, as Fingerprint's does, whatever
	// the decoder left unread after the last frame.
	if _, err := io.Copy(hash, ctxio.Reader{Context: ctx, Reader: f}); err != nil {
		snapshot.Close()
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	snapshot.Hash = hex.EncodeToString(hash.Sum(nil))
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		snapshot.Close()
		return nil, fmt.Errorf("unpack %s: %w", path, err)
	}
	if snapshot.Info, err = temp.Stat(); err != nil {
		snapshot.Close()
		return nil, fmt.Errorf("unpack %s: %w", path, err)
	}
	return snapshot, nil
}

// Close releases the open file and removes the temporary copy of a
// compressed rollout.
func (s *codexRolloutSnapshot) Close() error {
	if s == nil || s.File == nil {
		return nil
	}
	err := s.File.Close()
	s.File = nil
	if s.tempPath != "" {
		if removeErr := os.Remove(s.tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
		s.tempPath = ""
	}
	return err
}

// identity returns the on-disk rollout's file identity.
func (s *codexRolloutSnapshot) identity(path string) (inode, device uint64) {
	if s.Compressed {
		return sourceFileIdentityForPath(path, s.Source)
	}
	return sourceFileIdentityForFile(s.File, s.Source)
}

// changeTime returns the on-disk rollout's inode change time.
func (s *codexRolloutSnapshot) changeTime(path string) (int64, bool) {
	if s.Compressed {
		return codexIndexChangeTime(path, s.Source)
	}
	return codexIndexChangeTimeForFile(s.File, s.Source)
}

// newCodexRolloutDecoder returns a sequential, memory-bounded zstd decoder
// over a compressed rollout.
func newCodexRolloutDecoder(src io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(
		src,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(codexRolloutDecoderMemory),
		zstd.WithDecoderMaxWindow(codexRolloutDecoderMaxWindow),
	)
}

// codexRolloutPlainSize reports the bytes a full parse of path reads: the
// file's size for a plain rollout, and for a compressed one the unpacked
// size its frame header declares, falling back to the compressed size when
// the header carries none or cannot be read.
func codexRolloutPlainSize(path string, info os.FileInfo) int64 {
	if !isCompressedCodexRolloutPath(path) {
		return info.Size()
	}
	f, err := os.Open(path)
	if err != nil {
		return info.Size()
	}
	defer f.Close()
	var head [zstd.HeaderMaxSize]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return info.Size()
	}
	var header zstd.Header
	if err := header.Decode(head[:n]); err != nil || !header.HasFCS {
		return info.Size()
	}
	if header.FrameContentSize > uint64(1<<62) {
		return info.Size()
	}
	return int64(header.FrameContentSize)
}

// decodeCodexRollout writes the plain bytes of a compressed rollout to dst.
// The zstd stream must decode to its end; a truncated or corrupt file fails.
func decodeCodexRollout(ctx context.Context, src io.Reader, dst io.Writer) error {
	decoder, err := newCodexRolloutDecoder(src)
	if err != nil {
		return err
	}
	defer decoder.Close()
	if _, err := io.Copy(dst, ctxio.Reader{Context: ctx, Reader: decoder}); err != nil {
		return err
	}
	return nil
}

// openCodexRolloutLines opens a rollout for a forward line scan, plain or
// compressed, without a temporary copy.
func openCodexRolloutLines(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !isCompressedCodexRolloutPath(path) {
		return f, nil
	}
	decoder, err := newCodexRolloutDecoder(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &codexRolloutLineStream{decoder: decoder, file: f}, nil
}

type codexRolloutLineStream struct {
	decoder *zstd.Decoder
	file    *os.File
}

func (s *codexRolloutLineStream) Read(p []byte) (int, error) {
	return s.decoder.Read(p)
}

func (s *codexRolloutLineStream) Close() error {
	s.decoder.Close()
	return s.file.Close()
}
