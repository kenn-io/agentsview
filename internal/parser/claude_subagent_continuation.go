package parser

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A Claude sub-agent transcript lives under a parent session's subagents tree:
// <project>/<parent-session>/subagents/<agent-id>.jsonl, or deeper for tools
// that group dispatches under a workflow directory. The same named sub-agent can
// run again under a second parent session — a resumed session dispatching the
// same agent — and its transcript is then a second file with the same name under
// that second parent.
//
// Both files are one sub-agent session: the id names the agent, and the second
// run continues where the first stopped. The later transcript is therefore a
// companion of the session's own transcript, and its entries are appended to the
// one session. Spelling the sub-agent id per parent instead would rename every
// stored sub-agent session id — the id is written both in the sub-agent's own
// transcript and in each parent's link to it — to recover one file.
//
// The join is guarded: a companion is only joined when it starts after the run it
// continues has ended, and only when both parent sessions sit in the same project
// directory. A companion that fails either test is not joined, not recorded as
// skipped and not deleted: it stays visible as a file the archive does not
// explain, which is the honest state for two runs that cannot be shown to be one.

// claudeSubagentTranscriptRel reports the sub-agent transcript's path relative to
// its enclosing subagents directory, together with that directory. ok is false
// for any path that is not a sub-agent transcript, so project-level transcripts
// and tool-result companions are untouched.
func claudeSubagentTranscriptRel(
	path string,
) (subagentsDir, rel string, ok bool) {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "agent-") ||
		!strings.HasSuffix(base, ".jsonl") {
		return "", "", false
	}
	subagentsDir = claudeEnclosingSubagentsDir(path)
	if subagentsDir == "" {
		return "", "", false
	}
	rel, err := filepath.Rel(subagentsDir, path)
	if err != nil || rel == "" || rel == "." {
		return "", "", false
	}
	return subagentsDir, rel, true
}

// claudeSubagentSiblingTranscripts returns the transcripts of the same sub-agent
// dispatch under the other parent sessions of the same project. Membership is
// the same set seen from either file — same project directory, same path below
// each parent's subagents tree — so which file the sync happens to parse cannot
// change which entries the session ends up with.
func claudeSubagentSiblingTranscripts(path string) []string {
	subagentsDir, rel, ok := claudeSubagentTranscriptRel(path)
	if !ok {
		return nil
	}
	parentDir := filepath.Dir(subagentsDir)
	projectDir := filepath.Dir(parentDir)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil
	}
	parentName := filepath.Base(parentDir)
	var siblings []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == parentName {
			continue
		}
		candidate := filepath.Join(
			projectDir, entry.Name(), "subagents", rel,
		)
		if !IsRegularFile(candidate) {
			continue
		}
		siblings = append(siblings, candidate)
	}
	slices.Sort(siblings)
	return siblings
}

// claudeSubagentChainMember is one transcript of a sub-agent session, parsed.
type claudeSubagentChainMember struct {
	path   string
	result ParseResult
}

