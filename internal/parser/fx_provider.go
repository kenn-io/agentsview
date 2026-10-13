package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

func fxSessionIDValid(id string) bool {
	if len(id) == 0 || len(id) > 255 || id == "." || id == ".." || strings.EqualFold(id, "v2") {
		return false
	}
	for _, c := range []byte(id) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func newFxProviderFactory(def AgentDef) ProviderFactory {
	caps := Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
	caps.Source.ForceReplaceOnParse = CapabilitySupported
	caps.Sync.FingerprintHashRequiredForFreshness = true
	return fxProviderFactory{NewSourceSetFactory(def, caps, func(cfg ProviderConfig) SourceSet {
		return NewDirectoryJSONLSourceSet(AgentFx, cfg.Roots,
			WithDescendPath(func(root, path string) bool {
				return filepath.Dir(path) == filepath.Clean(root) && fxSessionIDValid(filepath.Base(path))
			}),
			WithIncludePath(func(_, path string) bool {
				return filepath.Base(path) == "events.jsonl"
			}),
			WithLookupIDValid(fxSessionIDValid),
			WithSessionIDFromPath(func(_, path string) string { return filepath.Base(filepath.Dir(path)) }),
			WithProjectHint(func(_, _ string) string { return "" }),
			WithCompanionFiles(fxCompanionFiles),
			WithOptionalCompanion(func(path string) bool {
				return filepath.Base(path) == "usage-v2.json" || filepath.Base(path) == "tool-results"
			}, "tool-results"),
			WithCompanionTranscript(fxCompanionTranscript),
			WithParseFile(fxParseFile),
			WithForceReplace(),
			WithContentHashing(),
		)
	})}
}

type fxProviderFactory struct {
	ProviderFactory
}

func (f fxProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &fxSessionProvider{f.ProviderFactory.NewProvider(cfg).(*SourceSetProvider)}
}

type fxSessionProvider struct {
	*SourceSetProvider
}

func (p *fxSessionProvider) ParseSourceSize(ctx context.Context, source SourceRef) (int64, error) {
	path, ok, err := p.sources.(DirectoryJSONLSourceSet).pathFromSource(ctx, source)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("fx source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return 0, err
	}
	size := info.Size()
	for _, companion := range fxCompanionFiles(path) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		info, err := os.Lstat(companion)
		if filepath.Base(companion) != "tool-results" {
			info, err = os.Stat(companion)
		}
		if err != nil || info == nil {
			continue
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		} else if info.Mode().Type() == os.ModeDir && filepath.Base(companion) == "tool-results" {
			f, err := os.Open(path)
			if err != nil {
				return 0, err
			}
			defer f.Close()
			reader := newLineReaderContext(ctx, f, maxLineSize)
			defer releaseLineReader(reader)
			for {
				line, ok := reader.nextBytes()
				if !ok {
					break
				}
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				ref := gjson.GetBytes(line, "event.tool_result.artifact_ref").Str
				if !fxArtifactRefValid(ref) {
					continue
				}
				info, err := os.Lstat(filepath.Join(companion, ref))
				if err == nil && info != nil && info.Mode().IsRegular() {
					size += min(info.Size(), int64(maxPersistedToolResultSize))
				}
			}
			if err := reader.Err(); err != nil {
				return 0, err
			}
		}
	}
	return size, nil
}

func fxCompanionFiles(path string) []string {
	dir := filepath.Dir(path)
	return []string{filepath.Join(dir, "session.json"), filepath.Join(dir, "usage-v2.json"), filepath.Join(dir, "subagent", "owner.json"), filepath.Join(dir, "tool-results")}
}

func fxCompanionTranscript(path string) (string, bool) {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "tool-results" {
		return filepath.Join(filepath.Dir(dir), "events.jsonl"), true
	}
	switch filepath.Base(path) {
	case "tool-results":
		return filepath.Join(dir, "events.jsonl"), true
	case "session.json", "usage-v2.json":
		return filepath.Join(dir, "events.jsonl"), true
	case "owner.json":
		if filepath.Base(dir) == "subagent" {
			return filepath.Join(filepath.Dir(dir), "events.jsonl"), true
		}
	}
	return "", false
}

func fxParseFile(ctx context.Context, path string, req ParseRequest) ([]ParseResult, []string, error) {
	session, messages, err := parseFxSession(ctx, path, req.Machine)
	if err != nil || session == nil {
		return nil, nil, err
	}
	session.File.Hash = req.Fingerprint.Hash
	return []ParseResult{{Session: *session, Messages: messages, UsageEvents: session.UsageEvents}}, nil, nil
}
