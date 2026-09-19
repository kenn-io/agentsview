package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// ReplicaTarget identifies one remote replica database. It is the wire shape
// the CLI sends to the daemon for a delegated push, so its JSON names are part
// of the daemon API.
type ReplicaTarget struct {
	URL    string `json:"url"`
	Schema string `json:"schema,omitempty"`
	// MachineName labels the sessions this archive owns on the replica. It
	// must not be "local", the SQLite sentinel for sessions that originated
	// on this machine.
	MachineName   string `json:"machine_name"`
	AllowInsecure bool   `json:"allow_insecure,omitzero"`
	// PushVectors reports whether the target accepts the vector push phase.
	PushVectors bool `json:"push_vectors,omitzero"`
}

// ReplicaTargetRef names one configured replica target before its settings
// are resolved. Name is empty for a single unnamed config section.
type ReplicaTargetRef struct {
	Name      string
	IsDefault bool
}

// Label is the operator-facing target name used in multi-target output.
func (r ReplicaTargetRef) Label() string {
	if r.Name == "" {
		return "default"
	}
	if r.IsDefault {
		return r.Name + " (default)"
	}
	return r.Name
}

// SyncStateTarget scopes the archive's per-target push watermarks and
// fingerprints. An unnamed section keeps the unscoped legacy keys.
func (r ReplicaTargetRef) SyncStateTarget() string { return r.Name }

// MigrateLegacySyncState reports whether the first push of this target moves
// unscoped legacy sync-state keys into the named default target.
func (r ReplicaTargetRef) MigrateLegacySyncState() bool {
	return r.IsDefault && r.Name != ""
}

// ConfiguredReplica is one replica target with its settings resolved from
// the operator's config.
type ConfiguredReplica struct {
	ReplicaTargetRef
	Target ReplicaTarget
	// Projects and ExcludeProjects are the configured push scope. At most
	// one is set.
	Projects        []string
	ExcludeProjects []string
}

// PusherOptions scopes one connected push session.
type PusherOptions struct {
	// Projects limits push scope to these project names.
	// Mutually exclusive with ExcludeProjects.
	Projects []string
	// ExcludeProjects excludes these project names from push.
	// Mutually exclusive with Projects.
	ExcludeProjects []string
	// SyncStateTarget scopes per-target push watermarks and fingerprints.
	SyncStateTarget string
	// MigrateLegacySyncState moves unsuffixed legacy sync-state keys into the
	// named default target the first time that target runs.
	MigrateLegacySyncState bool
	// VectorSource, when non-nil, enables the vector push phase, replicating
	// the local vectors.db active generation into the replica. Nil skips
	// the phase.
	VectorSource VectorPushSource
}

// Pusher is one connected push session from the SQLite archive to a replica.
// Callers must finish every push before Close.
type Pusher interface {
	// EnsureSchema creates or migrates the replica schema so a push can run.
	EnsureSchema(ctx context.Context) error
	// PushWithOptions sends changed sessions, messages, and optionally
	// vectors. onProgress, when non-nil, is called after each batch.
	PushWithOptions(
		ctx context.Context, opts PushOptions,
		onProgress func(PushProgress),
	) (PushResult, error)
	Close() error
}

// ReplicaStore is a replica opened for reading. ReadOnly reports true; local
// writes return db.ErrReadOnly.
type ReplicaStore interface {
	db.Store
	// SetCursorSecret installs the HMAC key that signs pagination cursors.
	SetCursorSecret(secret []byte)
	// SetCustomPricing installs operator-supplied model rates.
	SetCustomPricing(rates map[string]config.CustomModelRate)
	Close() error
}

// ReplicaStatus summarizes a replica for the `<backend> status` command.
type ReplicaStatus struct {
	Machine    string
	LastPushAt string
	Sessions   int
	Messages   int
}

// SyncStateStore is the archive-side key/value store that holds push
// watermarks. *db.DB implements it.
type SyncStateStore interface {
	GetSyncState(ctx context.Context, key string) (string, error)
	SetSyncState(ctx context.Context, key, value string) error
	GetOrCreateSyncState(ctx context.Context, key, defaultValue string) (string, error)
}

// Replica is a remote database the archive pushes into and that serves the
// web UI read-only. Implementations must be safe to use as zero values; the
// CLI and server hold one value per compiled-in backend.
type Replica interface {
	// Name is the CLI verb and the daemon push route segment, e.g. "pg".
	Name() string
	// DisplayName is the product name shown to operators, e.g. "PostgreSQL".
	DisplayName() string
	// Targets lists every configured target. A config without named
	// sections yields one unnamed default target even when nothing is set,
	// so callers report "url not configured" per target.
	Targets(cfg config.Config) ([]ReplicaTargetRef, error)
	// ResolveTarget applies defaults and environment overrides to one
	// configured target.
	ResolveTarget(cfg config.Config, ref ReplicaTargetRef) (ConfiguredReplica, error)
	// NewPusher connects to target and prepares a push session from local.
	NewPusher(
		target ReplicaTarget, local *db.DB, opts PusherOptions,
	) (Pusher, error)
	// OpenStore connects for CLI reads without touching the schema.
	OpenStore(target ReplicaTarget) (ReplicaStore, error)
	// OpenServeStore connects for `<backend> serve`: it brings the schema
	// current when the role may write, verifies compatibility, and probes
	// optional capabilities such as insight generation.
	OpenServeStore(ctx context.Context, target ReplicaTarget) (ReplicaStore, error)
	// Status reads replica-side counters. lastPush is the archive-side
	// watermark the caller wants echoed back.
	Status(
		ctx context.Context, target ReplicaTarget, lastPush string,
	) (ReplicaStatus, error)
	// LastPushAt reads the archive-side watermark for target under the
	// effective project filters.
	LastPushAt(
		ctx context.Context, local SyncStateStore, target ReplicaTargetRef,
		projects, excludeProjects []string,
	) (string, error)
}

// SelectTargets picks the configured targets a command runs against: every
// target when all is set, the named target, or the default.
func SelectTargets(
	r Replica, cfg config.Config, name string, all bool,
) ([]ReplicaTargetRef, error) {
	if all && strings.TrimSpace(name) != "" {
		return nil, errors.New("target name cannot be combined with --all")
	}
	refs, err := r.Targets(cfg)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%s: no targets configured", r.Name())
	}
	named := refs[0].Name != ""
	if !named && strings.TrimSpace(name) != "" {
		return nil, fmt.Errorf(
			"%s target %q is not configured; config uses a single legacy [%s] block",
			r.Name(), name, r.Name(),
		)
	}
	if all {
		return refs, nil
	}
	normalized := strings.TrimSpace(strings.ToLower(name))
	if normalized == "" {
		return refs[:1], nil
	}
	for _, ref := range refs {
		if ref.Name == normalized {
			return []ReplicaTargetRef{ref}, nil
		}
	}
	return nil, fmt.Errorf(
		"%s target %q is not configured", r.Name(), name,
	)
}

// DefaultTarget resolves the default configured target.
func DefaultTarget(r Replica, cfg config.Config) (ConfiguredReplica, error) {
	refs, err := SelectTargets(r, cfg, "", false)
	if err != nil {
		return ConfiguredReplica{}, err
	}
	return r.ResolveTarget(cfg, refs[0])
}
