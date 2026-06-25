package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// QwenPaw stores each session as a JSON file under
// <root>/<workspace>/sessions/[<subdir>/]<stem>.json. It is a
// directory-of-files provider: discovery, watching, change classification, and
// fingerprinting come from JSONLSourceSet. The ParseFile option makes that
// source set a full SourceSet so it rides the generic factory. Its colon-joined
// raw IDs are resolved by reconstruction (RawSessionIDSourceFiles), and a
// DB-recorded path outside the configured roots is honored via
// StoredPathFallbackRoot; ForceReplace mirrors the wholesale-rewrite parse
// outcome.
func newQwenPawProviderFactory(def AgentDef) ProviderFactory {
	return newSourceSetFactory(
		def,
		qwenPawProviderCapabilities(),
		func(cfg ProviderConfig) SourceSet { return newQwenPawSourceSet(cfg.Roots) },
	)
}

func newQwenPawSourceSet(roots []string) JSONLSourceSet {
	return newJSONLSourceSet(AgentQwenPaw, roots,
		withRecursive(),
		withExtensions(".json"),
		withContentHashing(),
		withSymlinkFollowing(),
		withDescendPath(qwenPawDescendPath),
		withIncludePath(isQwenPawSourcePath),
		withProjectHint(qwenPawProjectHintFromPath),
		withSessionIDFromPath(qwenPawSessionIDFromPath),
		withRawSessionIDSourceFiles(qwenPawRawSessionIDSourceFiles),
		withStoredPathFallbackRoot(qwenPawStoredPathRoot),
		withParseFile(qwenPawParseFile),
		withForceReplace(),
	)
}

