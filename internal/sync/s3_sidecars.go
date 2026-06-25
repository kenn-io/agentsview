package sync

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
)

func hydrateS3ClaudeToolResults(
	sessionPath, sessionURI string,
) (rewrote, sawPersisted bool, err error) {
	rewritePath := sessionPath + ".s3rewrite"
	rewrote, sawPersisted, err = writeS3ClaudeToolResultsRewrite(
		sessionPath, sessionURI, rewritePath,
	)
	if err != nil {
		_ = os.Remove(rewritePath)
		return false, sawPersisted, err
	}
	if !rewrote {
		_ = os.Remove(rewritePath)
		return false, sawPersisted, nil
	}
	if err := os.Rename(rewritePath, sessionPath); err != nil {
		_ = os.Remove(rewritePath)
		return false, sawPersisted, err
	}
	return true, sawPersisted, nil
}

func writeS3ClaudeToolResultsRewrite(
	sessionPath, sessionURI, rewritePath string,
) (bool, bool, error) {
	in, err := os.Open(sessionPath)
	if err != nil {
		return false, false, err
	}
	defer in.Close()

	out, err := os.Create(rewritePath)
	if err != nil {
		return false, false, err
	}

	rewrote := false
	sawPersisted := false
	downloaded := make(map[string]string)
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			body, suffix := splitLineSuffix(line)
			rewritten, changed, sawLinePersisted, rewriteErr := rewriteS3ClaudeToolResultLine(
				sessionPath, sessionURI, body, downloaded,
			)
			if sawLinePersisted {
				sawPersisted = true
			}
			if rewriteErr != nil {
				_ = out.Close()
				return false, sawPersisted, rewriteErr
			}
			if changed {
				rewrote = true
			}
			if _, writeErr := io.WriteString(
				out, rewritten+suffix,
			); writeErr != nil {
				_ = out.Close()
				return false, sawPersisted, writeErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = out.Close()
			return false, sawPersisted, err
		}
	}
	if err := out.Close(); err != nil {
		return false, sawPersisted, err
	}
	return rewrote, sawPersisted, nil
}

func splitLineSuffix(line string) (body, suffix string) {
	if before, ok := strings.CutSuffix(line, "\r\n"); ok {
		return before, "\r\n"
	}
	if before, ok := strings.CutSuffix(line, "\n"); ok {
		return before, "\n"
	}
	return line, ""
}

func rewriteS3ClaudeToolResultLine(
	sessionPath, sessionURI, line string, downloaded map[string]string,
) (string, bool, bool, error) {
	if !strings.Contains(line, "persisted-output") &&
		!strings.Contains(line, "persistedOutputPath") {
		return line, false, false, nil
	}

	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return line, false, false, nil
	}
	msg, ok := top["message"].(map[string]any)
	if !ok {
		return line, false, false, nil
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return line, false, false, nil
	}

	resolvePath := func(original string) (string, bool, error) {
		return localS3ClaudeToolResultPath(
			sessionPath, sessionURI, original, downloaded,
		)
	}

	changed := false
	sawPersisted := false
	if tur, ok := top["toolUseResult"].(map[string]any); ok {
		if p, ok := tur["persistedOutputPath"].(string); ok {
			if _, ok := s3ClaudeToolResultRef(p, sessionURI); ok {
				sawPersisted = true
			}
			local, ok, err := resolvePath(p)
			if err != nil {
				return "", false, sawPersisted, err
			}
			if ok {
				tur["persistedOutputPath"] = local
				changed = true
			}
		}
	}
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		content, ok := block["content"].(string)
		if !ok {
			continue
		}
		original := persistedOutputPathFromContent(content)
		if original == "" {
			continue
		}
		if _, ok := s3ClaudeToolResultRef(original, sessionURI); ok {
			sawPersisted = true
		}
		local, ok, err := resolvePath(original)
		if err != nil {
			return "", false, sawPersisted, err
		}
		if !ok {
			continue
		}
		block["content"] = strings.ReplaceAll(content, original, local)
		changed = true
	}
	if !changed {
		return line, false, sawPersisted, nil
	}
	encoded, err := json.Marshal(top)
	if err != nil {
		return "", false, sawPersisted, err
	}
	return string(encoded), true, sawPersisted, nil
}

