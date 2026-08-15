package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const deepSeekHarnessSessionPrefix = "deepseek-harness:"

func newDeepSeekHarnessProviderFactory(def AgentDef) ProviderFactory {
	inner := NewSourceSetFactory(
		def,
		deepSeekHarnessProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newDeepSeekHarnessSourceSet(cfg.Roots)
		},
	)
	return deepSeekHarnessProviderFactory{ProviderFactory: inner}
}

// deepSeekHarnessProviderFactory keeps Harness's arbitrary raw IDs out of the
// generic provider normalizer. Other providers intentionally accept prefixed
// RawSessionID values, while Harness must preserve literal '~', "%7E", and
// "%25" bytes and only decode the canonical escaping used by FullSessionID.
type deepSeekHarnessProviderFactory struct {
	ProviderFactory
}

func (f deepSeekHarnessProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	provider := f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)
	return &deepSeekHarnessProvider{SourceSetProvider: provider}
}

type deepSeekHarnessProvider struct {
	*SourceSetProvider
}

func (p *deepSeekHarnessProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if req.RawSessionID == "" {
		req.RawSessionID = decodeDeepSeekHarnessCanonicalRawID(
			ProviderRawSessionIDFromFull(p.Def, req.FullSessionID),
		)
	}
	return p.sources.FindSource(ctx, req)
}

func newDeepSeekHarnessSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentDeepSeekHarness, roots,
		WithRecursive(),
		WithExtensions(".jsonl", ".zstd"),
		WithIncludePath(isPreferredDeepSeekHarnessSourcePath),
		WithProjectHint(deepSeekHarnessProjectHint),
		WithSessionIDFromPath(deepSeekHarnessSessionIDFromPath),
		WithLookupIDValid(func(rawID string) bool {
			return strings.TrimSpace(rawID) != ""
		}),
		WithCompanionFiles(deepSeekHarnessAlternateSourceFiles),
		WithCompanionTranscript(deepSeekHarnessAlternateSourcePath),
		WithParseFile(deepSeekHarnessParseFile),
		WithForceReplace(),
	)
}

func deepSeekHarnessParseFile(
	ctx context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	if err := rejectMixedDeepSeekHarnessEncoding(path); err != nil {
		return nil, nil, err
	}
	result, err := parseDeepSeekHarnessSession(ctx, path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if req.Fingerprint.Hash != "" {
		result.Session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{result}, nil, nil
}

func deepSeekHarnessPathParts(root, path string) (
	project, encodedID string, ok bool,
) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 ||
		(parts[2] != "session.jsonl" && parts[2] != "session.jsonl.zstd") {
		return "", "", false
	}
	if parts[0] != "_no-cwd" &&
		(!strings.HasPrefix(parts[0], "--") || !strings.HasSuffix(parts[0], "--")) {
		return "", "", false
	}
	if parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return "", "", false
	}
	if _, err := decodeDeepSeekHarnessSegment(parts[1]); err != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func isDeepSeekHarnessSourcePath(root, path string) bool {
	_, _, ok := deepSeekHarnessPathParts(root, path)
	return ok
}

// isPreferredDeepSeekHarnessSourcePath keeps discovery deterministic if an
// invalid session directory contains both physical encodings. Zstd is the
// upstream default, so it owns the logical source while both paths exist; the
// parse step still rejects the mixed directory instead of choosing either log.
func isPreferredDeepSeekHarnessSourcePath(root, path string) bool {
	if !isDeepSeekHarnessSourcePath(root, path) {
		return false
	}
	if filepath.Base(path) == "session.jsonl.zstd" {
		return true
	}
	_, err := os.Lstat(path + ".zstd")
	return errors.Is(err, os.ErrNotExist)
}

func deepSeekHarnessAlternateSourcePath(path string) (string, bool) {
	switch filepath.Base(path) {
	case "session.jsonl":
		return path + ".zstd", true
	case "session.jsonl.zstd":
		return strings.TrimSuffix(path, ".zstd"), true
	default:
		return "", false
	}
}

func deepSeekHarnessAlternateSourceFiles(path string) []string {
	alternate, ok := deepSeekHarnessAlternateSourcePath(path)
	if !ok {
		return nil
	}
	return []string{alternate}
}

func rejectMixedDeepSeekHarnessEncoding(path string) error {
	alternate, ok := deepSeekHarnessAlternateSourcePath(path)
	if !ok {
		return nil
	}
	_, err := os.Lstat(alternate)
	switch {
	case err == nil:
		return fmt.Errorf(
			"DeepSeek Harness session directory contains both session.jsonl and session.jsonl.zstd",
		)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("stat alternate DeepSeek Harness session log: %w", err)
	}
}

// DeepSeek Harness session IDs are arbitrary strings, while AgentsView reserves
// '~' as the remote-machine separator. Escape both the separator and the escape
// marker so the canonical ID remains readable, reversible, and local.
func encodeDeepSeekHarnessCanonicalRawID(rawID string) string {
	rawID = strings.ReplaceAll(rawID, "%", "%25")
	return strings.ReplaceAll(rawID, "~", "%7E")
}

func decodeDeepSeekHarnessCanonicalRawID(rawID string) string {
	rawID = strings.ReplaceAll(rawID, "%7E", "~")
	return strings.ReplaceAll(rawID, "%25", "%")
}

func deepSeekHarnessCanonicalSessionID(rawID string) string {
	return deepSeekHarnessSessionPrefix + encodeDeepSeekHarnessCanonicalRawID(rawID)
}

func deepSeekHarnessSessionIDFromPath(root, path string) string {
	_, encodedID, ok := deepSeekHarnessPathParts(root, path)
	if !ok {
		return ""
	}
	id, err := decodeDeepSeekHarnessSegment(encodedID)
	if err != nil {
		return ""
	}
	return id
}

func deepSeekHarnessProjectHint(root, path string) string {
	project, _, ok := deepSeekHarnessPathParts(root, path)
	if !ok || project == "_no-cwd" {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(project, "--"), "--")
}

func deepSeekHarnessProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
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
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			TruncationStatus:     CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}
