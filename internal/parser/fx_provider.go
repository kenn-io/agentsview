package parser

import (
	"context"
	"path/filepath"
)

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
	return NewSourceSetFactory(def, caps, func(cfg ProviderConfig) SourceSet {
		return NewDirectoryJSONLSourceSet(AgentFx, cfg.Roots,
			WithDescendPath(func(root, path string) bool {
				return filepath.Dir(path) == filepath.Clean(root) && IsValidSessionID(filepath.Base(path)) ||
					filepath.Dir(filepath.Dir(path)) == filepath.Clean(root) && filepath.Base(path) == "subagent"
			}),
			WithIncludePath(func(root, path string) bool {
				return filepath.Base(path) == "events.jsonl" && filepath.Dir(filepath.Dir(path)) == filepath.Clean(root)
			}),
			WithSessionIDFromPath(func(_, path string) string { return filepath.Base(filepath.Dir(path)) }),
			WithProjectHint(func(_, _ string) string { return "" }),
			WithCompanionFiles(fxCompanionFiles),
			WithCompanionTranscript(fxCompanionTranscript),
			WithParseFile(fxParseFile),
			WithForceReplace(),
			WithContentHashing(),
		)
	})
}

func fxCompanionFiles(path string) []string {
	dir := filepath.Dir(path)
	return []string{filepath.Join(dir, "session.json"), filepath.Join(dir, "usage-v2.json"), filepath.Join(dir, "subagent", "owner.json")}
}

func fxCompanionTranscript(path string) (string, bool) {
	dir := filepath.Dir(path)
	switch filepath.Base(path) {
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