func persistedOutputPathFromContent(content string) string {
	const marker = "Full output saved to:"
	_, after, ok := strings.Cut(content, marker)
	if !ok {
		return ""
	}
	rest := strings.TrimSpace(after)
	if before, _, ok := strings.Cut(rest, "\n"); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(rest)
}

func localS3ClaudeToolResultPath(
	sessionPath, sessionURI, original string, downloaded map[string]string,
) (string, bool, error) {
	ref, ok := s3ClaudeToolResultRef(original, sessionURI)
	if !ok {
		return "", false, nil
	}
	key := ref.cacheKey()
	if local, ok := downloaded[key]; ok {
		return local, true, nil
	}
	uri := s3ClaudeToolResultURI(sessionURI, ref)
	local := s3ClaudeToolResultLocalPath(sessionPath, ref)
	rc, err := fetchS3Object(uri)
	if err != nil {
		if isMissingS3Object(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return "", false, err
	}
	f, err := os.Create(local)
	if err != nil {
		return "", false, err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		_ = os.Remove(local)
		if isMissingS3Object(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if err := f.Close(); err != nil {
		return "", false, err
	}
	downloaded[key] = local
	return local, true, nil
}

func isMissingS3Object(err error) bool {
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && resp.Code == minio.NoSuchKey
}

func s3ClaudeToolResultRel(original string) (string, bool) {
	ref, ok := s3ClaudeToolResultRef(original, "")
	if !ok {
		return "", false
	}
	return ref.Rel, true
}

type s3ClaudeToolResultReference struct {
	Rel           string
	SubagentLocal bool
}

func (r s3ClaudeToolResultReference) cacheKey() string {
	if r.SubagentLocal {
		return "subagent:" + r.Rel
	}
	return "parent:" + r.Rel
}

func s3ClaudeToolResultRef(
	original, sessionURI string,
) (s3ClaudeToolResultReference, bool) {
	normalized, ok := normalizePersistedOutputPath(original)
	if !ok {
		return s3ClaudeToolResultReference{}, false
	}
	parts := strings.Split(normalized, "/")
	layoutStart := s3ClaudeToolResultLayoutStart(parts)
	if layoutStart >= 0 {
		return s3ClaudeToolResultRefFromLayout(parts, layoutStart)
	}
	return s3ClaudeToolResultRefFromSessionURI(parts, sessionURI)
}

func s3ClaudeToolResultRefFromLayout(
	parts []string, layoutStart int,
) (s3ClaudeToolResultReference, bool) {
	toolResultsIdx := -1
	for i := layoutStart + 1; i < len(parts); i++ {
		part := parts[i]
		if part == "tool-results" {
			toolResultsIdx = i
			break
		}
	}
	if toolResultsIdx < 0 || toolResultsIdx == len(parts)-1 {
		return s3ClaudeToolResultReference{}, false
	}
	rel := strings.Join(parts[toolResultsIdx+1:], "/")
	if !safeS3RelPath(rel) {
		return s3ClaudeToolResultReference{}, false
	}
	return s3ClaudeToolResultReference{
		Rel:           rel,
		SubagentLocal: s3ClaudeToolResultIsSubagentLocal(parts, layoutStart, toolResultsIdx),
	}, true
}

func s3ClaudeToolResultRefFromSessionURI(
	parts []string, sessionURI string,
) (s3ClaudeToolResultReference, bool) {
	parentSuffix, subagentSuffix := s3ClaudeSessionSidecarSuffixes(sessionURI)
	if len(parentSuffix) == 0 {
		return s3ClaudeToolResultReference{}, false
	}
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] != "tool-results" {
			continue
		}
		rel := strings.Join(parts[i+1:], "/")
		if !safeS3RelPath(rel) {
			continue
		}
		before := parts[:i]
		if len(subagentSuffix) > 0 &&
			hasStringSuffix(before, subagentSuffix) {
			return s3ClaudeToolResultReference{
				Rel:           rel,
				SubagentLocal: true,
			}, true
		}
		if hasStringSuffix(before, parentSuffix) {
			return s3ClaudeToolResultReference{
				Rel:           rel,
				SubagentLocal: false,
			}, true
		}
	}
	return s3ClaudeToolResultReference{}, false
}

func s3ClaudeSessionSidecarSuffixes(
	sessionURI string,
) (parentSuffix, subagentSuffix []string) {
	if sessionURI == "" {
		return nil, nil
	}
	sessionPath := strings.TrimSuffix(sessionURI, ".jsonl")
	parts := strings.Split(sessionPath, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, nil
	}
	if layoutParts := s3ClaudeObjectLayoutParts(parts); len(layoutParts) > 0 {
		parts = layoutParts
	}
	sessionName := parts[len(parts)-1]
	if strings.HasPrefix(sessionName, "agent-") {
		subagentsIdx := -1
		for i := len(parts) - 2; i > 0; i-- {
			if parts[i] == "subagents" {
				subagentsIdx = i
				break
			}
		}
		if subagentsIdx > 0 && subagentsIdx < len(parts)-1 {
			parentSuffix = []string{parts[subagentsIdx-1]}
			subagentSuffix = append([]string(nil), parts[subagentsIdx-1:]...)
			return parentSuffix, subagentSuffix
		}
	}
	return []string{sessionName}, nil
}

func s3ClaudeObjectLayoutParts(parts []string) []string {
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "raw" && parts[i+1] == "claude" {
			if i+2 < len(parts) {
				return parts[i+2:]
			}
			return nil
		}
	}
	return nil
}

