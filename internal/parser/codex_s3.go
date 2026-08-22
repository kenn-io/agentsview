package parser

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

func (p *codexProvider) S3Scanner() S3SessionScanner {
	if p.spec.agent != AgentCodex {
		return S3SessionScanner{
			Agent: p.spec.agent,
			Keep:  func(string, []string) bool { return false },
		}
	}
	return codexS3Scanner()
}

func (p *codexProvider) S3SessionID(uri string) string {
	if p.spec.agent != AgentCodex {
		return ""
	}
	uuid := CodexSessionUUIDFromFilename(pathBase(uri))
	if uuid == "" {
		return ""
	}
	return "codex:" + uuid
}

func (p *codexProvider) S3TempRelPath(objectPath string) (string, error) {
	if p.spec.agent != AgentCodex {
		return "", fmt.Errorf("unsafe s3 object name: %q", objectPath)
	}
	return s3TempRelPathAfterRawAgent(objectPath, "codex", codexS3TempRelParts)
}

func (p *codexProvider) S3StatSession(uri string) (S3Object, error) {
	return StatCodexS3Session(uri)
}

func (p *codexProvider) S3PostFetchHydrate(
	tempDir, tempPath, configuredRoot, objectURI string,
) error {
	return nil
}

// StatCodexS3Session returns metadata for a Codex rollout object.
func StatCodexS3Session(uri string) (S3Object, error) {
	return statS3Object(uri)
}

// codexS3Scanner lists Codex rollout-*.jsonl under an s3:// sessions root
// (any depth — Codex nests under 2026/MM/DD/). Project is derived from
// session content, so it is left empty here, as in the local path.
func codexS3Scanner() S3SessionScanner {
	return S3SessionScanner{
		Agent: AgentCodex,
		Keep: func(_ string, segs []string) bool {
			return isCodexSessionFilename(segs[len(segs)-1])
		},
	}
}

// FindCodexS3ParentSessionURI locates one explicitly named parent rollout
// under the same configured Codex S3 root as childURI. It lists metadata only;
// callers decide whether and where to materialize the matching object.
func FindCodexS3ParentSessionURI(
	configuredRoot, childURI, parentID string,
) (string, bool) {
	if parentID == "" || strings.TrimSpace(parentID) != parentID ||
		strings.ContainsAny(parentID, `/\\`) ||
		CodexSessionUUIDFromFilename("rollout-x-"+parentID+".jsonl") != parentID {
		return "", false
	}
	root, ok := codexS3RootURI(configuredRoot, childURI)
	if !ok {
		return "", false
	}
	objects, err := listS3Objects(root)
	if err != nil {
		return "", false
	}
	var matches []string
	for _, obj := range objects {
		if _, withinRoot := s3RelativePath(root, obj.URI); !withinRoot {
			continue
		}
		name := path.Base(obj.URI)
		if CodexSessionUUIDFromFilename(name) == parentID {
			matches = append(matches, obj.URI)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	sort.Slice(matches, func(i, j int) bool {
		iArchived := strings.Contains(matches[i], "/archived_sessions/")
		jArchived := strings.Contains(matches[j], "/archived_sessions/")
		if iArchived != jArchived {
			return !iArchived
		}
		return matches[i] < matches[j]
	})
	return matches[0], true
}

func codexS3RootURI(configuredRoot, sessionURI string) (string, bool) {
	if !strings.HasPrefix(sessionURI, "s3://") {
		return "", false
	}
	if configuredRoot != "" {
		configuredRoot = strings.TrimSuffix(configuredRoot, "/")
		if !strings.HasPrefix(configuredRoot, "s3://") {
			return "", false
		}
		if _, ok := s3RelativePath(configuredRoot, sessionURI); !ok {
			return "", false
		}
		return configuredRoot, true
	}
	parts := strings.Split(strings.TrimPrefix(sessionURI, "s3://"), "/")
	if len(parts) < 2 || !isCodexSessionFilename(parts[len(parts)-1]) {
		return "", false
	}
	for i := len(parts) - 3; i >= 1; i-- {
		if parts[i] == "raw" && parts[i+1] == "codex" {
			return "s3://" + strings.Join(parts[:i+2], "/"), true
		}
	}
	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "sessions" || parts[i] == "archived_sessions" {
			return "s3://" + strings.Join(parts[:i], "/"), true
		}
	}
	if len(parts) >= 5 && IsDigits(parts[len(parts)-4]) &&
		IsDigits(parts[len(parts)-3]) && IsDigits(parts[len(parts)-2]) {
		return "s3://" + strings.Join(parts[:len(parts)-4], "/"), true
	}
	return "s3://" + strings.Join(parts[:len(parts)-1], "/"), true
}

// CodexS3SessionIndexURI returns the session_index.jsonl URI adjacent to the
// configured Codex sessions root represented by a rollout URI.
func CodexS3SessionIndexURI(sessionURI string) (string, bool) {
	if !strings.HasPrefix(sessionURI, "s3://") {
		return "", false
	}
	trimmed := strings.TrimPrefix(sessionURI, "s3://")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || !isCodexSessionFilename(parts[len(parts)-1]) {
		return "", false
	}

	for i := len(parts) - 3; i >= 1; i-- {
		if parts[i] == "raw" && parts[i+1] == "codex" {
			rootEnd := i + 2
			if rootEnd < len(parts)-1 &&
				(parts[rootEnd] == "sessions" ||
					parts[rootEnd] == "archived_sessions") {
				return s3URIWithLast(parts[:rootEnd], CodexSessionIndexFilename), true
			}
			return s3URIWithLast(parts[:i+1], CodexSessionIndexFilename), true
		}
	}

	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "sessions" || parts[i] == "archived_sessions" {
			return s3URIWithLast(parts[:i], CodexSessionIndexFilename), true
		}
	}

	sessionRootEnd := len(parts) - 1
	if len(parts) >= 5 &&
		IsDigits(parts[len(parts)-4]) &&
		IsDigits(parts[len(parts)-3]) &&
		IsDigits(parts[len(parts)-2]) {
		sessionRootEnd = len(parts) - 4
	}
	if sessionRootEnd <= 0 {
		return "", false
	}
	parent := parts[:sessionRootEnd]
	if len(parent) > 1 {
		parent = parent[:len(parent)-1]
	}
	return s3URIWithLast(parent, CodexSessionIndexFilename), true
}

func codexS3TempRelParts(parts []string) []string {
	for i, part := range parts {
		if part == "sessions" || part == "archived_sessions" {
			return parts[i:]
		}
	}
	if len(parts) == 0 {
		return parts
	}
	return append([]string{"sessions"}, parts...)
}
