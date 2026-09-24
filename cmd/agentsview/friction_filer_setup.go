package main

import (
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
)

type frictionFilerDeps struct {
	Cfg       *config.Config
	Store     filing.LinkStore
	Conn      *kata.Conn
	Runner    *review.Runner
	IsPGServe bool
	Now       func() time.Time
}

func frictionKinds(names []string) []friction.Kind {
	out := make([]friction.Kind, 0, len(names))
	for _, n := range names {
		out = append(out, friction.Kind(n))
	}
	return out
}

// newFrictionFiler builds the filer only on the agentsview filing hub with
// [kata] enabled (spec §13.0). The decision is logged once at startup.
func newFrictionFiler(d frictionFilerDeps) *filing.Filer {
	if !d.Cfg.Kata.Enabled {
		return nil
	}
	pushes := d.Cfg.HasPGPushTarget()
	if !filing.EligibleHost(d.IsPGServe, pushes) {
		log.Printf("friction: [kata] is configured but this instance pushes to PostgreSQL, so it is not the filing hub; Kata filing is off here")
		return nil
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	pk := d.Cfg.Friction.Kata
	f := &filing.Filer{
		Kata:      d.Conn,
		Store:     d.Store,
		Redact:    filing.DefaultRedact,
		Now:       now,
		Policy:    filing.Policy{AutoFile: pk.AutoFile, ReopenOnRecurrence: pk.ReopenOnRecurrence, Kinds: frictionKinds(pk.Kinds), Actor: d.Cfg.Kata.Actor},
		Instance:  d.Cfg.InstallationID,
		PublicURL: d.Cfg.PublicURL,
	}
	if d.Runner != nil {
		f.Snapshot, f.Rerender = d.Runner.Snapshot, d.Runner.Rerender
	}
	log.Printf("friction: Kata filing enabled on this hub (pg serve=%v, auto_file=%v)", d.IsPGServe, pk.AutoFile)
	return f
}

// attachFiler sets Runner.Filer and AutoFile without ever storing a typed-nil
// pointer in the interface, which would read as "filing on".
func attachFiler(r *review.Runner, f *filing.Filer) {
	if f == nil {
		r.Filer = nil
		r.AutoFile = false
		return
	}
	r.Filer = f
	r.AutoFile = f.Policy.AutoFile
}
