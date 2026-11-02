// Package gittrailer supplies optional Git commit session provenance.
package gittrailer

import (
	"errors"
	"regexp"
	"strings"
)

var (
	agentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	uuidPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Session retains the harness's exact native identifier, including its case.
type Session struct{ Agent, ID string }

func (s Session) validate() error {
	validID := tokenPattern.MatchString(s.ID)
	if s.Agent == "claude" || s.Agent == "codex" {
		validID = uuidPattern.MatchString(s.ID)
	}
	if !agentPattern.MatchString(s.Agent) || !validID {
		return errors.New("invalid current session identifier")
	}
	return nil
}

// Discover follows Kata's close-transcript discovery rules: an explicit pair
// overrides discovery; otherwise every nonempty source must agree. No source
// is a silent absence. It never looks for transcripts or another session.
func Discover(getenv func(string) string) (Session, error) {
	current := Session{getenv("AGENTSVIEW_SESSION_AGENT"), getenv("AGENTSVIEW_SESSION_ID")}
	if current.Agent != "" || current.ID != "" {
		if err := current.validate(); err != nil {
			return Session{}, err
		}
		return current, nil
	}
	variables := []struct{ agent, name string }{
		{"claude", "CLAUDE_CODE_SESSION_ID"},
		{"codex", "CODEX_THREAD_ID"},
		{"codex", "CODEX_SESSION_ID"},
	}
	for _, v := range variables {
		id := getenv(v.name)
		if id == "" {
			continue
		}
		candidate := Session{v.agent, id}
		if current.ID != "" {
			if current.Agent != candidate.Agent || !strings.EqualFold(current.ID, candidate.ID) {
				return Session{}, errors.New("ambiguous current session; set the explicit override pair")
			}
			continue
		}
		current = candidate
	}
	if current.ID == "" {
		return Session{}, nil
	}
	if err := current.validate(); err != nil {
		return Session{}, err
	}
	return current, nil
}
