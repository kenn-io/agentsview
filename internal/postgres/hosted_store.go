package postgres

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// HostedStore is the explicit public identity boundary around physical queries.
// It deliberately does not embed Store: new capabilities require a mapping choice.
type HostedStore struct {
	physical  *Store
	core      *RawProjectionStore
	tenant    string
	cursorMu  sync.RWMutex
	cursorKey [32]byte
}

var _ db.Store = (*HostedStore)(nil)

type hostedRevision struct{ Identity, Selection, Corpus int64 }

var ErrHostedIdentityChanged = errors.New("hosted identity changed during read; retry the request")
var ErrHostedCursor = errors.New("invalid or expired hosted cursor")

func newHostedAdapter(pg *sql.DB, tenant string) (*HostedStore, error) {
	var revisionTrigger bool
	if err := pg.QueryRowContext(context.Background(), `SELECT EXISTS(
 SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_language l ON l.oid=p.prolang
 WHERE t.tgrelid='sessions'::regclass AND t.tgname='hosted_legacy_revision'
 AND t.tgenabled='O' AND t.tgtype=29 AND t.tgqual IS NULL AND t.tgnargs=0
 AND t.tgattr=''::int2vector AND NOT t.tgisinternal AND t.tgconstraint=0
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred
 AND p.oid='hosted_legacy_revision()'::regprocedure
 AND p.pronamespace=(SELECT relnamespace FROM pg_class WHERE oid='sessions'::regclass)
 AND NOT p.prosecdef AND p.proconfig IS NULL AND p.prosrc=$1 AND p.provolatile='v'
 AND l.lanname='plpgsql'
) AND EXISTS(
 SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
 WHERE p.oid='hosted_legacy_alias(text)'::regprocedure AND NOT p.prosecdef
 AND p.proconfig IS NULL AND p.prosrc=$2 AND p.provolatile='i' AND p.proisstrict
 AND l.lanname='sql'
)`, hostedLegacyRevisionBody, hostedLegacyAliasBody).Scan(&revisionTrigger); err != nil {
		return nil, err
	}
	if !revisionTrigger {
		return nil, errors.New("hosted identity revision protection is missing or altered; owner must reprovision the schema")
	}
	core, err := NewRawProjectionStore(pg, RawProjectionOptions{Tenant: tenant})
	if err != nil {
		return nil, err
	}
	h := &HostedStore{physical: &Store{pg: pg, hostedRelations: true}, core: core, tenant: tenant}
	core.curationGuard = h.guardCurationIdentity
	if _, err = rand.Read(h.cursorKey[:]); err != nil {
		return nil, err
	}
	return h, nil
}
func (h *HostedStore) DB() *sql.DB  { return h.physical.DB() }
func (h *HostedStore) Close() error { return h.physical.Close() }
func (h *HostedStore) SetCustomPricing(p map[string]config.CustomModelRate) {
	h.physical.SetCustomPricing(p)
}
func (h *HostedStore) InsightGenerationAvailable() bool {
	return h.physical.InsightGenerationAvailable()
}
func (h *HostedStore) DetectInsightGenerationAvailability(ctx context.Context) error {
	return h.physical.DetectInsightGenerationAvailability(ctx)
}
func (h *HostedStore) revision(ctx context.Context) (hostedRevision, error) {
	var r hostedRevision
	err := h.physical.pg.QueryRowContext(ctx, `SELECT identity_revision,selection_revision,corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&r.Identity, &r.Selection, &r.Corpus)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return r, err
}

// Read failures are also revision checked: a disappearing physical row during
// convergence must retry resolution, not report an accidental not-found result.
func hostedRead[T any](ctx context.Context, h *HostedStore, read func(hostedRevision) (T, error)) (T, error) {
	var zero T
	for range 3 {
		before, err := h.revision(ctx)
		if err != nil {
			return zero, err
		}
		value, readErr := read(before)
		after, err := h.revision(ctx)
		if err != nil {
			return zero, err
		}
		if before == after {
			return value, readErr
		}
	}
	return zero, ErrHostedIdentityChanged
}
func (h *HostedStore) SetCursorSecret(secret []byte) {
	h.cursorMu.Lock()
	h.cursorKey = sha256.Sum256(secret)
	h.cursorMu.Unlock()
	h.physical.SetCursorSecret(secret)
}
func (h *HostedStore) cipher() (cipher.AEAD, error) {
	h.cursorMu.RLock()
	key := h.cursorKey
	h.cursorMu.RUnlock()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (h *HostedStore) sealCursor(payload string, r hostedRevision) (string, error) {
	if payload == "" {
		return "", nil
	}
	c, err := h.cipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	aad := fmt.Sprintf("hosted-v1:%s:%d:%d", h.tenant, r.Selection, r.Identity)
	return base64.RawURLEncoding.EncodeToString(c.Seal(append([]byte{1}, nonce...), nonce, []byte(payload), []byte(aad))), nil
}
func (h *HostedStore) openCursor(token string, r hostedRevision) (string, error) {
	if token == "" {
		return "", nil
	}
	if len(token) > 32768 {
		return "", ErrHostedCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", ErrHostedCursor
	}
	c, err := h.cipher()
	if err != nil {
		return "", err
	}
	if len(b) < 1+c.NonceSize()+c.Overhead() || b[0] != 1 {
		return "", ErrHostedCursor
	}
	aad := fmt.Sprintf("hosted-v1:%s:%d:%d", h.tenant, r.Selection, r.Identity)
	p, err := c.Open(nil, b[1:1+c.NonceSize()], b[1+c.NonceSize():], []byte(aad))
	if err != nil {
		return "", ErrHostedCursor
	}
	return string(p), nil
}

// Public cursor helpers accept public coordinates only; physical cursor work
// stays inside ListSessions/GetSidebarSessionIndex.
func (h *HostedStore) EncodeCursor(c db.SessionCursor) string {
	token, err := hostedRead(context.Background(), h, func(r hostedRevision) (string, error) {
		id, err := h.resolve(context.Background(), c.ID)
		if err != nil {
			return "", err
		}
		if id.SessionID == "" {
			return "", ErrHostedCursor
		}
		physical := c
		physical.ID = id.SessionID
		return h.sealCursor(h.physical.EncodeCursor(physical), r)
	})
	if err != nil {
		return ""
	}
	return token
}
func (h *HostedStore) DecodeCursor(token string) (db.SessionCursor, error) {
	return hostedRead(context.Background(), h, func(r hostedRevision) (db.SessionCursor, error) {
		p, err := h.openCursor(token, r)
		if err != nil {
			return db.SessionCursor{}, err
		}
		c, err := h.physical.DecodeCursor(p)
		if err != nil {
			return c, err
		}
		refs := hostedRefs{}
		refs.add(&c.ID)
		err = refs.mapIDs(context.Background(), h)
		return c, err
	})
}
func (h *HostedStore) EncodeActivityReportToken(payload []byte) (string, error) {
	r, err := h.revision(context.Background())
	if err != nil {
		return "", err
	}
	return h.sealCursor(string(payload), r)
}
func (h *HostedStore) DecodeActivityReportToken(token string) ([]byte, error) {
	r, err := h.revision(context.Background())
	if err != nil {
		return nil, err
	}
	p, err := h.openCursor(token, r)
	return []byte(p), err
}

func (h *HostedStore) SetVectorSearcher(searcher db.VectorSearcher) {
	h.physical.SetVectorSearcher(searcher)
}
func (h *HostedStore) SetSemanticUnavailableReason(reason string) {
	h.physical.SetSemanticUnavailableReason(reason)
}
