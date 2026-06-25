package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

var (
	fetchS3Object       = parser.FetchS3Object
	statS3Object        = parser.StatS3Object
	statClaudeS3Session = parser.StatClaudeS3Session
)

func s3SourceFileInfo(file parser.DiscoveredFile) (os.FileInfo, error) {
	size := file.SourceSize
	mtime := file.SourceMtime
	if mtime == 0 {
		stat := statS3Object
		if file.Agent == parser.AgentClaude {
			stat = statClaudeS3Session
		}
		obj, err := stat(file.Path)
		if err != nil {
			return nil, err
		}
		size = obj.Size
		mtime = obj.LastModified.UnixNano()
	}
	return fakeSnapshotInfo{
		fName:  path.Base(file.Path),
		fSize:  size,
		fMtime: mtime,
	}, nil
}

func isS3SourcePath(path string) bool {
	return strings.HasPrefix(path, "s3://")
}

func s3SessionIDPrefix(machine string) string {
	if machine == "" {
		return ""
	}
	return machine + "~"
}

func applyIDPrefixToID(prefix, id string) string {
	if prefix == "" || id == "" || strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + id
}

func applyIDPrefixToIDs(prefix string, ids []string) []string {
	if prefix == "" || len(ids) == 0 {
		return ids
	}
	prefixed := make([]string, len(ids))
	for i, id := range ids {
		prefixed[i] = applyIDPrefixToID(prefix, id)
	}
	return prefixed
}

func applyIDPrefixToParsedResult(
	prefix string, result *parser.ParseResult,
) {
	if prefix == "" {
		return
	}
	result.Session.ID = applyIDPrefixToID(prefix, result.Session.ID)
	result.Session.ParentSessionID = applyIDPrefixToID(
		prefix, result.Session.ParentSessionID,
	)
	result.Session.SourceSessionID = applyIDPrefixToID(
		prefix, result.Session.SourceSessionID,
	)
	for i := range result.Messages {
		for j := range result.Messages[i].ToolCalls {
			call := &result.Messages[i].ToolCalls[j]
			call.SubagentSessionID = applyIDPrefixToID(
				prefix, call.SubagentSessionID,
			)
			for k := range call.ResultEvents {
				event := &call.ResultEvents[k]
				event.SubagentSessionID = applyIDPrefixToID(
					prefix, event.SubagentSessionID,
				)
			}
		}
	}
}

func safeS3TempRelPath(file parser.DiscoveredFile) (string, error) {
	trimmed := strings.TrimPrefix(file.Path, "s3://")
	parts := strings.Split(trimmed, "/")
	relParts := parts
	if len(parts) > 1 {
		relParts = parts[1:]
	}
	if file.Agent == parser.AgentClaude {
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "raw" && parts[i+1] == "claude" {
				relParts = parts[i+2:]
				break
			}
		}
	}
	if file.Agent == parser.AgentCodex {
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "raw" && parts[i+1] == "codex" {
				relParts = parts[i+2:]
				break
			}
		}
	}
	if len(relParts) == 0 {
		return "", fmt.Errorf("unsafe s3 object name: %q", file.Path)
	}
	for _, part := range relParts {
		if part == "" || part == "." || part == ".." ||
			strings.ContainsAny(part, `\/`) {
			return "", fmt.Errorf("unsafe s3 object name: %q", file.Path)
		}
	}
	return filepath.Join(relParts...), nil
}

func (e *Engine) shouldSkipFileWithPrefix(
	prefix, sessionID string, info os.FileInfo,
) bool {
	if e.forceParse {
		return false
	}
	fullID := applyIDPrefixToID(prefix, sessionID)
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(
		fullID,
	)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	if e.db.GetSessionDataVersion(fullID) <
		db.CurrentDataVersion() {
		return false
	}
	return true
}

