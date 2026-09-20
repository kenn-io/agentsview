package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Backend is the ClickHouse replica. It adapts this package's push, schema,
// store, and status functions to the storage.Replica contract; it holds no
// state. ClickHouse keeps its push cursor in the mirror's own metadata, so
// LastPushAt reads the replica rather than the archive.
type Backend struct{}

var _ storage.Replica = Backend{}

func (Backend) Name() string { return "clickhouse" }

func (Backend) DisplayName() string { return "ClickHouse" }

// Targets lists the configured [clickhouse.NAME] sections, default first, or
// one unnamed default target for a legacy [clickhouse] block or no block.
func (Backend) Targets(cfg config.Config) ([]storage.ReplicaTargetRef, error) {
	names, defaultName, err := cfg.ClickHouseTargetNames()
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
		ch  config.ClickHouseConfig
		err error
	)
	if ref.Name == "" {
		ch, err = cfg.ResolveClickHouse()
	} else {
		ch, err = cfg.ResolveClickHouseTarget(ref.Name)
	}
	if err != nil {
		return storage.ConfiguredReplica{}, err
	}
	return storage.ConfiguredReplica{
		ReplicaTargetRef: ref,
		Target:           ReplicaTarget(ch),
		Projects:         ch.Projects,
		ExcludeProjects:  ch.ExcludeProjects,
	}, nil
}

// ReplicaTarget maps a resolved [clickhouse] section onto the connection
// identity a push or serve uses. The ClickHouse database plays the schema
// role. ClickHouse has no vector phase.
func ReplicaTarget(ch config.ClickHouseConfig) storage.ReplicaTarget {
	return storage.ReplicaTarget{
		URL:           ch.URL,
		Schema:        ch.Database,
		MachineName:   ch.MachineName,
		AllowInsecure: ch.AllowInsecure,
		PushVectors:   ch.PushVectorsEnabled(),
	}
}

func target(t storage.ReplicaTarget) Target {
	return Target{URL: t.URL, Database: t.Schema}
}

// ValidateTarget rejects a plaintext or unverified-TLS remote connection
// unless the target allows it.
func (Backend) ValidateTarget(t storage.ReplicaTarget) error {
	return CheckTransportSecurity(t.URL, t.AllowInsecure)
}

