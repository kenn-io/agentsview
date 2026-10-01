package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

var _ db.ToolSequenceReadSource = (*HostedStore)(nil)

func (h *HostedStore) ToolSequenceReadSource(ctx context.Context, id string) (string, bool, error) {
	binding, err := hostedRead(ctx, h, func(revision hostedRevision) (string, error) {
		source, err := h.resolve(ctx, id)
		if err != nil || source.State == RawIdentityGone {
			return "", err
		}
		return fmt.Sprintf("%d:%d:%d:%t:%s", revision.Identity, revision.Selection, revision.Corpus, source.Legacy, source.SessionID), nil
	})
	if err == ErrHostedIdentityChanged {
		return "", true, nil
	}
	return binding, false, err
}
