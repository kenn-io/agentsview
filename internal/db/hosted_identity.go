package db

import (
	"context"
	"fmt"
)

// SessionIdentityError reports a public alias that cannot select one transcript.
// Variants contains public choices, never physical storage row identities.
type SessionIdentityError struct {
	State    string   `json:"state"`
	Variants []string `json:"variants,omitempty"`
}

func (e *SessionIdentityError) Error() string { return fmt.Sprintf("session identity is %s", e.State) }

// SessionWatchStateStore lets remote watches notice alias membership transitions.
type SessionWatchStateStore interface {
	GetSessionWatchState(id string) (SessionWatchState, error)
}

const SessionWatchResolved = "resolved"

type SessionWatchState struct {
	State            string   `json:"state"`
	PublicID         string   `json:"public_id,omitempty"`
	Variants         []string `json:"variants,omitempty"`
	IdentityRevision int64    `json:"identity_revision"`
	ContentRevision  string   `json:"content_revision,omitempty"`
}

// ProviderResumeIdentityStore converts a hosted alias to provider-owned resume identity.
type ProviderResumeIdentityStore interface {
	GetProviderResumeID(ctx context.Context, id string) (string, error)
}
