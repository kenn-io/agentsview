package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

func newTauProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		tauProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newTauSourceSet(cfg) },
	)
}

func newTauSourceSet(cfg ProviderConfig) DirectoryJSONLSourceSet {
	return NewDirectoryJSONLSourceSet(AgentTau, cfg.Roots,
		WithIncludePath(isTauSourcePath),
		WithProjectHint(func(root, path string) string { return "" }),
		WithSessionIDFromPath(func(root, path string) string {
			return tauSessionIDFromPathWithRewriter(root, path, cfg.PathRewriter)
		}),
		WithLookupIDValid(isValidTauSessionID),
		WithParseFile(func(ctx context.Context, path string, req ParseRequest) (
			[]ParseResult, []string, error,
		) {
			return tauParseFile(ctx, path, req, cfg.PathRewriter)
		}),
		WithForceReplace(),
	)
}

func isTauSourcePath(root, path string) bool {
	name := filepath.Base(path)
	if name == "index.jsonl" || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	return isValidTauSessionID(tauSessionIDFromPath(root, path))
}

func tauSessionIDFromPath(root, path string) string {
	return tauSessionIDFromPathWithRewriter(root, path, nil)
}

func tauSessionIDFromPathWithRewriter(
	root, path string, pathRewriter func(string) string,
) string {
	name, ok := strings.CutSuffix(filepath.Base(path), ".jsonl")
	if !ok || name == "index" {
		return ""
	}
	if name != "default" {
		return name
	}
	if root == "" {
		root = filepath.Dir(filepath.Dir(path))
	}
	if pathRewriter != nil {
		root = pathRewriter(root)
	}
	rootHash := sha256.Sum256([]byte(filepath.Clean(root)))
	return "default-" + hex.EncodeToString([]byte(filepath.Base(filepath.Dir(path)))) + "-" +
		hex.EncodeToString(rootHash[:16])
}

func isValidTauSessionID(rawID string) bool {
	if rawID == "" {
		return false
	}
	for _, c := range rawID {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func tauParseFile(
	ctx context.Context, path string, req ParseRequest,
	pathRewriter func(string) string,
) ([]ParseResult, []string, error) {
	session, messages, err := parseTauSession(
		ctx, path, req.Machine, pathRewriter,
	)
	if err != nil {
		return nil, nil, err
	}
	if session == nil {
		return nil, nil, nil
	}
	if req.Fingerprint.Hash != "" {
		session.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{{Session: *session, Messages: messages}}, nil, nil
}

func tauProviderCapabilities() Capabilities {
	caps := Capabilities{
		Source: jsonlFileProviderSourceCapabilities(),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
	caps.Source.ForceReplaceOnParse = CapabilitySupported
	return caps
}
