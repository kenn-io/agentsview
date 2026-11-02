package server

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/friction/filing"
)

// autoFileFriction is the friction job's automatic filing pass. It does
// nothing unless this hub has [friction.kata] auto_file and [kata] enabled.
func (s *Server) autoFileFriction(ctx context.Context) error {
	s.mu.RLock()
	policy, backfill := s.cfg.Friction.Kata, s.cfg.Friction.BackfillDays
	s.mu.RUnlock()
	runner := s.frictionRunner
	if kataCfg := s.kataConn().Config(); !policy.AutoFile || runner == nil || !kataCfg.Enabled || !kataCfg.Hub {
		return nil
	}
	store, ok := s.db.(filing.Store)
	if !ok {
		return nil
	}
	latest, err := time.Parse(time.DateOnly, runner.LatestCompleteDate())
	if err != nil {
		return err
	}
	f := &filing.Filer{Kata: s.kataConn(), Store: store, PublicURL: s.frictionPublicURL()}
	rep, err := f.Reconcile(ctx, filing.Policy{
		Kinds: policy.FileKinds(),
		Since: latest.AddDate(0, 0, 1-max(backfill, 1)).Format(time.DateOnly),
	})
	if rep != (filing.ReconcileReport{}) {
		log.Printf("friction: automatic filing created %d, linked %d, will retry %d, needs a person %d",
			rep.Created, rep.Linked, rep.Retry, rep.NeedsPerson)
	}
	return err
}
