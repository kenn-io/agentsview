package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// SiblingMetadataSourceSetOptions configures companion files that should map
// back to a primary JSONL source.
type SiblingMetadataSourceSetOptions struct {
	SiblingGlobs         []string
	SiblingPaths         func(root, sourcePath string) []string
	SourcePathForSibling func(root, siblingPath string) (string, bool)
}

// SiblingMetadataSourceSet extends JSONLSourceSet for source layouts where a
// primary transcript file has sibling metadata files that affect freshness.
type SiblingMetadataSourceSet struct {
	JSONLSourceSet
	siblingOptions SiblingMetadataSourceSetOptions
}

// NewSiblingMetadataSourceSet returns a JSONL source helper with sibling
// metadata event and fingerprint support.
func NewSiblingMetadataSourceSet(
	provider AgentType,
	roots []string,
	options JSONLSourceSetOptions,
	siblingOptions SiblingMetadataSourceSetOptions,
) SiblingMetadataSourceSet {
	return SiblingMetadataSourceSet{
		JSONLSourceSet: jsonlSourceSetFromOptions(provider, roots, options),
		siblingOptions: siblingOptions,
	}
}

func (s SiblingMetadataSourceSet) WatchPlan(ctx context.Context) (WatchPlan, error) {
	plan, err := s.JSONLSourceSet.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	for i := range plan.Roots {
		plan.Roots[i].IncludeGlobs = append(
			plan.Roots[i].IncludeGlobs,
			s.siblingOptions.SiblingGlobs...,
		)
	}
	return plan, nil
}

func (s SiblingMetadataSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := s.JSONLSourceSet.SourcesForChangedPath(ctx, req)
	if err != nil || len(sources) > 0 {
		return sources, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.siblingOptions.SourcePathForSibling == nil {
		return nil, nil
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		sourcePath, ok := s.siblingOptions.SourcePathForSibling(root, req.Path)
		if !ok {
			continue
		}
		source, ok, err := s.sourceForPath(ctx, sourcePath)
		if err != nil {
			return nil, err
		}
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s SiblingMetadataSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	resolved, ok, err := s.sourceFromRef(ctx, source)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("sibling metadata source path unavailable")
	}
	src := resolved.Opaque.(JSONLSource)
	path := src.Path
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	fingerprint := SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(h, "source", path, info); err != nil {
		return SourceFingerprint{}, err
	}
	if s.siblingOptions.SiblingPaths != nil {
		for _, siblingPath := range s.siblingOptions.SiblingPaths(src.Root, path) {
			siblingInfo, err := siblingMetadataFileInfo(siblingPath)
			if err != nil {
				return SourceFingerprint{}, err
			}
			if siblingInfo == nil {
				continue
			}
			fingerprint.Size += siblingInfo.Size()
			if siblingMTime := siblingInfo.ModTime().UnixNano(); siblingMTime > fingerprint.MTimeNS {
				fingerprint.MTimeNS = siblingMTime
			}
			if err := addSiblingMetadataFingerprintPart(
				h, "sibling", siblingPath, siblingInfo,
			); err != nil {
				return SourceFingerprint{}, err
			}
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

func (s SiblingMetadataSourceSet) sourceFromRef(
	ctx context.Context,
	source SourceRef,
) (SourceRef, bool, error) {
	switch src := source.Opaque.(type) {
	case JSONLSource:
		if src.Root != "" && src.Path != "" {
			return source, true, nil
		}
	case *JSONLSource:
		if src != nil && src.Root != "" && src.Path != "" {
			source.Opaque = *src
			return source, true, nil
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if ref, ok, err := s.sourceForPath(ctx, candidate); err != nil {
			return SourceRef{}, false, err
		} else if ok {
			return ref, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func siblingMetadataFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, nil
	}
	return info, nil
}

func addSiblingMetadataFingerprintPart(
	h interface{ Write([]byte) (int, error) },
	label string,
	path string,
	info os.FileInfo,
) error {
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(
		h,
		"%s:%s:%d:%d:%s\n",
		label,
		filepath.Base(path),
		info.Size(),
		info.ModTime().UnixNano(),
		hash,
	)
	return nil
}