func qwenPawParseFile(
	_ context.Context, path string, req ParseRequest,
) ([]ParseResult, []string, error) {
	sess, msgs, err := parseQwenPawSession(path, req.Source.ProjectHint, req.Machine)
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

// qwenPawRawSessionIDSourceFiles reconstructs the sessions JSON path from a
// colon-joined raw ID across each configured root. The filename-stem discovery
// scan cannot match these IDs because they contain colons.
func qwenPawRawSessionIDSourceFiles(roots []string, rawID string) []string {
	var candidates []string
	for _, root := range roots {
		if path := qwenPawSourceFileForRawID(root, rawID); path != "" {
			candidates = append(candidates, path)
		}
	}
	return candidates
}

// qwenPawStoredPathRoot synthesizes the configured root for a stored qwenpaw
// source path that is not under any current root. It locates the implicit
// <root>/<workspace>/sessions/ layout, validates the path is a real qwenpaw
// source shape, and confirms the file still exists so a stale DB row does not
// resolve to a missing file.
func qwenPawStoredPathRoot(storedPath string) (string, bool) {
	path := filepath.Clean(storedPath)
	root, ok := qwenPawImplicitRoot(path)
	if !ok || !isQwenPawSourcePath(root, path) {
		return "", false
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", false
	}
	return root, true
}

// qwenPawImplicitRoot derives the QwenPaw root implied by a source path of the
// form <root>/<workspace>/sessions/[<subdir>/]<stem>.json. The root is the
// grandparent of the sessions/ directory. Returns false when the path has no
// sessions/ segment in the expected position.
func qwenPawImplicitRoot(path string) (string, bool) {
	// path = <root>/<workspace>/sessions/<stem>.json
	//     or <root>/<workspace>/sessions/<subdir>/<stem>.json
	sessionsDir := filepath.Dir(path)
	if filepath.Base(sessionsDir) != "sessions" {
		// Allow one subdir level (e.g. console/).
		sessionsDir = filepath.Dir(sessionsDir)
		if filepath.Base(sessionsDir) != "sessions" {
			return "", false
		}
	}
	workspaceDir := filepath.Dir(sessionsDir)
	root := filepath.Dir(workspaceDir)
	if root == "" || root == "." {
		return "", false
	}
	return root, true
}

// qwenPawSourceFileForRawID resolves a rawID to a sessions JSON file under root.
//
// Raw ID shapes:
//
//   - <workspace>:<stem>            -> <root>/<workspace>/sessions/<stem>.json
//   - <workspace>:<subdir>:<stem>   -> <root>/<workspace>/sessions/<subdir>/<stem>.json
//
// The subdir segment disambiguates the sessions/console/ layout from the
// sessions/ root so two files with the same stem cannot collide.
//
// Returns "" when the rawID is malformed, references a traversal component
// (".", ".."), escapes the resolved sessions directory, or the file does
// not exist.
func qwenPawSourceFileForRawID(root, rawID string) string {
	if root == "" {
		return ""
	}
	workspace, rest, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidQwenPawIDPart(workspace) {
		return ""
	}
	var candidate string
	if subdir, stem, found := strings.Cut(rest, ":"); found {
		if !IsValidQwenPawIDPart(subdir) ||
			!IsValidQwenPawIDPart(stem) {
			return ""
		}
		candidate = filepath.Join(
			root, workspace, "sessions", subdir, stem+".json",
		)
	} else {
		if !IsValidQwenPawIDPart(rest) {
			return ""
		}
		candidate = filepath.Join(
			root, workspace, "sessions", rest+".json",
		)
	}
	if !qwenPawCandidateUnderRoot(root, candidate) {
		return ""
	}
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// qwenPawCandidateUnderRoot reports whether candidate resolves to a path
// inside <root>/<workspace>/sessions/. Both sides are cleaned and converted
// to absolute form so that "." / ".." segments in the candidate cannot
// escape the QwenPaw root.
func qwenPawCandidateUnderRoot(root, candidate string) bool {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	absCand, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCand)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[1] != "sessions" {
		return false
	}
	return true
}

func isQwenPawSourcePath(root, path string) bool {
	parts, ok := qwenPawSourcePathParts(root, path)
	return ok && qwenPawSourcePathPartsValid(parts)
}

func qwenPawSourcePathPartsValid(parts []string) bool {
	if len(parts) < 3 || parts[1] != "sessions" {
		return false
	}
	workspace := parts[0]
	stem, ok := strings.CutSuffix(parts[len(parts)-1], ".json")
	if !ok || !IsValidQwenPawIDPart(workspace) ||
		!IsValidQwenPawIDPart(stem) {
		return false
	}
	switch len(parts) {
	case 3:
		return true
	case 4:
		subdir := parts[2]
		return !strings.HasPrefix(subdir, ".") &&
			IsValidQwenPawIDPart(subdir)
	default:
		return false
	}
}

func qwenPawProjectHintFromPath(root, path string) string {
	parts, ok := qwenPawSourcePathParts(root, path)
	if !ok || len(parts) < 3 {
		return ""
	}
	return parts[0]
}

func qwenPawSessionIDFromPath(root, path string) string {
	parts, ok := qwenPawSourcePathParts(root, path)
	if !ok || !qwenPawSourcePathPartsValid(parts) {
		return ""
	}
	stem := strings.TrimSuffix(parts[len(parts)-1], ".json")
	if len(parts) == 4 {
		return parts[0] + ":" + parts[2] + ":" + stem
	}
	return parts[0] + ":" + stem
}

func qwenPawSourcePathParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false
		}
	}
	return parts, true
}

func qwenPawDescendPath(root, path string) bool {
	parts, ok := qwenPawSourcePathParts(root, path)
	if !ok {
		return false
	}
	switch len(parts) {
	case 1:
		return IsValidQwenPawIDPart(parts[0])
	case 2:
		return IsValidQwenPawIDPart(parts[0]) && parts[1] == "sessions"
	case 3:
		subdir := parts[2]
		if parts[1] != "sessions" ||
			!IsValidQwenPawIDPart(parts[0]) ||
			strings.HasPrefix(subdir, ".") ||
			!IsValidQwenPawIDPart(subdir) {
			return false
		}
		info, err := os.Lstat(path)
		if err != nil {
			return true
		}
		return info.Mode()&os.ModeSymlink == 0
	default:
		return false
	}
}

func qwenPawProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:       CapabilitySupported,
			Thinking:           CapabilitySupported,
			ToolCalls:          CapabilitySupported,
			ToolResults:        CapabilitySupported,
			MalformedLineCount: CapabilitySupported,
		},
	}
}
