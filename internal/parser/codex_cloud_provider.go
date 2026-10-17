package parser

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// codexCloudMirrorSuffix ends every Codex cloud mirror file. Fetcher state
// files sit beside them and never match.
const codexCloudMirrorSuffix = ".jsonl.zstd"

func newCodexCloudProviderFactory(def AgentDef) ProviderFactory {
	return codexCloudProviderFactory{NewSourceSetFactory(
		def,
		codexCloudProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newCodexCloudSourceSet(cfg.Roots)
		},
	)}
}

// codexCloudProviderFactory adds raw capture of mirror files to the
// source set provider.
type codexCloudProviderFactory struct{ ProviderFactory }

func (f codexCloudProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &codexCloudProvider{f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)}
}

type codexCloudProvider struct{ *SourceSetProvider }

var _ StreamingRawCaptureSourceProvider = (*codexCloudProvider)(nil)

func (p *codexCloudProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context, yield func(SourceRef) error,
) (bool, error) {
	err := p.DiscoverEach(withRawCaptureStreamingTraversal(ctx), yield)
	return err == nil, err
}

// PlanRawCapture captures the mirror file alone. Fetcher state files hold
// offsets and cursors for this machine only, so they never travel.
func (p *codexCloudProvider) PlanRawCapture(
	ctx context.Context, source SourceRef,
) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	src, ok := source.Opaque.(JSONLSource)
	if !ok || src.Root == "" || src.Path == "" || isS3URI(src.Root) ||
		!isCodexCloudMirrorPath(src.Root, src.Path) {
		return RawCapturePlan{}, invalidRawCapturePlan("codex-cloud source is not a local mirror file")
	}
	info, err := os.Lstat(src.Path)
	if err != nil {
		return RawCapturePlan{}, err
	}
	if !info.Mode().IsRegular() {
		return RawCapturePlan{}, invalidRawCapturePlan("codex-cloud mirror is not a regular file")
	}
	rel, err := filepath.Rel(src.Root, src.Path)
	if err != nil {
		return RawCapturePlan{}, invalidRawCapturePlan(
			"resolve codex-cloud mirror path: %s", rawCaptureFilesystemError(err),
		)
	}
	return RawCapturePlan{
		ConfiguredRoot: src.Root,
		CaptureRoot:    src.Root,
		SourceKey:      source.Key,
		// The fetcher only appends whole zstd frames, apart from cutting a
		// torn tail; the capturer's prefix check turns that into a replace.
		Entries: []RawCaptureEntry{{
			Path: filepath.ToSlash(rel), LocalPath: src.Path, Appendable: true,
		}},
	}, nil
}

func newCodexCloudSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentCodexCloud, roots,
		WithRecursive(),
		// filepath.Ext of a mirror file is ".zstd"; the default list is
		// ".jsonl" only.
		WithExtensions(".zstd"),
		WithIncludePath(isCodexCloudMirrorPath),
		WithSessionIDFromPath(codexCloudThreadIDFromPath),
		WithLookupIDValid(func(rawID string) bool {
			return strings.TrimSpace(rawID) != ""
		}),
		WithParseFile(codexCloudParseFile),
		WithForceReplace(),
	)
}

func isCodexCloudMirrorPath(_, path string) bool {
	return codexCloudThreadIDFromPath("", path) != ""
}

func codexCloudThreadIDFromPath(_, path string) string {
	id, ok := strings.CutSuffix(filepath.Base(path), codexCloudMirrorSuffix)
	if !ok {
		return ""
	}
	return id
}

func codexCloudParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	result, ok, err := ParseCodexCloudFile(ctx, path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		// The monitor writes thread metadata before anything else, so a
		// file without it was written by a broken or older fetcher.
		log.Printf("codex-cloud: skipping %s: no thread metadata record", path)
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{result}, nil, nil
}

func codexCloudProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		RawCapture: RawCaptureCapabilities{
			Support: CapabilitySupported,
			Shape:   RawCaptureShapeFiles,
			Append:  RawCaptureAppendOne,
		},
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
