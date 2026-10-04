package postgres

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.SessionSourceBinder = (*HostedStore)(nil)

// SessionSourceChanged reports a read that saw the hosted identity move.
func (h *HostedStore) SessionSourceChanged(err error) bool {
	return errors.Is(err, ErrHostedIdentityChanged)
}

// SessionSourceBinding names the physical session a hosted ID resolves to now.
func (h *HostedStore) SessionSourceBinding(ctx context.Context, id string) (string, error) {
	return hostedRead(ctx, h, func(revision hostedRevision) (string, error) {
		source, err := h.resolve(ctx, id)
		if err != nil || source.State == RawIdentityGone {
			return "", err
		}
		return fmt.Sprintf("%d:%d:%d:%t:%s", revision.Identity, revision.Selection, revision.Corpus, source.Legacy, source.SessionID), nil
	})
}
