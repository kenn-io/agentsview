// Package archivepush runs one push from the SQLite archive to a replica or
// mirror. The process that owns the archive calls it: the daemon's push routes
// and a CLI push that opened the archive itself. Both run the same steps, so a
// push behaves the same whichever process runs it.
//
// The caller syncs the archive and settles the sync engine first (see
// sync.Engine.SettleForPush). A push does not hold the sync lock, so local
// ingestion keeps writing while it runs; each push takes its cutoff under the
// archive write lock, and later pushes pick up writes that land meanwhile.
package archivepush

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// RefreshPricing refreshes model pricing before a push. A failed refresh is
// logged and the push continues with the prices on hand, unless ctx ended.
func RefreshPricing(
	ctx context.Context, refresh func(context.Context) error,
) error {
	if refresh != nil {
		if err := refresh(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			log.Printf("warning: pricing refresh failed: %v", err)
		}
	}
	return ctx.Err()
}

// Stage names a step of a replica push, for progress output.
type Stage string

const (
	StageConnect Stage = "connect"
	StageSchema  Stage = "schema"
	StagePush    Stage = "push"
)

// ReplicaSession pushes the archive to one replica. It keeps the connection
// between pushes and drops it after any replica error, so the next push
// reconnects.
type ReplicaSession struct {
	// Connect opens the replica pusher.
	Connect func(context.Context) (storage.Pusher, error)
	// EnsurePricing refreshes model pricing before each push. Optional.
	EnsurePricing func(context.Context) error
	// OnStage, when set, is called as each stage starts. The returned func,
	// when not nil, is called when the stage succeeds.
	OnStage func(Stage) func()
	// FrictionReview, when set, reviews stale friction findings under this
	// engine's sync lock before each push, so pushed sessions carry current
	// findings. Set it when the replica stores friction. Optional.
	FrictionReview *syncpkg.Engine

	target storage.Pusher
}

// Push reviews friction, refreshes pricing, connects if needed, prepares the
// schema, and pushes. On a push error the result holds what was copied.
func (s *ReplicaSession) Push(
	ctx context.Context,
	opts storage.PushOptions,
	onProgress func(storage.PushProgress),
) (storage.PushResult, error) {
	if engine := s.FrictionReview; engine != nil {
		// The push itself runs without the sync lock.
		if err := engine.RunExclusive(func() error {
			return engine.DrainStaleFrictionLocked(ctx)
		}); err != nil {
			return storage.PushResult{}, err
		}
	}
	if err := RefreshPricing(ctx, s.EnsurePricing); err != nil {
		return storage.PushResult{}, err
	}
	if s.target == nil {
		done := s.stage(StageConnect)
		target, err := s.Connect(ctx)
		if err != nil {
			return storage.PushResult{}, err
		}
		s.target = target
		done()
	}
	done := s.stage(StageSchema)
	if err := s.target.EnsureSchema(ctx); err != nil {
		s.Close()
		return storage.PushResult{}, fmt.Errorf("schema: %w", err)
	}
	done()
	s.stage(StagePush)
	// A failed push still returns what it copied: a vector build can fail
	// after the sessions land.
	result, err := s.target.PushWithOptions(ctx, opts, onProgress)
	if err != nil {
		s.Close()
	}
	return result, err
}

// Close drops the replica connection. The session can push again afterward.
func (s *ReplicaSession) Close() {
	if s.target != nil {
		_ = s.target.Close()
		s.target = nil
	}
}

func (s *ReplicaSession) stage(stage Stage) func() {
	if s.OnStage != nil {
		if done := s.OnStage(stage); done != nil {
			return done
		}
	}
	return func() {}
}

// MirrorPush pushes the archive to a local mirror such as DuckDB.
type MirrorPush struct {
	Mirror  storage.Mirror
	Config  config.DuckDBConfig
	Archive *db.DB
	Options storage.MirrorPushOptions
	// EnsurePricing refreshes model pricing before each push. Optional.
	EnsurePricing func(context.Context) error
}

// Push refreshes pricing and pushes. full rewrites the whole mirror.
func (m MirrorPush) Push(
	ctx context.Context, full bool,
	onProgress func(storage.MirrorPushProgress),
) (storage.MirrorPushResult, error) {
	if err := RefreshPricing(ctx, m.EnsurePricing); err != nil {
		return storage.MirrorPushResult{}, err
	}
	return m.Mirror.Push(ctx, m.Config, m.Archive, m.Options, full, onProgress)
}
