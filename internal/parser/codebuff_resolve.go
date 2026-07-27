// ABOUTME: Helpers for resolving a bare on-disk Codebuff/Freebuff
// ABOUTME: timestamp directory name back to its canonical session
// ABOUTME: ID. Used by the CLI session-get command to support
// ABOUTME: `agentsview session get <timestamp>` without having to
// ABOUTME: add a generic SourceSessionID lookup query to every
// ABOUTME: storage backend.
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
// Empty rawID returns nil. A roots entry that does not exist on
// disk is silently skipped. The agent type is determined by which
// Roots list the match was found in; run-state.json is not read.
//
// Duplicate timestamps across projects return multiple matches
// so the caller can surface an explicit ambiguity error listing
// every candidate canonical ID instead of silently picking one.
func FindCodebuffFreebuffMatches(
	pairs []CodebuffFamilyRoots,
	rawID string,
) []CodebuffFamilyMatch {
	if rawID == "" {
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
