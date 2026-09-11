package postgres

import (
	"context"
	"errors"
	"go.kenn.io/agentsview/internal/db"
	"slices"
	"strings"
)

func (h *HostedStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	return h.getSession(ctx, id, false)
}
func (h *HostedStore) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	return h.getSession(ctx, id, true)
}
func (h *HostedStore) getSession(ctx context.Context, id string, full bool) (*db.Session, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) (*db.Session, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			return nil, err
		}
		var s *db.Session
		if full {
			s, err = h.physical.GetSessionFull(ctx, target.SessionID)
		} else {
			s, err = h.physical.GetSession(ctx, target.SessionID)
		}
		if err != nil || s == nil {
			return s, err
		}
		out := *s
		r := hostedRefs{}
		r.session(&out)
		err = r.mapIDs(ctx, h)
		return &out, err
	})
}
func (h *HostedStore) ListSessions(ctx context.Context, f db.SessionFilter) (db.SessionPage, error) {
	return hostedRead(ctx, h, func(rev hostedRevision) (db.SessionPage, error) {
		q := f
		var err error
		q.Cursor, err = h.openCursor(f.Cursor, rev)
		if err != nil {
			return db.SessionPage{}, err
		}
		p, err := h.physical.ListSessions(ctx, q)
		if err != nil {
			return p, err
		}
		p.Sessions = slices.Clone(p.Sessions)
		r := hostedRefs{}
		for i := range p.Sessions {
			r.session(&p.Sessions[i])
		}
		if err = r.mapIDs(ctx, h); err != nil {
			return p, err
		}
		p.NextCursor, err = h.sealCursor(p.NextCursor, rev)
		return p, err
	})
}
func (h *HostedStore) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	return hostedRead(ctx, h, func(rev hostedRevision) (db.SidebarSessionIndex, error) {
		q := f
		var err error
		q.Cursor, err = h.openCursor(f.Cursor, rev)
		if err != nil {
			return db.SidebarSessionIndex{}, err
		}
		p, err := h.physical.GetSidebarSessionIndex(ctx, q)
		if err != nil {
			return p, err
		}
		p.Sessions = slices.Clone(p.Sessions)
		r := hostedRefs{}
		for i := range p.Sessions {
			r.add(&p.Sessions[i].ID)
			r.parent(p.Sessions[i].ID, &p.Sessions[i].ParentSessionID, &p.Sessions[i].ParentSessionIDs)
		}
		if err = r.mapIDs(ctx, h); err != nil {
			return p, err
		}
		p.NextCursor, err = h.sealCursor(p.NextCursor, rev)
		return p, err
	})
}
func (h *HostedStore) GetChildSessions(ctx context.Context, id string) ([]db.Session, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) ([]db.Session, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			return nil, err
		}
		var children []db.Session
		if target.Legacy {
			children, err = h.physical.GetChildSessions(ctx, target.SessionID)
		} else {
			rows, e := h.physical.pg.QueryContext(ctx, `SELECT `+pgSessionCols+` FROM sessions WHERE id IN (SELECT owner.session_id `+hostedLinkFromSQL+` AND e.kind='parent' AND target.session_id=$1) AND deleted_at IS NULL ORDER BY COALESCE(started_at,created_at),id`, target.SessionID)
			if e != nil {
				return nil, e
			}
			children, err = scanPGSessionRows(rows)
			rows.Close()
		}
		s := children
		if err != nil {
			return nil, err
		}
		s = slices.Clone(s)
		r := hostedRefs{}
		for i := range s {
			s[i].ParentSessionID = nil
			r.session(&s[i])
		}
		err = r.mapIDs(ctx, h)
		for i := range s {
			parent := target.PublicID
			s[i].ParentSessionID = &parent
		}
		return s, err
	})
}
func (h *HostedStore) FindSessionIDsByPartial(ctx context.Context, partial string, limit int) ([]string, error) {
	return h.findAliases(ctx, partial, limit, false)
}
func (h *HostedStore) FindSessionIDsByRawSuffix(ctx context.Context, suffix string, limit int) ([]string, error) {
	return h.findAliases(ctx, suffix, limit, true)
}
func (h *HostedStore) findAliases(ctx context.Context, term string, limit int, suffix bool) ([]string, error) {
	if limit <= 0 || limit > db.MaxSessionLimit {
		limit = db.MaxSessionLimit
	}
	return hostedRead(ctx, h, func(_ hostedRevision) ([]string, error) {
		predicate := `strpos(a.alias_id,$1)>0`
		if suffix {
			predicate = `right(a.alias_id,length($1))=$1`
		}
		rows, err := h.physical.pg.QueryContext(ctx, `SELECT DISTINCT s.id FROM raw_session_public_aliases a JOIN raw_session_branches b ON b.group_id=a.group_id AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id) JOIN sessions s ON s.id=b.session_id WHERE b.active AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false) AND `+predicate+` UNION SELECT id FROM sessions WHERE provenance_kind='legacy' AND `+func() string {
			if suffix {
				return `right(id,length($1))=$1`
			}
			return `strpos(id,$1)>0`
		}()+` ORDER BY 1 LIMIT $2`, term, limit)
		if err != nil {
			return nil, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		r := hostedRefs{}
		for i := range ids {
			r.add(&ids[i])
		}
		err = r.mapIDs(ctx, h)
		slices.Sort(ids)
		return slices.Compact(ids), err
	})
}
func (h *HostedStore) GetMessages(ctx context.Context, id string, from, limit int, asc bool) ([]db.Message, error) {
	return h.readMessages(ctx, id, func(p string) ([]db.Message, error) { return h.physical.GetMessages(ctx, p, from, limit, asc) })
}
func (h *HostedStore) GetMessagesWindow(ctx context.Context, id string, w db.MessageWindow) ([]db.Message, error) {
	return h.readMessages(ctx, id, func(p string) ([]db.Message, error) { return h.physical.GetMessagesWindow(ctx, p, w) })
}
func (h *HostedStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	return h.readMessages(ctx, id, func(p string) ([]db.Message, error) { return h.physical.GetAllMessages(ctx, p) })
}
func (h *HostedStore) readMessages(ctx context.Context, id string, read func(string) ([]db.Message, error)) ([]db.Message, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) ([]db.Message, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			return nil, err
		}
		msgs, err := read(target.SessionID)
		if err != nil {
			return nil, err
		}
		r := hostedRefs{}
		msgs = r.messages(msgs)
		err = r.mapIDs(ctx, h)
		return msgs, err
	})
}
func hostedDetail[T any](ctx context.Context, h *HostedStore, id string, read func(string) (T, error)) (T, error) {
	return hostedRead(ctx, h, func(_ hostedRevision) (T, error) {
		target, err := h.resolve(ctx, id)
		if err != nil || target.SessionID == "" {
			var zero T
			return zero, err
		}
		return read(target.SessionID)
	})
}
func (h *HostedStore) GetResumeModelCounts(ctx context.Context, id string) ([]db.ModelCount, error) {
	return hostedDetail(ctx, h, id, func(p string) ([]db.ModelCount, error) { return h.physical.GetResumeModelCounts(ctx, p) })
}
func (h *HostedStore) GetSessionActivity(ctx context.Context, id string) (*db.SessionActivityResponse, error) {
	return hostedDetail(ctx, h, id, func(p string) (*db.SessionActivityResponse, error) { return h.physical.GetSessionActivity(ctx, p) })
}
func (h *HostedStore) SearchSession(ctx context.Context, id, query string) ([]int, error) {
	return hostedDetail(ctx, h, id, func(p string) ([]int, error) { return h.physical.SearchSession(ctx, p, query) })
}

func (h *HostedStore) GetProviderResumeID(ctx context.Context, id string) (string, error) {
	session, err := h.GetSessionFull(ctx, id)
	if err != nil {
		return "", err
	}
	if session == nil {
		return "", &db.SessionIdentityError{State: "gone"}
	}
	if session.SourceSessionID != "" {
		return session.SourceSessionID, nil
	}
	target, err := h.resolve(ctx, id)
	if err != nil {
		return "", err
	}
	if target.Legacy {
		return strings.TrimPrefix(target.SessionID, session.Agent+":"), nil
	}
	return "", errors.New("provider resume identity is unavailable")
}
