package db

import "errors"

const (
	MaxCommitSHAs          = 200
	MaxCommitLinks         = 4096
	MaxCommitResponseBytes = 8 << 20
)

var (
	ErrInvalidCommitQuery     = errors.New("commit lookup requires at most 200 hexadecimal SHAs of 7 to 40 characters")
	ErrCommitLookupTooLarge   = errors.New("commit lookup exceeds result limits")
	ErrCommitIndexUnavailable = errors.New("commit index unavailable; upgrade and run agentsview commits backfill")
)

// CommitSession is the availability marker for a recorded session.
type CommitSession struct {
	_  struct{} `json:"-" nullable:"true"`
	ID string   `json:"id"`
}

// CommitTurn is prompt context for a recorded commit call or a labelled heuristic.
// It does not prove authorship of every staged change.
type CommitTurn struct {
	_       struct{} `json:"-" nullable:"true"`
	Ordinal int      `json:"ordinal"`
	At      string   `json:"at"`
	Prompt  string   `json:"prompt"`
	Why     string   `json:"why" enum:"commit-call,wrote,before,match"`
}

// CommitLink is one observation connecting a commit to an agent session.
// Coordinates identify the transcript call; the turn identifies its user prompt.
// Unknown and deleted sessions retain commit facts, without session text.
type CommitLink struct {
	SessionID        string         `json:"session_id"`
	CommitSHA        string         `json:"commit_sha"`
	Resolved         bool           `json:"resolved"`
	Source           string         `json:"source" enum:"transcript,trailer"`
	MessageOrdinal   *int           `json:"message_ordinal"`
	CallIndex        *int           `json:"call_index"`
	RepositoryPath   string         `json:"repository_path"`
	Branch           string         `json:"branch"`
	Subject          string         `json:"subject"`
	CommittedAt      string         `json:"committed_at"`
	Ambiguous        bool           `json:"ambiguous"`
	Agent            string         `json:"agent"`
	Project          string         `json:"project"`
	SessionStartedAt string         `json:"session_started_at"`
	WebURL           string         `json:"web_url"`
	Session          *CommitSession `json:"session"`
	Gone             bool           `json:"gone"`
	Turn             *CommitTurn    `json:"turn"`
}
