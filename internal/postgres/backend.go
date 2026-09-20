package postgres

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Backend is the PostgreSQL (and CockroachDB) replica. It adapts this
// package's connection, schema, push, and status functions to the
// storage.Replica contract; it holds no state.
type Backend struct{}

var _ storage.Replica = Backend{}

func (Backend) Name() string { return "pg" }

func (Backend) DisplayName() string { return "PostgreSQL" }

// Targets lists the configured [pg.NAME] sections, default first, or one
// unnamed default target for a legacy [pg] block or no block at all.
func (Backend) Targets(cfg config.Config) ([]storage.ReplicaTargetRef, error) {
	names, defaultName, err := cfg.PGTargetNames()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []storage.ReplicaTargetRef{{IsDefault: true}}, nil
	}
	refs := make([]storage.ReplicaTargetRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, storage.ReplicaTargetRef{
			Name:      name,
			IsDefault: name == defaultName,
		})
	}
	return refs, nil
}

func (Backend) ResolveTarget(
	cfg config.Config, ref storage.ReplicaTargetRef,
) (storage.ConfiguredReplica, error) {
	var (
		pg  config.PGConfig
		err error
	)
	if ref.Name == "" {
		pg, err = cfg.ResolvePG()
	} else {
		pg, err = cfg.ResolvePGTarget(ref.Name)
	}
	if err != nil {
		return storage.ConfiguredReplica{}, err
	}
	return storage.ConfiguredReplica{
		ReplicaTargetRef: ref,
		Target:           ReplicaTarget(pg),
		Projects:         pg.Projects,
		ExcludeProjects:  pg.ExcludeProjects,
	}, nil
}

// ReplicaTarget maps a resolved [pg] section onto the connection identity a
// push or serve uses.
func ReplicaTarget(pg config.PGConfig) storage.ReplicaTarget {
	return storage.ReplicaTarget{
		URL:           pg.URL,
		Schema:        pg.Schema,
		MachineName:   pg.MachineName,
		AllowInsecure: pg.AllowInsecure,
		PushVectors:   pg.PushVectorsEnabled(),
	}
}

// ValidateTarget accepts every target: PostgreSQL transport security is
// negotiated by sslmode at connect time, not rejected up front.
func (Backend) ValidateTarget(storage.ReplicaTarget) error { return nil }