// NewPusher rejects a plaintext remote connection unless the target allows
// it, then prepares a push. The pusher connects on EnsureSchema.
func (Backend) NewPusher(
	ctx context.Context, t storage.ReplicaTarget, local *db.DB,
	opts storage.PusherOptions,
) (storage.Pusher, error) {
	if err := CheckTransportSecurity(t.URL, t.AllowInsecure); err != nil {
		return nil, err
	}
	s, err := New(ctx, target(t), local, t.MachineName, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (Backend) OpenStore(
	t storage.ReplicaTarget,
) (storage.ReplicaStore, error) {
	if err := CheckTransportSecurity(t.URL, t.AllowInsecure); err != nil {
		return nil, err
	}
	store, err := NewStore(context.Background(), target(t))
	if err != nil {
		return nil, err
	}
	return store, nil
}

// OpenServeStore creates the mirror schema when the role may, then connects
// with the schema and data-version checks NewStore performs.
func (Backend) OpenServeStore(
	ctx context.Context, t storage.ReplicaTarget,
) (storage.ReplicaStore, error) {
	if err := CheckTransportSecurity(t.URL, t.AllowInsecure); err != nil {
		return nil, err
	}
	if err := EnsureSchema(ctx, target(t)); err != nil {
		if !IsPermissionError(err) {
			return nil, fmt.Errorf("schema migration failed: %w", err)
		}
	}
	store, err := NewStore(ctx, target(t))
	if err != nil {
		return nil, err
	}
	return store, nil
}

// Status reads the mirror's own metadata and counters. The archive supplies
// only its id, which scopes the per-archive cursor keys; a missing archive
// reads the unscoped keys and logs why.
func (Backend) Status(
	ctx context.Context, local *db.DB, t storage.ConfiguredReplica,
	projects, excludeProjects []string,
) (storage.ReplicaStatus, error) {
	if err := CheckTransportSecurity(t.Target.URL, t.Target.AllowInsecure); err != nil {
		return storage.ReplicaStatus{}, err
	}
	archiveID := ""
	if local != nil {
		var err error
		archiveID, err = local.GetArchiveID(ctx)
		if err != nil {
			log.Printf("warning: reading local archive id: %v", err)
			archiveID = ""
		}
	}
	status, err := ReadStatus(
		ctx, target(t.Target), t.Target.MachineName, archiveID,
		projects, excludeProjects,
	)
	if err != nil {
		return storage.ReplicaStatus{}, err
	}
	rows := []storage.StatusRow{
		{Label: "Machine:", Value: status.Machine},
		{Label: "Last push:", Value: valueOrNever(status.LastPushAt)},
		{Label: "Last push machine:", Value: status.LastPushMachine},
	}
	if status.Scope != "" {
		rows = append(rows, storage.StatusRow{Label: "Push scope:", Value: status.Scope})
	}
	rows = append(rows,
		storage.StatusRow{Label: "ClickHouse sessions:", Value: strconv.Itoa(status.Sessions)},
		storage.StatusRow{Label: "ClickHouse messages:", Value: strconv.Itoa(status.Messages)},
	)
	if status.SchemaMissing {
		rows = append(rows, storage.StatusRow{Label: "Schema:", Value: "not created yet"})
	}
	return storage.ReplicaStatus{Rows: rows}, nil
}

// LastPushAt reads this archive's cursor from the mirror metadata.
func (Backend) LastPushAt(
	ctx context.Context, local *db.DB, t storage.ConfiguredReplica,
	_, _ []string,
) (string, error) {
	if err := CheckTransportSecurity(t.Target.URL, t.Target.AllowInsecure); err != nil {
		return "", err
	}
	if local == nil {
		return "", errors.New("clickhouse last push requires the local archive id")
	}
	archiveID, err := local.GetArchiveID(ctx)
	if err != nil {
		return "", err
	}
	status, err := ReadStatus(ctx, target(t.Target), t.Target.MachineName, archiveID, nil, nil)
	if err != nil {
		return "", err
	}
	return status.LastPushAt, nil
}

func valueOrNever(v string) string {
	if v == "" {
		return "never"
	}
	return v
}

// VectorGenerations lists the embedding generations pushed to store, oldest
// first.
func (Backend) VectorGenerations(
	ctx context.Context, store storage.ReplicaStore,
) ([]storage.VectorGenerationInfo, error) {
	chStore, err := asStore(store)
	if err != nil {
		return nil, err
	}
	return ListVectorGenerationInfo(ctx, chStore.DB())
}

// OpenVectorSearcher serves the pushed generation matching gen.Fingerprint.
// Every generation shares one chunk table, so a registered generation is
// always ready to search.
func (Backend) OpenVectorSearcher(
	ctx context.Context, store storage.ReplicaStore, gen storage.VectorGenerationInfo,
	maxInputChars int, encode storage.VectorQueryEncoder,
) (db.VectorSearcher, string, error) {
	chStore, err := asStore(store)
	if err != nil {
		return nil, "", err
	}
	dim, found, err := LookupVectorGeneration(ctx, chStore.DB(), gen.Fingerprint)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "ClickHouse has no embedding generation matching fingerprint " + gen.Fingerprint, nil
	}
	return NewVectorSearcher(chStore.DB(), gen.Fingerprint, dim, maxInputChars, encode), "", nil
}

// asStore unwraps the concrete ClickHouse store a replica-neutral caller
// holds; only this backend opens them, so any other type is a programming
// error.
func asStore(store storage.ReplicaStore) (*Store, error) {
	chStore, ok := store.(*Store)
	if !ok {
		return nil, fmt.Errorf("clickhouse store is %T, not *clickhouse.Store", store)
	}
	return chStore, nil
}
