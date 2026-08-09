// ABOUTME: Helpers for resolving a bare on-disk Codebuff/Freebuff
// ABOUTME: timestamp directory name to a session location.
// ABOUTME: Codebuff and Freebuff share the same on-disk layout
// ABOUTME: (<root>/<project>/chats/<timestamp>/); the caller
// ABOUTME: decides which agent type owns each location because
// ABOUTME: Freebuff is intentionally absent from parser.Registry
// ABOUTME: and so cfg.ResolveDirs(parser.AgentFreebuff) is often
// ABOUTME: empty, leaving the shared codebuff roots as the only
// ABOUTME: location to interrogate on disk.
package parser

import (
	"os"
	"path/filepath"
)

// CodebuffFamilyRoots ties one of the two storage agents to its
// list of root directories. Both Codebuff and Freebuff share the
// same on-disk layout: <root>/<project>/chats/<timestamp>/. The
// agent type (Codebuff vs Freebuff) is read from run-state.json
// per session, but for bare-ID resolution at the CLI boundary
// each roots list is associated with one agent type up front so
// the resolver can build the canonical ID prefix without reading
// every candidate's run-state.json.
type CodebuffFamilyRoots struct {
	Agent AgentType
	Roots []string
}

// CodebuffFamilyMatch is one possible canonical ID for a bare
// timestamp resolved against the on-disk storage. The Agent field
// distinguishes Codebuff from Freebuff so the canonical ID prefix
// matches the on-disk agentType (rather than the registry default).
type CodebuffFamilyMatch struct {
	Agent       AgentType
	ProjectHint string
	RawID       string
}

// CanonicalID returns the full canonical ID the parser would have
// stored for this match: "<agent>:<project>:<rawID>".
func (c CodebuffFamilyMatch) CanonicalID() string {
	return string(c.Agent) + ":" + c.ProjectHint + ":" + c.RawID
}

// FindCodebuffFreebuffMatches walks each roots list looking for
// a directory whose chats/<rawID> subdirectory contains an
// existing chat-messages.json. Returns one match per (agent,
// project) pair whose chats subdirectory matches rawID.
//
// Empty rawID returns nil. rawID must be a single filesystem-safe
// path component: IDs containing path separators, "." or ".."
// segments, or absolute paths fail closed and return nil, so a
// traversal-shaped rawID can never join into a path outside the
// configured roots. This holds independently of the caller's
// IsCodebuffTimestamp pre-gate. A roots entry that does not exist on
// disk is silently skipped. The agent type is determined by which
// Roots list the match was found in; run-state.json is not read.
//
// Duplicate timestamps across projects return multiple matches
// so the caller can surface an explicit ambiguity error listing
// every candidate canonical ID instead of silently picking one.
//
// Note: Freebuff is intentionally absent from parser.Registry, so
// cfg.ResolveDirs(parser.AgentFreebuff) is empty whenever the user
// has not set FREEBUFF_DIR explicitly. In that case callers
// (e.g. cmd/agentsview/session_get.go resolveBareCodebuffID) must
// test both AgentCodebuff and AgentFreebuff prefixes against the
// service per matched (project, rawID) location, because the
// shared codebuff root list may contain freebuff sessions the
// resolver cannot classify from the registry alone.
func FindCodebuffFreebuffMatches(
	pairs []CodebuffFamilyRoots,
	rawID string,
) []CodebuffFamilyMatch {
	if !isSafeSinglePathComponent(rawID) {
		return nil
	}
	var out []CodebuffFamilyMatch
	for _, p := range pairs {
		for _, root := range p.Roots {
			projects, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, project := range projects {
				if !project.IsDir() {
					continue
				}
				chatPath := filepath.Join(
					root, project.Name(), "chats",
					rawID, "chat-messages.json",
				)
				if !isWithinRoot(root, chatPath) {
					continue
				}
				if _, err := os.Stat(chatPath); err != nil {
					continue
				}
				out = append(out, CodebuffFamilyMatch{
					Agent:       p.Agent,
					ProjectHint: project.Name(),
					RawID:       rawID,
				})
			}
		}
	}
	return out
}
