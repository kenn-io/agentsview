package remotesync

import (
	"context"
	"io"

	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// Source fetches the remote half of one sync: the remote's targets, a
// manifest of them, and archives of their files. HTTPSource talks to a remote
// daemon; package sshremote runs agentsview on the remote over SSH.
type Source interface {
	// Targets resolves the files the remote publishes.
	Targets(ctx context.Context) (TargetSet, error)
	// Manifest lists targets with sizes and mtimes. supported is false when
	// the remote cannot build a manifest; the caller then falls back to a full
	// archive.
	Manifest(ctx context.Context, targets TargetSet) (manifest Manifest, supported bool, err error)
	// Archive opens a tar stream for request. Close on the stream reports
	// any failure the remote reported after the body ended.
	Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error)
}

// ProgressReporter is a Source that reports progress of its own, such as an
// SSH source installing agentsview on its host. RemoteSync hands it the
// sync's progress callback before the first fetch. That callback is
// serialized when several hosts prepare at once, so a source must report only
// through it.
type ProgressReporter interface {
	ReportProgress(progress syncpkg.ProgressFunc)
}

// ArchiveEncoding names the compression of an ArchiveStream body.
type ArchiveEncoding string

const (
	ArchiveEncodingNone ArchiveEncoding = ""
	ArchiveEncodingGzip ArchiveEncoding = "gzip"
	ArchiveEncodingZstd ArchiveEncoding = "zstd"
)

// ArchiveStream is an open archive body returned by a Source.
type ArchiveStream struct {
	Body     io.ReadCloser
	Encoding ArchiveEncoding
	// Size is the encoded body length in bytes, or 0 when unknown.
	Size int64
}
