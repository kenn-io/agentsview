package why

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/service"
)

// Lookup is the shared archive operation, supplied lazily by the CLI.
type Lookup func(context.Context, []string) (map[string][]service.CommitSessionLink, error)

// Ref holds a service card or a literal named issue/document.
type Ref struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	How     string `json:"how"`
	FoundBy string `json:"found_by"`
	Card    any    `json:"card"`
}

// Step records the bounded work performed for this question.
type Step struct {
	Tool  string  `json:"tool"`
	Asked string  `json:"asked"`
	Found int     `json:"found"`
	MS    int64   `json:"ms"`
	Error *string `json:"error"`
}

// Result is the public why JSON contract; rendering context stays private.
type Result struct {
	Question              Question `json:"question"`
	Lines                 []Line   `json:"lines"`
	Refs                  []Ref    `json:"refs"`
	Steps                 []Step   `json:"steps"`
	Older                 int      `json:"older"`
	Explained             bool     `json:"explained"`
	commits               []Commit
	associations          map[string][]int
	ambiguousAssociations map[string]map[int]bool
}
type issueCard struct {
	Issue string `json:"issue"`
}
type pathCard struct {
	Path string `json:"path"`
}

// Explain joins local evidence to stored records without initiating source work.
func Explain(ctx context.Context, in *Inspection, lookup Lookup) (*Result, error) {
	r := &Result{Question: in.Question, Lines: append([]Line{}, in.Lines...), Refs: []Ref{}, Steps: []Step{}, Older: in.Older, commits: in.Commits, associations: map[string][]int{}, ambiguousAssociations: map[string]map[int]bool{}}
	asked := fmt.Sprintf("%s:%d-%d at %s", in.Question.Path, intValue(in.Question.Start), intValue(in.Question.End), in.Question.Rev)
	found := len(in.Lines)
	if in.Question.Commit != "" {
		asked = "commit " + in.Question.Commit
		found = 1
	}
	r.Steps = append(r.Steps, Step{Tool: "blame", Asked: asked, Found: found, MS: in.Milliseconds})
	shas := make([]string, 0, len(in.Commits))
	texts := []string{}
	for _, c := range in.Commits {
		shas = append(shas, c.SHA)
		texts = append(texts, c.Message)
	}
	start := time.Now()
	links := map[string][]service.CommitSessionLink{}
	lookupAsked := "skipped: no committed lines"
	if len(shas) > 0 {
		var err error
		links, err = lookup(ctx, shas)
		if err != nil {
			return nil, fmt.Errorf("read commit sessions (check archive connectivity or upgrade/backfill the commit index): %w", err)
		}
		lookupAsked = strings.Join(shas, ", ")
	}
	bySession := map[string]int{}
	pairs := map[string]map[string]bool{}
	for _, c := range in.Commits {
		admitted := map[string]bool{}
		for _, link := range links[c.SHA] {
			if link.SessionID == "" {
				return nil, fmt.Errorf("archive returned a commit link without a session ID")
			}
			if !admitted[link.SessionID] {
				if len(admitted) >= 3 {
					continue
				}
				admitted[link.SessionID] = true
			}
			index, exists := bySession[link.SessionID]
			if !exists {
				index = len(r.Refs)
				bySession[link.SessionID] = index
				pairs[link.SessionID] = map[string]bool{}
				r.Refs = append(r.Refs, Ref{ID: fmt.Sprintf("r%d", index+1), Key: "session:" + link.SessionID, How: "direct", Card: link})
				if link.Session != nil && !link.Gone && !link.Ambiguous {
					r.Explained = true
				}
				if link.Session != nil && !link.Gone && link.Turn != nil {
					texts = append(texts, link.Turn.Prompt)
				}
			}
			if !containsIndex(r.associations[c.SHA], index) {
				r.associations[c.SHA] = append(r.associations[c.SHA], index)
			}
			if link.Ambiguous {
				if r.ambiguousAssociations[c.SHA] == nil {
					r.ambiguousAssociations[c.SHA] = map[int]bool{}
				}
				r.ambiguousAssociations[c.SHA][index] = true
			}
			pair := c.SHA + " " + link.Source
			ref := &r.Refs[index]
			sentence := fmt.Sprintf("Commit %s: %s evidence", c.SHA, link.Source)
			if link.Ambiguous {
				sentence += " (ambiguous evidence)"
			}
			if in.Question.Path != "" {
				sentence += fmt.Sprintf(" covering %d selected lines", c.Covered)
			}
			if ambiguous, exists := pairs[link.SessionID][pair]; exists {
				if link.Ambiguous && !ambiguous {
					pairs[link.SessionID][pair] = true
					ref.FoundBy = strings.Replace(ref.FoundBy, fmt.Sprintf("Commit %s: %s evidence", c.SHA, link.Source), sentence, 1)
				}
			} else if len(pairs[link.SessionID]) < 12 {
				pairs[link.SessionID][pair] = link.Ambiguous
				if ref.FoundBy != "" {
					ref.FoundBy += "; "
				}
				ref.FoundBy += sentence
			}
		}
	}
	r.Steps = append(r.Steps, Step{Tool: "commit_sessions", Asked: lookupAsked, Found: len(r.Refs), MS: time.Since(start).Milliseconds()})
	start = time.Now()
	named, err := FindNamedRefs(texts, func(path string) (bool, error) { return in.DocumentExists(ctx, path) })
	if err != nil {
		return nil, fmt.Errorf("check named documents: %w", err)
	}
	for _, n := range named {
		var card any = issueCard{Issue: n.Issue}
		if n.Path != "" {
			card = pathCard{Path: n.Path}
		}
		r.Refs = append(r.Refs, Ref{ID: fmt.Sprintf("r%d", len(r.Refs)+1), Key: n.Key, How: "named", FoundBy: "Named in a selected commit message or retained session prompt", Card: card})
	}
	r.Steps = append(r.Steps, Step{Tool: "refs", Asked: "selected commit messages and canonical session prompts; documents checked in the pinned tree", Found: len(named), MS: time.Since(start).Milliseconds()})
	return r, nil
}
func intValue(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
func containsIndex(indices []int, index int) bool {
	for _, i := range indices {
		if i == index {
			return true
		}
	}
	return false
}
