package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const HostedEmbeddingEncoding = "halfvec-normalized-v1"

var (
	ErrHostedEmbeddingStale          = errors.New("hosted embedding source stale")
	ErrHostedEmbeddingLeaseLost      = errors.New("hosted embedding lease lost")
	ErrHostedEmbeddingUnprovisioned  = errors.New("hosted embeddings not provisioned")
	ErrHostedEmbeddingWorkLimit      = errors.New("hosted embedding work limit exceeded")
	ErrHostedEmbeddingInvalidResults = errors.New("hosted embedding results invalid")
)

type HostedEmbeddingRecipe struct {
	Fingerprint       string
	ProfileName       string
	Model             string
	Dimensions        int
	BuilderVersion    string
	ChunkerVersion    string
	MaxInputChars     int
	ChunkOverlapChars int
	DocumentPrefix    string
	QueryPrefix       string
	InputSuffix       string
	RequestDimensions bool
	IncludeAutomated  bool
	EncodingType      string
	CorpusScope       string
}

// CanonicalHostedEmbeddingRecipe validates vector identity independently of the
// named transport profile. Credentials and endpoints must never enter this value.
func CanonicalHostedEmbeddingRecipe(r HostedEmbeddingRecipe) (HostedEmbeddingRecipe, error) {
	if r.Model == "" || r.ProfileName == "" || r.Dimensions < 1 || r.Dimensions > 4000 || r.BuilderVersion == "" || r.ChunkerVersion == "" || r.CorpusScope == "" || r.MaxInputChars < 1 || r.MaxInputChars > 64<<20 || r.ChunkOverlapChars < 0 || r.ChunkOverlapChars >= r.MaxInputChars || r.EncodingType != HostedEmbeddingEncoding {
		return r, fmt.Errorf("invalid hosted embedding recipe")
	}
	if len(r.Model)+len(r.ProfileName)+len(r.BuilderVersion)+len(r.ChunkerVersion)+len(r.CorpusScope)+len(r.DocumentPrefix)+len(r.QueryPrefix)+len(r.InputSuffix) > 65536 {
		return r, ErrHostedEmbeddingWorkLimit
	}
	identity := r
	identity.Fingerprint = ""
	identity.ProfileName = ""
	b, _ := json.Marshal(identity)
	h := embeddingHash(b)
	if r.Fingerprint != "" && r.Fingerprint != h {
		return r, fmt.Errorf("hosted embedding recipe fingerprint mismatch")
	}
	r.Fingerprint = h
	return r, nil
}
func embeddingHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type HostedEmbeddingProvisionOptions struct {
	ActivationMode string
}

type HostedEmbeddingGeneration struct {
	ActivationMode string
	ID             int64
	InstanceKey    string
	Recipe         HostedEmbeddingRecipe
}
type HostedEmbeddingLease struct {
	GenerationID           int64
	SessionID              string
	Revision, AttemptFence int64
	Owner, Token           string
	ExpiresAt              time.Time
}
type HostedEmbeddingChunk struct {
	Index           int
	Text, InputHash string
}
type HostedEmbeddingDocument struct {
	Key         string
	Unit        db.EmbeddableUnit
	ContentHash string
	Chunks      []HostedEmbeddingChunk
}
type HostedEmbeddingSnapshot struct {
	Generation   HostedEmbeddingGeneration
	Lease        HostedEmbeddingLease
	Eligible     bool
	Documents    []HostedEmbeddingDocument
	ManifestHash string
}
type HostedEmbeddingVector struct {
	DocumentKey string
	ChunkIndex  int
	Values      []float32
}
type HostedEmbeddingLimits struct {
	MaxUnitBytes, MaxSnapshotBytes                    int64
	MaxDocuments, MaxChunks                           int
	MaxVectorBytes, MaxOccurrenceBytes, MaxSpillBytes int64
	RowPageSize                                       int
	SnapshotTimeout                                   time.Duration
	MaxRows                                           int
}
type HostedEmbeddingOptions struct {
	Schema, Tenant string
	Limits         HostedEmbeddingLimits
	MaxAttempts    int
	RetryDelay     time.Duration
}
type HostedEmbeddingReconcileResult struct {
	Outbox, Dirty, Backfill int
	BackfillFinished        bool
}
type HostedEmbeddingGenerationStatus struct {
	Generation                             HostedEmbeddingGeneration
	BackfillFinished                       bool
	BackfillAfterSessionID                 string
	Ready, Leased, Retry, Failed, Complete int64
	Errors                                 map[string]int64
}
type HostedEmbeddingStatus struct {
	Active, Desired *HostedEmbeddingGenerationStatus
	ActivationReady bool
	ActiveAvailable bool
}
type HostedEmbeddingStore struct {
	pg             *sql.DB
	schema, tenant string
	limits         HostedEmbeddingLimits
	maxAttempts    int
	retryDelay     time.Duration
}