// joinClaudeSubagentContinuations appends the entries of a sub-agent's other
// transcripts to the session parsed from this one. The result is the same whole
// session whichever of the transcripts the sync parsed: members are ordered by
// their own first entry, so the appended ordinals — and with them the session's
// resume state — stay in the order the sub-agent actually ran.
//
// The parsed transcript stays the session's stored file: the sync engine keys
// freshness on a session's stored file path, so a parse that claimed a different
// file would re-parse on every pass.
func (p *claudeProvider) joinClaudeSubagentContinuations(
	ctx context.Context,
	path, project, machine string,
	opts claudeParseOptions,
	results []ParseResult,
) ([]ParseResult, bool, error) {
	if _, _, ok := claudeSubagentTranscriptRel(path); !ok {
		return results, false, nil
	}
	siblings := claudeSubagentSiblingTranscripts(path)
	if len(siblings) == 0 {
		return results, false, nil
	}
	// headResult is captured inside the search loop rather than re-indexed as
	// results[head] afterwards: the loop bound is what proves the index is in
	// range, so reading the element here keeps that proof local.
	head := -1
	var headResult ParseResult
	for i := range results {
		if results[i].Session.File.Path == path &&
			strings.HasPrefix(results[i].Session.ID, "agent-") {
			head = i
			headResult = results[i]
			break
		}
	}
	if head < 0 {
		return results, false, nil
	}

	members := []claudeSubagentChainMember{{path: path, result: headResult}}
	for _, sibling := range siblings {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		siblingOpts := opts
		// Sibling lineage resolution is a project-level transcript concern; a
		// sub-agent transcript has no replayed project sibling to resolve.
		siblingOpts.siblingLineage = false
		siblingResults, _, err := claudeParseFile(
			sibling, project, machine, siblingOpts,
		)
		if err != nil {
			// A companion that cannot be read leaves the session as parsed
			// rather than failing the thread that can be stored.
			continue
		}
		for _, siblingResult := range siblingResults {
			if siblingResult.Session.ID != headResult.Session.ID {
				// Entries of another session inside a companion transcript are
				// not this session's to claim.
				continue
			}
			members = append(members, claudeSubagentChainMember{
				path: sibling, result: siblingResult,
			})
		}
	}
	if len(members) == 1 {
		return results, false, nil
	}

	slices.SortStableFunc(members, func(a, b claudeSubagentChainMember) int {
		if !a.result.Session.StartedAt.Equal(b.result.Session.StartedAt) {
			if a.result.Session.StartedAt.Before(b.result.Session.StartedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})

	joined := headResult
	joined.Messages = nil
	joined.Session.MessageCount = 0
	joined.Session.UserMessageCount = 0
	joined.Session.MalformedLines = 0
	joined.Session.TotalOutputTokens = 0
	joined.Session.PeakContextTokens = 0
	joined.Session.HasTotalOutputTokens = false
	joined.Session.HasPeakContextTokens = false
	// A continued session's stored transcript ends in a companion, so no
	// resume point inside the parsed file describes the end of its messages.
	joined.Checkpoint = nil
	joined.CheckpointHashState = nil
	joined.CheckpointAnchorDigest = ""

	accepted := 0
	for _, member := range members {
		if accepted > 0 &&
			!member.result.Session.StartedAt.After(joined.Session.EndedAt) {
			// The guard: a run that had already started when the previous run
			// was still going is not the same dispatch continuing. Leave it out
			// and leave its file unexplained.
			if member.path == path {
				// The transcript being parsed must always be stored, so a
				// refusal that would drop it refuses the whole join instead.
				return results, false, nil
			}
			continue
		}
		if accepted == 0 {
			joined.Session.StartedAt = member.result.Session.StartedAt
			joined.Session.FirstMessage = member.result.Session.FirstMessage
		}
		base := 0
		if len(joined.Messages) > 0 {
			base = joined.Messages[len(joined.Messages)-1].Ordinal + 1
		}
		messages := slices.Clone(member.result.Messages)
		for i := range messages {
			messages[i].Ordinal += base
		}
		joined.Messages = append(joined.Messages, messages...)
		joined.UsageEvents = append(
			joined.UsageEvents, member.result.UsageEvents...,
		)
		joined.Session.EndedAt = member.result.Session.EndedAt
		joined.Session.MessageCount += member.result.Session.MessageCount
		joined.Session.UserMessageCount += member.result.Session.UserMessageCount
		joined.Session.MalformedLines += member.result.Session.MalformedLines
		joined.Session.TotalOutputTokens += member.result.Session.TotalOutputTokens
		if member.result.Session.PeakContextTokens >
			joined.Session.PeakContextTokens {
			joined.Session.PeakContextTokens =
				member.result.Session.PeakContextTokens
		}
		joined.Session.HasTotalOutputTokens = joined.Session.HasTotalOutputTokens ||
			member.result.Session.HasTotalOutputTokens
		joined.Session.HasPeakContextTokens = joined.Session.HasPeakContextTokens ||
			member.result.Session.HasPeakContextTokens
		joined.Session.IsTruncated = member.result.Session.IsTruncated
		if member.result.Session.TerminationStatus != "" {
			joined.Session.TerminationStatus =
				member.result.Session.TerminationStatus
		}
		accepted++
	}
	if accepted < 2 {
		return results, false, nil
	}
	out := slices.Clone(results)
	out[head] = joined
	return out, true, nil
}
