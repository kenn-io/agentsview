package parser

import (
	"context"
	"path/filepath"
)

// Amp stores each thread as a single JSON file in a directory. It is a
// directory-of-files provider: discovery, watching, change classification,
// lookup, and fingerprinting come from JSONLSourceSet, and the ParseFile option
// makes that source set a full SourceSet so it rides the generic factory.
func newAmpProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		ampProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newAmpSourceSet(cfg.Roots) },
	)
}

func newAmpSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentAmp, roots,
		WithExtensions(".json"),
		WithFollowSymlinkFiles(),
		WithContentHashing(),
		WithIncludePath(isAmpSourcePath),
		WithSessionIDFromPath(func(root, path string) string {
			return ampThreadIDFromPath(path)
		}),
		WithParseFile(ampParseFile),
	)
}

func ampParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseAmpSession(path, req.Machine)
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

func isAmpSourcePath(root, path string) bool {
	return IsAmpThreadFileName(filepath.Base(path))
}

func ampProviderCapabilities() Capabilities {
	return Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			Thinking:     CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
