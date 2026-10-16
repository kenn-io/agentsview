package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/agentsview/internal/ctxio"
)

// Codex compresses a rollout whose file has not changed for a week to
// rollout-<ts>-<uuid>.jsonl.zst and removes the plain file, keeping the
// file's modification time. The compressed file contains the original JSONL
// bytes, not a separate transcript. Codex reads prefer the plain sibling while
// both exist. Resuming a compressed rollout restores the JSONL at the same
// path and removes the compressed file before returning for append.
// Compression does not replicate sessions into other configured roots.
//
// Upstream lifecycle and source links are recorded in
// docs/internal/session-format-sources.md under Codex. Keep representation
// selection distinct from policies for independently imported copies.
//
// AgentsView decodes the compressed stream once through io.Reader. Compressed
// rollouts are parsed in full; no checkpoint or cursor resumes from offsets
// into their compressed bytes.
const (
	codexRolloutSuffix           = ".jsonl"
	codexCompressedRolloutSuffix = ".jsonl.zst"
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

// codexRolloutSnapshot owns the source file and yields decoded bytes.
type codexRolloutSnapshot struct {
	File       *os.File
	Reader     io.Reader
	Source     os.FileInfo
	Compressed bool
	decoder    *zstd.Decoder
	raw        io.Reader
	hash       hash.Hash
}

// plainCodexRolloutSnapshot wraps an already open plain rollout.
func plainCodexRolloutSnapshot(f *os.File, info os.FileInfo) codexRolloutSnapshot {
	return codexRolloutSnapshot{File: f, Reader: io.LimitReader(f, info.Size()), Source: info}
}

// openCodexRollout opens a bounded snapshot of the source. Compressed bytes
// are hashed and decoded as the caller reads, without a temporary copy.
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
	snapshot := plainCodexRolloutSnapshot(f, info)
	if isCompressedCodexRolloutPath(path) {
		snapshot.Compressed = true
		snapshot.hash = sha256.New()
		snapshot.raw = io.TeeReader(snapshot.Reader, snapshot.hash)
		snapshot.decoder, err = newZstdStreamDecoder(ctxio.Reader{Context: ctx, Reader: snapshot.raw})
		if err != nil {
			snapshot.Close()
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		snapshot.Reader = snapshot.decoder
	}
	return &snapshot, nil
}

// Close releases the decoder and the original source file.
func (s *codexRolloutSnapshot) Close() error {
	if s == nil || s.File == nil {
		return nil
	}
	if s.decoder != nil {
		s.decoder.Close()
	}
	err := s.File.Close()
	s.File = nil
	return err
}

// sourceHash completes the compressed-byte fingerprint without decoding again.
func (s *codexRolloutSnapshot) sourceHash(ctx context.Context) (string, error) {
	if _, err := io.Copy(io.Discard, ctxio.Reader{Context: ctx, Reader: s.raw}); err != nil {
		return "", err
	}
	return hex.EncodeToString(s.hash.Sum(nil)), nil
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
	decoder, err := newZstdStreamDecoder(f)
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
