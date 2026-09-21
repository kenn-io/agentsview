package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func deepSeekHarnessPathVersion(path string) (int64, bool) {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".zstd")
	if name == "session.jsonl" {
		return 0, true
	}
	const prefix = "session.v"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
		return 0, false
	}
	digits := name[len(prefix) : len(name)-len(".jsonl")]
	if digits == "" || digits[0] == '0' {
		return 0, false
	}
	for index := range digits {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, false
		}
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}

// isSupportedDeepSeekHarnessFormatVersion keeps discovery inside the released
// generations this build reads. A newer harness writes its own generation
// alongside the ones already in a session directory, and every released bump so
// far renamed required events or renumbered sequences, so an unread generation
// cannot stand in for a supported one. Discovery ignores it and keeps reporting
// the newest generation it can actually parse; an explicitly targeted log still
// fails with the header's unsupported-version error.
func isSupportedDeepSeekHarnessFormatVersion(version int64) bool {
	return version >= deepSeekHarnessOldestFormatVersion &&
		version <= deepSeekHarnessNewestFormatVersion
}

func deepSeekHarnessPathParts(root, path string) (
	project, encodedID string, version int64, ok bool,
) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", 0, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	version, ok = deepSeekHarnessPathVersion(parts[2])
	if !ok || !isSupportedDeepSeekHarnessFormatVersion(version) {
		return "", "", 0, false
	}
	if parts[0] != "_no-cwd" &&
		(!strings.HasPrefix(parts[0], "--") || !strings.HasSuffix(parts[0], "--")) {
		return "", "", 0, false
	}
	if parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return "", "", 0, false
	}
	if _, err := decodeDeepSeekHarnessSegment(parts[1]); err != nil {
		return "", "", 0, false
	}
	return parts[0], parts[1], version, true
}

// isPreferredDeepSeekHarnessSourcePath keeps discovery deterministic when one
// session directory retains multiple immutable generations or both physical
// encodings. The numerically newest canonical generation wins; within that
// generation zstd owns the logical source while both encodings exist. The parse
// step still rejects a mixed-encoding directory instead of choosing either log.
func isPreferredDeepSeekHarnessSourcePath(root, path string) bool {
	_, _, version, ok := deepSeekHarnessPathParts(root, path)
	if !ok {
		return false
	}
	pathIsZstd := strings.HasSuffix(filepath.Base(path), ".zstd")
	highest := int64(-1)
	zstdAtVersion := false
	entries, err := os.ReadDir(filepath.Dir(path))
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			siblingVersion, siblingOK := deepSeekHarnessPathVersion(entry.Name())
			if !siblingOK || !isSupportedDeepSeekHarnessFormatVersion(siblingVersion) {
				continue
			}
			if siblingVersion > highest {
				highest = siblingVersion
			}
			if siblingVersion == version && strings.HasSuffix(entry.Name(), ".zstd") {
				zstdAtVersion = true
			}
		}
	}
	switch {
	case version < highest:
		return false
	case version > highest:
		// A missing newest generation is still the deletion event that should
		// retire the session; only an existing older generation is shadowed.
		return true
	case pathIsZstd:
		return true
	default:
		return !zstdAtVersion
	}
}

func deepSeekHarnessAlternateSourcePath(path string) (string, bool) {
	if strings.HasSuffix(filepath.Base(path), ".zstd") {
		plain := strings.TrimSuffix(path, ".zstd")
		if _, ok := deepSeekHarnessPathVersion(plain); ok {
			return plain, true
		}
		return "", false
	}
	if _, ok := deepSeekHarnessPathVersion(path); ok {
		return path + ".zstd", true
	}
	return "", false
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
		plain, compressed := path, alternate
		if strings.HasSuffix(filepath.Base(path), ".zstd") {
			plain, compressed = alternate, path
		}
		return fmt.Errorf(
			"DeepSeek Harness session directory contains both %s and %s",
			filepath.Base(plain), filepath.Base(compressed),
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
	_, encodedID, _, ok := deepSeekHarnessPathParts(root, path)
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
	project, _, _, ok := deepSeekHarnessPathParts(root, path)
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
