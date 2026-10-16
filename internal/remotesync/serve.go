package remotesync

import (
	"context"
	"errors"
	"io"

	"go.kenn.io/agentsview/internal/config"
)

// Refusals shared by the HTTP routes and the SSH serve command. Their
// messages are the response bodies the HTTP routes return.
var (
	ErrTargetNotAllowed    = errors.New("remote sync target is not allowed")
	ErrFileNotAllowed      = errors.New("remote sync file is not allowed")
	ErrManifestUnsupported = errors.New("manifest unavailable for sanitized file-scoped agents")
)

// ServeManifest builds the manifest a collector asked for, limited to what
// the remote's own target resolution allows. Resolution and build errors are
// returned unwrapped so HTTP 500 bodies keep their existing text.
func ServeManifest(ctx context.Context, cfg config.Config, requested TargetSet) (Manifest, error) {
	allowed, err := ResolveTargets(cfg)
	if err != nil {
		return Manifest{}, err
	}
	targets, ok := SelectAllowedTargets(allowed, requested)
	if !ok {
		return Manifest{}, ErrTargetNotAllowed
	}
	// Sanitized file-scoped agents (Windsurf) stream a transformed subset of
	// their directory through WriteArchive; the manifest cannot model that
	// per-file transformation, and advertising the raw tree would let a delta
	// request fetch unsanitized files. Collectors fall back to the full
	// archive flow. Verbatim file-scoped agents (RooCode) are fine:
	// BuildManifest lists exactly their curated files.
	if targets.HasSanitizedFileScopedAgents() {
		return Manifest{}, ErrManifestUnsupported
	}
	return BuildManifest(ctx, targets)
}

// ArchivePlan is an authorized archive request, ready to stream. Planning
// before writing lets transports report refusals before any body bytes.
type ArchivePlan struct {
	allowed TargetSet
	targets TargetSet
	files   []string
	delta   bool
}

// PlanArchive authorizes request against the remote's own resolution. A
// non-nil DeltaFiles selects delta mode even when empty: an explicit empty
// delta yields an empty tar, not the full corpus.
func PlanArchive(cfg config.Config, request ArchiveRequest) (ArchivePlan, error) {
	allowed, err := ResolveTargets(cfg)
	if err != nil {
		return ArchivePlan{}, err
	}
	targets, ok := SelectAllowedTargets(allowed, request.TargetSet)
	if !ok {
		return ArchivePlan{}, ErrTargetNotAllowed
	}
	plan := ArchivePlan{allowed: allowed, targets: targets, delta: request.DeltaFiles != nil}
	if plan.delta {
		plan.files, ok = SelectAllowedFiles(allowed, request.DeltaFiles)
		if !ok {
			return ArchivePlan{}, ErrFileNotAllowed
		}
	}
	return plan, nil
}

// Write streams the planned archive to w.
func (p ArchivePlan) Write(ctx context.Context, w io.Writer) error {
	if p.delta {
		return WriteArchiveFiles(ctx, w, p.allowed, p.files)
	}
	return WriteArchive(ctx, w, p.targets)
}
