package parser

import (
	"context"
	"path/filepath"
)

// DeepSeek TUI stores each session as a single JSON file in a directory. It is
// a directory-of-files provider: discovery, watching, change classification,
// lookup, and fingerprinting come from JSONLSourceSet, and the ParseFile option
// makes that source set a full SourceSet so it rides the generic factory.
func newDeepSeekTUIProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		deepSeekTUIProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newDeepSeekTUISourceSet(cfg.Roots) },
	)
}

func newDeepSeekTUISourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentDeepSeekTUI, roots,
		withExtensions(".json"),
		withFollowSymlinkFiles(),
		withContentHashing(),
		withIncludePath(isDeepSeekTUISourcePath),
		withSessionIDFromPath(func(root, path string) string {
			return deepSeekTUISessionIDFromPath(path)
		}),
		withParseFile(deepSeekTUIParseFile),
	)
}

func deepSeekTUIParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseDeepSeekTUISession(path, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{{Session: *sess, Messages: msgs}}, nil, nil
}

func isDeepSeekTUISourcePath(root, path string) bool {
	return isDeepSeekTUISessionFile(filepath.Base(path))
}

func deepSeekTUIProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			SessionName:  CapabilitySupported,
			Cwd:          CapabilitySupported,
			Model:        CapabilitySupported,
			Thinking:     CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
