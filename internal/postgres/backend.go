package postgres

import (
	"context"
	"fmt"

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

func (Backend) NewPusher(
	target storage.ReplicaTarget, local *db.DB, opts storage.PusherOptions,
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

func (Backend) Status(
	ctx context.Context, target storage.ReplicaTarget, lastPush string,
) (storage.ReplicaStatus, error) {
	status, err := ReadStatus(
		ctx, target.URL, target.Schema, target.MachineName,
		target.AllowInsecure, lastPush,
	)
	if err != nil {
		return storage.ReplicaStatus{}, err
	}
	return storage.ReplicaStatus{
		Machine:    status.Machine,
		LastPushAt: status.LastPushAt,
		Sessions:   status.PGSessions,
		Messages:   status.PGMessages,
	}, nil
}

func (Backend) LastPushAt(
	ctx context.Context, local storage.SyncStateStore, target storage.ReplicaTargetRef,
	projects, excludeProjects []string,
) (string, error) {
	return ReadLastPushAt(
		ctx, local, target.SyncStateTarget(), projects, excludeProjects,
		target.MigrateLegacySyncState(),
	)
}