// processS3Session reads a Claude/Codex session JSONL directly from object
// storage (in-process, no persistent local mirror): download the object's
// bytes, buffer them to a transient temp file so the existing path-based
// parsers (incremental offsets, subagent layout) work unchanged, run the
// normal per-agent processor, then delete the temp file.
func (e *Engine) processS3Session(
	ctx context.Context, file parser.DiscoveredFile, sourceInfo os.FileInfo,
) processResult {
	idPrefix := s3SessionIDPrefix(file.Machine)
	switch file.Agent {
	case parser.AgentClaude:
		sessionID := strings.TrimSuffix(sourceInfo.Name(), ".jsonl")
		fullID := applyIDPrefixToID(idPrefix, sessionID)
		if e.shouldSkipFileWithPrefix(idPrefix, sessionID, sourceInfo) &&
			e.db.GetSessionFilePath(fullID) == file.Path {
			sess, _ := e.db.GetSession(ctx, fullID)
			if sess != nil &&
				sess.Project != "" &&
				!parser.NeedsProjectReparse(sess.Project) {
				return processResult{skip: true}
			}
		}
	case parser.AgentCodex:
		if uuid := parser.CodexSessionUUIDFromFilename(
			path.Base(file.Path),
		); uuid != "" {
			sessionID := "codex:" + uuid
			fullID := applyIDPrefixToID(idPrefix, sessionID)
			if e.shouldSkipFileWithPrefix(idPrefix, sessionID, sourceInfo) &&
				e.db.GetSessionFilePath(fullID) == file.Path {
				return processResult{skip: true}
			}
		}
	}

	relPath, err := safeS3TempRelPath(file)
	if err != nil {
		return processResult{err: err}
	}
	rc, err := fetchS3Object(file.Path)
	if err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	defer rc.Close()
	dir, err := os.MkdirTemp("", "avs3-")
	if err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	f, err := os.Create(tmp)
	if err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	// Stream the object straight to disk so a large session never has to be
	// held whole in memory.
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return processResult{err: err, noCacheSkip: true}
	}
	if err := f.Close(); err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	hydratedToolResults := false
	sawPersistedToolResults := false
	if file.Agent == parser.AgentClaude {
		rewrote, sawPersisted, err := hydrateS3ClaudeToolResults(tmp, file.Path)
		if err != nil {
			return processResult{err: err, noCacheSkip: true}
		}
		hydratedToolResults = rewrote
		sawPersistedToolResults = sawPersisted
	}
	local := file
	local.Path = tmp
	var res processResult
	switch file.Agent {
	case parser.AgentClaude:
		res = e.processClaudeWithStoredSkip(ctx, local, sourceInfo, false)
	case parser.AgentCodex:
		res = e.processCodex(local, sourceInfo)
	default:
		return processResult{}
	}
	// Record the real s3:// source on each parsed session rather than the
	// transient temp path (which is deleted on return), so the stored source
	// pointer reflects where the session actually came from.
	for i := range res.results {
		applyIDPrefixToParsedResult(idPrefix, &res.results[i])
		res.results[i].Session.File.Path = file.Path
		res.results[i].Session.File.Size = sourceInfo.Size()
		res.results[i].Session.File.Mtime = sourceInfo.ModTime().UnixNano()
	}
	if hydratedToolResults || sawPersistedToolResults {
		res.forceReplace = true
	}
	res.excludedSessionIDs = applyIDPrefixToIDs(
		idPrefix, res.excludedSessionIDs,
	)
	return res
}

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

func s3MachineFromRoot(root string) string {
	segs := strings.Split(strings.TrimPrefix(root, "s3://"), "/")
	for i := len(segs) - 2; i > 1; i-- {
		if segs[i] == "raw" && isS3AgentRootSegment(segs[i+1]) {
			return segs[i-1]
		}
	}
	return ""
}

func isS3AgentRootSegment(seg string) bool {
	return seg == "claude" || seg == "codex"
}

func s3RelFromRoot(root, uri string) (string, bool) {
	prefix := strings.TrimSuffix(root, "/")
	if !strings.HasPrefix(uri, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(uri, prefix+"/"), true
}

func (e *Engine) hydrateS3DiscoveredFile(
	ctx context.Context, sessionID string, file *parser.DiscoveredFile,
) {
	if !isS3SourcePath(file.Path) {
		return
	}
	if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil {
		if sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		}
	}
	for _, root := range e.agentDirs[file.Agent] {
		if !isS3SourcePath(root) {
			continue
		}
		rel, ok := s3RelFromRoot(root, file.Path)
		if !ok {
			continue
		}
		if file.Machine == "" {
			file.Machine = s3MachineFromRoot(root)
		}
		if file.Project == "" && file.Agent == parser.AgentClaude {
			if first, _, ok := strings.Cut(rel, "/"); ok {
				file.Project = first
			}
		}
		break
	}
	if file.Machine == "" {
		if host, _ := parser.StripHostPrefix(sessionID); host != "" {
			file.Machine = host
		}
	}
	if file.SourceMtime == 0 {
		stat := statS3Object
		if file.Agent == parser.AgentClaude {
			stat = statClaudeS3Session
		}
		if obj, err := stat(file.Path); err == nil {
			file.SourceSize = obj.Size
			file.SourceMtime = obj.LastModified.UnixNano()
		}
	}
}