func (Backend) NewPusher(
	_ context.Context, target storage.ReplicaTarget, local *db.DB,
	opts storage.PusherOptions,
) (storage.Pusher, error) {
	s, err := New(
		target.URL, target.Schema, local,
		target.MachineName, target.AllowInsecure, opts,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (Backend) OpenStore(
	target storage.ReplicaTarget,
) (storage.ReplicaStore, error) {
	store, err := NewStore(target.URL, target.Schema, target.AllowInsecure)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// OpenServeStore connects, migrates the schema when the role may write,
// verifies schema and data-version compatibility, and probes whether the role
// can persist generated insights.
func (Backend) OpenServeStore(
	ctx context.Context, target storage.ReplicaTarget,
) (storage.ReplicaStore, error) {
	store, err := NewStore(target.URL, target.Schema, target.AllowInsecure)
	if err != nil {
		return nil, err
	}
	if err := prepareServeStore(ctx, store, target.Schema); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func prepareServeStore(ctx context.Context, store *Store, schema string) error {
	if err := EnsureSchema(ctx, store.DB(), schema); err != nil {
		if !IsReadOnlyError(err) {
			return fmt.Errorf("schema migration failed: %w", err)
		}
	}
	if err := CheckSchemaCompat(ctx, store.DB()); err != nil {
		return fmt.Errorf(
			"schema incompatible: %w\n"+
				"Drop and recreate the PG schema, then run "+
				"'agentsview pg push --full' to repopulate",
			err,
		)
	}
	if err := CheckDataVersionCompat(ctx, store.DB()); err != nil {
		return err
	}
	if err := store.DetectInsightGenerationAvailability(ctx); err != nil {
		return fmt.Errorf(
			"probing insight generation capability: %w", err,
		)
	}
	return nil
}

// Status reads the PostgreSQL counters and echoes the archive-side watermark.
// A watermark read failure is a warning, not a status failure: the
// watermark is informational while the PostgreSQL query proves connectivity.
func (b Backend) Status(
	ctx context.Context, local *db.DB, target storage.ConfiguredReplica,
	projects, excludeProjects []string,
) (storage.ReplicaStatus, error) {
	lastPush := ""
	if local != nil {
		var err error
		lastPush, err = b.LastPushAt(ctx, local, target, projects, excludeProjects)
		if err != nil {
			log.Printf("warning: reading last_push_at: %v", err)
			lastPush = ""
		}
	}
	status, err := ReadStatus(
		ctx, target.Target.URL, target.Target.Schema, target.Target.MachineName,
		target.Target.AllowInsecure, lastPush,
	)
	if err != nil {
		return storage.ReplicaStatus{}, err
	}
	return storage.ReplicaStatus{Rows: []storage.StatusRow{
		{Label: "Machine:", Value: status.Machine},
		{Label: "Last push:", Value: valueOrNever(status.LastPushAt)},
		{Label: "PG sessions:", Value: strconv.Itoa(status.PGSessions)},
		{Label: "PG messages:", Value: strconv.Itoa(status.PGMessages)},
	}}, nil
}

// LastPushAt reads the per-target watermark PostgreSQL pushes keep in the
// archive's sync state.
func (Backend) LastPushAt(
	ctx context.Context, local *db.DB, target storage.ConfiguredReplica,
	projects, excludeProjects []string,
) (string, error) {
	if local == nil {
		return "", nil
	}
	return ReadLastPushAt(
		ctx, local, target.SyncStateTarget(), projects, excludeProjects,
		target.MigrateLegacySyncState(),
	)
}

func valueOrNever(v string) string {
	if v == "" {
		return "never"
	}
	return v
}

// VectorGenerations lists the embedding generations pushed to store, oldest
// first. A database where pgvector was never installed has no
// vector_generations table and reports none.
func (Backend) VectorGenerations(
	ctx context.Context, store storage.ReplicaStore,
) ([]storage.VectorGenerationInfo, error) {
	pgStore, err := asStore(store)
	if err != nil {
		return nil, err
	}
	return ListVectorGenerationInfo(ctx, pgStore.DB())
}

// OpenVectorSearcher serves the pushed generation matching gen.Fingerprint.
// A registered generation whose chunk table is missing (a push interrupted
// between registering the generation and creating the table) degrades like
// a fingerprint miss: wiring a searcher against it would fail every query
// with a missing-relation error instead of a clear unavailable reason.
func (Backend) OpenVectorSearcher(
	ctx context.Context, store storage.ReplicaStore, gen storage.VectorGenerationInfo,
	maxInputChars int, encode storage.VectorQueryEncoder,
) (db.VectorSearcher, string, error) {
	pgStore, err := asStore(store)
	if err != nil {
		return nil, "", err
	}
	genID, dim, ok, err := LookupVectorGeneration(ctx, pgStore.DB(), gen.Fingerprint)
	if err != nil {
		return nil, "", fmt.Errorf("looking up PG vector generation: %w", err)
	}
	if !ok {
		return nil, "PostgreSQL has no embedding generation matching fingerprint " + gen.Fingerprint, nil
	}
	tableOK, err := VectorChunkTableExists(ctx, pgStore.DB(), genID)
	if err != nil {
		return nil, "", fmt.Errorf("probing PG vector chunk table: %w", err)
	}
	if !tableOK {
		return nil, fmt.Sprintf(
			"PG generation %d matches fingerprint %s but its chunk table is "+
				"missing (interrupted push?)", genID, gen.Fingerprint), nil
	}
	return NewVectorSearcher(pgStore.DB(), genID, dim, maxInputChars, encode), "", nil
}

// asStore unwraps the concrete PostgreSQL store a replica-neutral caller
// holds. The contract passes stores as storage.ReplicaStore so shared code
// never imports this package; only this backend opens them, so any other
// type is a programming error.
func asStore(store storage.ReplicaStore) (*Store, error) {
	pgStore, ok := store.(*Store)
	if !ok {
		return nil, fmt.Errorf("pg store is %T, not *postgres.Store", store)
	}
	return pgStore, nil
}
