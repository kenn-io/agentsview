package parser

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// S3Provider is the opt-in surface that lets a single-file provider ingest
// sessions from an s3:// root laid out as .../<machine>/raw/<agent>/...
// Claude and Codex implement it with extra sidecar, fork, and index behavior.
// Simple JSONL providers embed DefaultS3Provider.
type S3Provider interface {
	S3Scanner() S3SessionScanner
	S3SessionID(uri string) string
	S3TempRelPath(objectPath string) (string, error)
	S3StatSession(uri string) (S3Object, error)
	S3PostFetchHydrate(tempDir, tempPath, configuredRoot, objectURI string) error
}

// DefaultS3Provider covers the common single-file S3 layout: keep objects whose
// names match Extensions, derive project from the first path segment, identify
// the session as IDPrefix plus the filename stem, strip raw/<agent> for the
// temp materialization path, and skip sidecar folding and post-fetch hydrate.
type DefaultS3Provider struct {
	Agent      AgentType
	IDPrefix   string
	Extensions []string
}

var s3ProviderCache sync.Map // AgentType -> S3Provider (nil sentinel = not supported)

type s3ProviderNone struct{}

// S3ProviderFor returns the S3Provider implementation for a registered agent,
// if that agent advertises S3 discovery and its provider implements the
// interface. Results are cached.
func S3ProviderFor(agent AgentType) (S3Provider, bool) {
	if !AgentSupportsS3Discovery(agent) {
		s3ProviderCache.Store(agent, s3ProviderNone{})
		return nil, false
	}
	if v, ok := s3ProviderCache.Load(agent); ok {
		if _, none := v.(s3ProviderNone); none {
			return nil, false
		}
		return v.(S3Provider), true
	}
	p, ok := NewProvider(agent, ProviderConfig{})
	if !ok {
		s3ProviderCache.Store(agent, s3ProviderNone{})
		return nil, false
	}
	sp, ok := p.(S3Provider)
	if !ok {
		s3ProviderCache.Store(agent, s3ProviderNone{})
		return nil, false
	}
	s3ProviderCache.Store(agent, sp)
	return sp, true
}

// AgentSupportsS3Discovery reports whether the agent's factory declares
// Source.S3Discovery. Scheduling and S3 root-segment detection should trust
// this flag rather than a hardcoded agent whitelist.
func AgentSupportsS3Discovery(agent AgentType) bool {
	factory, ok := ProviderFactoryByType(agent)
	if !ok {
		return false
	}
	return factory.Capabilities().Source.S3Discovery == CapabilitySupported
}

func (p DefaultS3Provider) S3Scanner() S3SessionScanner {
	exts := append([]string(nil), p.Extensions...)
	return S3SessionScanner{
		Agent: p.Agent,
		Keep: func(_ string, segs []string) bool {
			if len(segs) < 2 {
				return false
			}
			base := segs[len(segs)-1]
			for _, ext := range exts {
				if ext != "" && strings.HasSuffix(strings.ToLower(base), strings.ToLower(ext)) {
					return true
				}
			}
			return false
		},
		Project: func(_ string, segs []string) string {
			if len(segs) == 0 {
				return ""
			}
			return segs[0]
		},
	}
}

func (p DefaultS3Provider) S3SessionID(uri string) string {
	base := path.Base(uri)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" || (stem == base && ext == "") {
		return ""
	}
	return p.IDPrefix + stem
}

func (p DefaultS3Provider) S3TempRelPath(objectPath string) (string, error) {
	return s3TempRelPathAfterRawAgent(objectPath, string(p.Agent), nil)
}

func (p DefaultS3Provider) S3StatSession(uri string) (S3Object, error) {
	return statS3Object(uri)
}

func (p DefaultS3Provider) S3PostFetchHydrate(
	tempDir, tempPath, configuredRoot, objectURI string,
) error {
	return nil
}

func s3TempRelPathAfterRawAgent(
	objectPath, agentSeg string, rewrite func([]string) []string,
) (string, error) {
	trimmed := strings.TrimPrefix(objectPath, "s3://")
	parts := strings.Split(trimmed, "/")
	relParts := parts
	if len(parts) > 1 {
		relParts = parts[1:]
	}
	if agentSeg != "" {
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "raw" && parts[i+1] == agentSeg {
				relParts = parts[i+2:]
				break
			}
		}
	}
	if rewrite != nil {
		relParts = rewrite(relParts)
	}
	return sanitizeS3TempRelParts(objectPath, relParts)
}

func sanitizeS3TempRelParts(objectPath string, relParts []string) (string, error) {
	if len(relParts) == 0 {
		return "", fmt.Errorf("unsafe s3 object name: %q", objectPath)
	}
	for _, part := range relParts {
		if part == "" || part == "." || part == ".." ||
			strings.ContainsAny(part, `\/`) {
			return "", fmt.Errorf("unsafe s3 object name: %q", objectPath)
		}
	}
	return filepath.Join(relParts...), nil
}

func isS3URI(root string) bool {
	return strings.HasPrefix(root, "s3://")
}
