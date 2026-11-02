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

func (s Session) validate() error {
	var validID bool
	if s.Agent == "claude" || s.Agent == "codex" {
		validID = uuidPattern.MatchString(s.ID)
	} else {
		validID = tokenPattern.MatchString(s.ID)
	}
	if !agentPattern.MatchString(s.Agent) || !validID {
		return errors.New("invalid current session identifier")
	}
	return nil
}

// Discover follows Kata's close-transcript discovery rules: an explicit pair
// overrides discovery; otherwise Claude and Codex together are ambiguous.
// Codex's current thread takes precedence over its root session. No source is
// a silent absence. It never looks for transcripts or another session.
func Discover(getenv func(string) string) (Session, error) {
	current := Session{getenv("AGENTSVIEW_SESSION_AGENT"), getenv("AGENTSVIEW_SESSION_ID")}
	if current.Agent != "" || current.ID != "" {
		if err := current.validate(); err != nil {
			return Session{}, err
		}
		return current, nil
	}
	codexID := getenv("CODEX_THREAD_ID")
	if codexID == "" {
		codexID = getenv("CODEX_SESSION_ID")
	}
	for _, candidate := range []Session{
		{"claude", getenv("CLAUDE_CODE_SESSION_ID")},
		{"codex", codexID},
	} {
		if candidate.ID == "" {
			continue
		}
		if current.ID != "" {
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
