package parser

import (
	"context"
	"path/filepath"
	"strings"
)

// kimiWorkSessionDirPrefix marks session directories under a Kimi Work
// (kimi-desktop "daimon" runtime) workspace that hold real user
// conversations. The runtime also writes auxiliary internal sessions
// (ctitle-* title generation, sklsum-* skill summaries, dvlt-* dev-loop
// tasks) into the same tree; those are not user conversations and must
// never be discovered, classified, or parsed, so every path predicate
// below requires this prefix on the session-directory component.
const kimiWorkSessionDirPrefix = "conv-"

// Kimi Work stores each conversation as a kimi-code kernel session, a
// wire.jsonl transcript byte-identical to what the Kimi provider reads.
// Parsing is shared with Kimi via parseKimiSession; this provider only
// narrows the accepted path shapes to conv-* session directories and
// re-labels the parsed session identity (kimi-work: prefix, AgentKimiWork)
// after parsing instead of forking the parser.
func newKimiWorkProviderFactory(def AgentDef) ProviderFactory {
	return NewSourceSetFactory(
		def,
		kimiProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet {
			return newKimiWorkSourceSet(cfg.Roots)
		},
	)
}

func newKimiWorkSourceSet(roots []string) JSONLSourceSet {
	return NewJSONLSourceSet(AgentKimiWork, roots,
		WithRecursive(),
		WithSymlinkFollowing(),
		WithIncludePath(isKimiWorkSourcePath),
		WithProjectHint(kimiProjectHintFromPath),
		WithSessionIDFromPath(func(root, path string) string {
			if !isKimiWorkSourcePath(root, path) {
				return ""
			}
			return kimiSessionIDFromPath(path)
		}),
		WithRawSessionIDSourceFiles(kimiWorkRawSessionIDSourceFiles),
		WithParseFile(kimiWorkParseFile),
		// Mirror the Kimi provider: persist a full-file content hash so a
		// resync does not clear the stored file_hash to NULL.
		WithContentHashing(),
	)
}

func kimiWorkParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseKimiSession(path, req.Source.ProjectHint, req.Machine)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}
	// parseKimiSession stamps the Kimi identity; rewrite it to Kimi Work
	// so IDs, the agent type, and session-level usage events all resolve
	// to this provider.
	rawID := strings.TrimPrefix(sess.ID, "kimi:")
	sess.ID = string(AgentKimiWork) + ":" + rawID
	sess.Agent = AgentKimiWork
	for i := range sess.UsageEvents {
		sess.UsageEvents[i].SessionID = sess.ID
		sess.UsageEvents[i].DedupKey = string(AgentKimiWork) + ":session:" + rawID
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return []ParseResult{{
		Session:     *sess,
		Messages:    msgs,
		UsageEvents: sess.UsageEvents,
	}}, nil, nil
}

// kimiWorkRawSessionIDSourceFiles reconstructs wire.jsonl candidate paths
// from a colon-joined raw ID, mirroring kimiRawSessionIDSourceFiles with
// the additional conv-* requirement on the session-directory component so
// auxiliary sessions can never be resolved through a stored or requested
// ID either.
func kimiWorkRawSessionIDSourceFiles(roots []string, rawID string) []string {
	parts := strings.Split(rawID, ":")
	if !kimiIDComponentsValid(parts...) {
		return nil
	}
	var sessionDir string
	switch len(parts) {
	case 2:
		sessionDir = parts[1]
	case 3:
		sessionDir = parts[2]
	default:
		return nil
	}
	if !isKimiWorkSessionDir(sessionDir) {
		return nil
	}
	var candidates []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		switch len(parts) {
		case 2:
			candidates = append(
				candidates,
				filepath.Join(root, parts[0], parts[1], "wire.jsonl"),
			)
		case 3:
			candidates = append(candidates, filepath.Join(
				root, parts[0], parts[2], "agents", parts[1], "wire.jsonl",
			))
		}
	}
	return candidates
}

func isKimiWorkSourcePath(root, path string) bool {
	parts, ok := kimiSourceRelParts(root, path)
	if !ok || len(parts) == 0 || parts[len(parts)-1] != "wire.jsonl" {
		return false
	}
	switch len(parts) {
	case 3:
		return isKimiWorkSessionDir(parts[1]) &&
			kimiIDComponentsValid(parts[0], parts[1])
	case 5:
		return parts[2] == "agents" &&
			isKimiWorkSessionDir(parts[1]) &&
			kimiIDComponentsValid(parts[0], parts[1], parts[3])
	default:
		return false
	}
}

func isKimiWorkSessionDir(name string) bool {
	return strings.HasPrefix(name, kimiWorkSessionDirPrefix)
}