func embeddingLimits(v HostedEmbeddingLimits) (HostedEmbeddingLimits, error) {
	max := HostedEmbeddingLimits{64 << 20, 128 << 20, 32768, 65536, 128 << 20, 1 << 20, 128 << 20, 512, 30 * time.Second, 1000000}
	a := []*int64{&v.MaxUnitBytes, &v.MaxSnapshotBytes, &v.MaxVectorBytes, &v.MaxOccurrenceBytes, &v.MaxSpillBytes}
	b := []int64{max.MaxUnitBytes, max.MaxSnapshotBytes, max.MaxVectorBytes, max.MaxOccurrenceBytes, max.MaxSpillBytes}
	for i, p := range a {
		if *p == 0 {
			*p = b[i]
		}
		if *p < 0 || *p > b[i] {
			return v, ErrHostedEmbeddingWorkLimit
		}
	}
	for _, pair := range [][2]*int{{&v.MaxDocuments, &max.MaxDocuments}, {&v.MaxChunks, &max.MaxChunks}, {&v.RowPageSize, &max.RowPageSize}, {&v.MaxRows, &max.MaxRows}} {
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
		if *pair[0] < 0 || *pair[0] > *pair[1] {
			return v, ErrHostedEmbeddingWorkLimit
		}
	}
	if v.SnapshotTimeout == 0 {
		v.SnapshotTimeout = max.SnapshotTimeout
	}
	if v.SnapshotTimeout < 0 || v.SnapshotTimeout > max.SnapshotTimeout {
		return v, ErrHostedEmbeddingWorkLimit
	}
	return v, nil
}
func NewHostedEmbeddingStore(ctx context.Context, p *sql.DB, o HostedEmbeddingOptions) (*HostedEmbeddingStore, error) {
	if err := CheckHostedTenant(ctx, p, o.Schema, o.Tenant); err != nil {
		return nil, err
	}
	tables, err := embeddingTables(ctx, p, o.Schema)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, ErrHostedEmbeddingUnprovisioned
	}
	limits, err := embeddingLimits(o.Limits)
	if err != nil {
		return nil, err
	}
	if o.Limits.RowPageSize == 0 {
		limits.RowPageSize = 256
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 5
	}
	if o.MaxAttempts < 1 || o.MaxAttempts > 10 {
		return nil, ErrHostedEmbeddingWorkLimit
	}
	if o.RetryDelay == 0 {
		o.RetryDelay = time.Second
	}
	if o.RetryDelay < 0 || o.RetryDelay > time.Minute {
		return nil, ErrHostedEmbeddingWorkLimit
	}
	return &HostedEmbeddingStore{p, o.Schema, o.Tenant, limits, o.MaxAttempts, o.RetryDelay}, nil
}
func (s *HostedEmbeddingStore) fence(ctx context.Context) (*sql.Tx, error) {
	tx, e := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	var one int
	e = tx.QueryRowContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE tenant_id=$1 AND singleton=1 FOR UPDATE`, s.tenant).Scan(&one)
	if e != nil {
		_ = tx.Rollback()
		return nil, e
	}
	return tx, nil
}
func loadEmbeddingGeneration(ctx context.Context, q hostedQuerier, id int64) (HostedEmbeddingGeneration, error) {
	var g HostedEmbeddingGeneration
	var b []byte
	e := q.QueryRowContext(ctx, `SELECT id,instance_key,recipe_json,activation_mode FROM hosted_embedding_generations WHERE id=$1`, id).Scan(&g.ID, &g.InstanceKey, &b, &g.ActivationMode)
	if e != nil {
		return g, e
	}
	if e = json.Unmarshal(b, &g.Recipe); e != nil {
		return g, e
	}
	g.Recipe, e = CanonicalHostedEmbeddingRecipe(g.Recipe)
	return g, e
}
func embeddingChunkTable(id int64) string { return fmt.Sprintf("hosted_embedding_chunks_g%d", id) }
