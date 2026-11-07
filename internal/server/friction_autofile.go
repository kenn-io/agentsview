package server

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/friction/filing"
)

// frictionAutoFilePerPass caps the new patterns one hourly pass files, so
// turning automatic filing on spreads a backlog over several hours instead
// of filing it to Kata at once.
const frictionAutoFilePerPass = 20

// frictionAutoFileOn reports whether this server files patterns
// automatically: a hub with [kata] enabled, a friction job, and
// [friction.kata] auto_file.
func (s *Server) frictionAutoFileOn() bool {
	s.mu.RLock()
	autoFile := s.cfg.Friction.Kata.AutoFile
	s.mu.RUnlock()
	kataCfg := s.kataConn().Config()
	return autoFile && s.frictionRunner != nil && kataCfg.Enabled && kataCfg.Hub
}

// autoFileFriction is the friction job's automatic filing pass. It does
// nothing unless frictionAutoFileOn.
func (s *Server) autoFileFriction(ctx context.Context) error {
	if !s.frictionAutoFileOn() {
		return nil
	}
	s.mu.RLock()
	policy, backfill := s.cfg.Friction.Kata, s.cfg.Friction.BackfillDays
	s.mu.RUnlock()
	runner := s.frictionRunner
	store, ok := s.db.(filing.Store)
	if !ok || !frictionLinksWritable(store) {
		return nil
	}
	// The catch-up released its work lease; hold one so an idle daemon
	// does not stop between creating an issue and saving its link.
	release, ok := s.idle.BeginWork()
	if !ok {
		return nil
	}
	defer release()
	latest, err := time.Parse(time.DateOnly, runner.LatestCompleteDate())
	if err != nil {
		return err
	}
	f := &filing.Filer{
		Kata: s.kataConn(), Store: store, PublicURL: s.frictionPublicURL(),
		Exclusive: func(work func() error) error { return s.SerializeArchiveWrite(ctx, work) },
	}
	rep, err := f.Reconcile(ctx, filing.Policy{
		Kinds:       policy.FileKinds(),
		Since:       latest.AddDate(0, 0, 1-max(backfill, 1)).Format(time.DateOnly),
		MinSessions: policy.FileMinSessions(),
		MaxNew:      frictionAutoFilePerPass,
	})
	if rep != (filing.ReconcileReport{}) {
		log.Printf("friction: automatic filing created %d, linked %d, will retry %d, needs a person %d",
			rep.Created, rep.Linked, rep.Retry, rep.NeedsPerson)
	}
	return err
}