func hasStringSuffix(values, suffix []string) bool {
	if len(suffix) == 0 || len(values) < len(suffix) {
		return false
	}
	start := len(values) - len(suffix)
	for i := range suffix {
		if values[start+i] != suffix[i] {
			return false
		}
	}
	return true
}

func s3ClaudeToolResultLayoutStart(parts []string) int {
	layoutStart := -1
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == ".claude" && parts[i+1] == "projects" {
			layoutStart = i + 3
		}
	}
	return layoutStart
}

func s3ClaudeToolResultIsSubagentLocal(
	parts []string, layoutStart, toolResultsIdx int,
) bool {
	for i := layoutStart + 1; i < toolResultsIdx-1; i++ {
		if parts[i] == "subagents" {
			return true
		}
	}
	return false
}

func normalizePersistedOutputPath(original string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(original), `\`, "/")
	if !isPortableAbsPath(normalized) {
		return "", false
	}
	parts := make([]string, 0, strings.Count(normalized, "/")+1)
	for part := range strings.SplitSeq(normalized, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return "", false
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

func isPortableAbsPath(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	if len(path) >= 3 &&
		((path[0] >= 'A' && path[0] <= 'Z') ||
			(path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && path[2] == '/' {
		return true
	}
	return strings.HasPrefix(path, "//")
}

func safeS3RelPath(rel string) bool {
	if rel == "" {
		return false
	}
	for part := range strings.SplitSeq(rel, "/") {
		if part == "" || part == "." || part == ".." ||
			strings.ContainsAny(part, `\`) {
			return false
		}
	}
	return true
}

func s3ClaudeToolResultURI(
	sessionURI string, ref s3ClaudeToolResultReference,
) string {
	base := strings.TrimSuffix(sessionURI, ".jsonl")
	if !ref.SubagentLocal && strings.HasPrefix(path.Base(base), "agent-") {
		if idx := strings.LastIndex(base, "/subagents/"); idx > 0 {
			base = base[:idx]
		}
	}
	return base + "/tool-results/" + ref.Rel
}

func s3ClaudeToolResultLocalPath(
	sessionPath string, ref s3ClaudeToolResultReference,
) string {
	base := strings.TrimSuffix(sessionPath, ".jsonl")
	needle := string(filepath.Separator) + "subagents" + string(filepath.Separator)
	if !ref.SubagentLocal && strings.HasPrefix(filepath.Base(base), "agent-") {
		if idx := strings.LastIndex(sessionPath, needle); idx > 0 {
			base = sessionPath[:idx]
		}
	}
	return filepath.Join(base, "tool-results", filepath.FromSlash(ref.Rel))
}
