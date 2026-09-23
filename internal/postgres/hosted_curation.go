package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/agentsview/internal/db"
)

func hostedCurationError(err error) error {
	if e, ok := errors.AsType[*RawIdentityError](err); ok {
		return &db.SessionIdentityError{State: string(e.Identity.State), Variants: e.Identity.Variants}
	}
	return err
}

func (h *HostedStore) curate(ctx context.Context, alias, field string, value any) error {
	target, err := h.resolve(ctx, alias)
	if err != nil {
		return err
	}
	if target.Legacy {
		_, err = h.legacyWrite(ctx, alias, target.SessionID, func(tx *sql.Tx, id string) (int64, error) { return legacyCurationTx(ctx, tx, id, field, value) })
		return err
	}
	return hostedCurationError(h.core.SetCuration(ctx, alias, field, value))
}

func (h *HostedStore) RenameSession(ctx context.Context, id string, name *string) error {
	var value any
	if name != nil {
		value = *name
	}
	return h.curate(ctx, id, "display_name", value)
}

func (h *HostedStore) SoftDeleteSession(ctx context.Context, id string) error {
	return h.curate(ctx, id, "trashed", true)
}

func (h *HostedStore) RestoreSession(ctx context.Context, id string) (int64, error) {
	target, err := h.resolve(ctx, id)
	if err != nil {
		return 0, err
	}
	if target.Legacy {
		return h.legacyWrite(ctx, id, target.SessionID, func(tx *sql.Tx, p string) (int64, error) {
			return legacyCurationTx(ctx, tx, p, "trashed", false)
		})
	}
	err = hostedCurationError(h.core.SetCuration(ctx, id, "trashed", false))
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func (h *HostedStore) StarSession(ctx context.Context, id string) (bool, error) {
	err := h.curate(ctx, id, "starred", true)
	return err == nil, err
}

func (h *HostedStore) UnstarSession(ctx context.Context, id string) error {
	return h.curate(ctx, id, "starred", false)
}

func (h *HostedStore) BulkStarSessions(ctx context.Context, ids []string) error {
	if _, err := h.resolveBatch(ctx, ids, false, true); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := h.StarSession(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *HostedStore) SoftDeleteSessions(ctx context.Context, ids []string) (int, error) {
	if _, err := h.resolveBatch(ctx, ids, false, true); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := h.SoftDeleteSession(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (h *HostedStore) ListStarredSessionIDs(ctx context.Context) ([]string, error) {
	return hostedMapped(ctx, h, func() ([]string, error) { return h.physical.ListStarredSessionIDs(ctx) }, func(p *[]string, r *hostedRefs) {
		*p = slices.Clone(*p)
		for i := range *p {
			r.add(&(*p)[i])
		}
	})
}

func (h *HostedStore) ListTrashedSessions(ctx context.Context) ([]db.Session, error) {
	return hostedMapped(ctx, h, func() ([]db.Session, error) { return h.physical.ListTrashedSessions(ctx) }, func(p *[]db.Session, r *hostedRefs) {
		*p = slices.Clone(*p)
		for i := range *p {
			r.session(&(*p)[i])
		}
	})
}

func (h *HostedStore) DeleteSessionIfTrashed(ctx context.Context, id string) (int64, error) {
	target, err := h.resolve(ctx, id)
	if err != nil {
		return 0, err
	}
	if target.Legacy {
		return h.legacyWrite(ctx, id, target.SessionID, func(tx *sql.Tx, p string) (int64, error) { return deleteLegacyTrashedTx(ctx, tx, p) })
	}
	deleted, err := h.core.ExcludeTrashedSession(ctx, id)
	if err != nil {
		return 0, hostedCurationError(err)
	}
	if deleted {
		return 1, nil
	}
	return 0, nil
}

func (h *HostedStore) EmptyTrash(ctx context.Context) (int, error) {
	n, err := h.core.EmptyTrash(ctx)
	if err != nil {
		return n, err
	}
	legacy, err := h.physical.emptyTrash(ctx, true)
	return n + legacy, err
}

func (h *HostedStore) PinMessage(ctx context.Context, id string, ordinal int64, note *string) (int64, error) {
	target, err := h.resolve(ctx, id)
	if err != nil {
		return 0, err
	}
	if target.Legacy {
		return h.legacyWrite(ctx, id, target.SessionID, func(tx *sql.Tx, p string) (int64, error) { return pinMessageTx(ctx, tx, p, ordinal, note) })
	}
	text := ""
	if note != nil {
		text = *note
	}
	pinID, err := h.core.SetPinReturningID(ctx, id, int(ordinal), true, text)
	return pinID, hostedCurationError(err)
}

func (h *HostedStore) UnpinMessage(ctx context.Context, id string, ordinal int64) error {
	target, err := h.resolve(ctx, id)
	if err != nil {
		return err
	}
	if target.Legacy {
		_, err = h.legacyWrite(ctx, id, target.SessionID, func(tx *sql.Tx, p string) (int64, error) {
			res, e := tx.ExecContext(ctx, `DELETE FROM pinned_messages WHERE session_id=$1 AND message_id=$2`, p, ordinal)
			if e != nil {
				return 0, e
			}
			return res.RowsAffected()
		})
		return err
	}
	return hostedCurationError(h.core.SetPin(ctx, id, int(ordinal), false, ""))
}

func (h *HostedStore) RemovePinReference(ctx context.Context, id, key string) error {
	target, err := h.resolve(ctx, id)
	if err != nil {
		return err
	}
	if target.Legacy {
		return db.ErrReadOnly
	}
	return hostedCurationError(h.core.RemovePin(ctx, id, key))
}

func (h *HostedStore) ListPinnedMessages(ctx context.Context, id, project string) ([]db.PinnedMessage, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) ([]db.PinnedMessage, error) {
		physical := ""
		var target hostedIdentity
		var err error
		if id != "" {
			target, err = h.resolve(ctx, id)
			if err != nil || target.SessionID == "" {
				return nil, err
			}
			physical = target.SessionID
		}
		pins, err := h.core.pinReferencePage(ctx, physical, project)
		if err != nil {
			return nil, err
		}
		r := hostedRefs{}
		for i := range pins {
			r.add(&pins[i].SessionID)
		}
		if err = r.mapIDs(ctx, h); err != nil {
			return nil, err
		}
		return pins, nil
	})
}

func (h *HostedStore) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	target, err := h.resolve(ctx, id)
	if err != nil || target.SessionID == "" {
		return 0, 0, false
	}
	return h.physical.GetSessionVersion(ctx, target.SessionID)
}

func (h *HostedStore) GetSessionWatchState(id string) (db.SessionWatchState, error) {
	return hostedRead(context.Background(), h, func(r hostedRevision) (db.SessionWatchState, error) {
		target, err := h.resolve(context.Background(), id)
		if conflict, ok := errors.AsType[*db.SessionIdentityError](err); ok {
			return db.SessionWatchState{State: conflict.State, Variants: conflict.Variants, IdentityRevision: r.Identity}, nil
		}
		if err != nil {
			return db.SessionWatchState{}, err
		}
		state := string(target.State)
		if target.State == RawIdentityUnique {
			state = db.SessionWatchResolved
		}
		return db.SessionWatchState{State: state, PublicID: target.PublicID, IdentityRevision: r.Identity, ContentRevision: target.ContentRevision}, nil
	})
}

// Recheck public legacy collisions inside the core's existing group lock and
// write transaction. Legacy membership writes take the same lock in their trigger.
func (h *HostedStore) guardCurationIdentity(ctx context.Context, tx *sql.Tx, alias string, target RawIdentity) error {
	var legacy string
	err := tx.QueryRowContext(ctx, `SELECT hosted_legacy_alias(id) FROM sessions WHERE provenance_kind='legacy' AND (id=$1 OR hosted_legacy_alias(id)=$1)`, alias).Scan(&legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	variants := append(slices.Clone(target.Variants), legacy)
	slices.Sort(variants)
	return &db.SessionIdentityError{State: "ambiguous", Variants: variants}
}
