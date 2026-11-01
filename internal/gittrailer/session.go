// Package gittrailer supplies optional Git commit session provenance.
package gittrailer

import (
	"errors"
	"regexp"
)

var (
	agentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	uuidPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Session retains the harness's exact native identifier, including its case.
type Session struct{ Agent, ID string }

// ValidAgent accepts Kata-compatible agent keys, including configured agents.
func ValidAgent(agent string) bool { return agentPattern.MatchString(agent) }

func (s Session) validate() error {
	validID := tokenPattern.MatchString(s.ID)
	if s.Agent == "claude" || s.Agent == "codex" {
		validID = uuidPattern.MatchString(s.ID)
	}
	if !ValidAgent(s.Agent) || !validID {
		return errors.New("invalid current session identifier")
	}
	return nil
}

// Discover follows Kata's close-transcript discovery rules: an explicit pair
// overrides discovery; otherwise every nonempty source must agree. No source
// is a silent absence. It never looks for transcripts or another session.
func Discover(getenv func(string) string, configured map[string]string) (Session, error) {
	for agent := range configured {
		if !ValidAgent(agent) {
			return Session{}, errors.New("invalid configured session agent key")
		}
	}
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
	for agent, name := range configured {
		variables = append(variables, struct{ agent, name string }{agent, name})
	}
	for _, v := range variables {
		id := getenv(v.name)
		if id == "" {
			continue
		}
		candidate := Session{v.agent, id}
		if current.ID != "" && current != candidate {
			return Session{}, errors.New("ambiguous current session; set the explicit override pair")
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
