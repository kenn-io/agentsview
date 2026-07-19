package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
)

// resolvePGServeVectorState classifies the startup gate into (wire, reason).
// It is the pure core of wirePGVectorSearch, split out so the decision
// logic is unit-testable without a PostgreSQL connection.
//
// Cases:
//   - vector disabled: (false, <reason>) — the caller records the PostgreSQL
//     setup and push workflow.
//   - enabled and the local config's fingerprint matches a PG generation:
//     (true, "") — the caller wires the searcher.
//   - enabled but no PG generation matches: (false, <reason>) — the caller
//     records the reason so the search endpoint can explain the miss.
//
// foundFPs is the comma-joined list of fingerprints PG does have, surfaced so
// an operator can see whether it is a "wrong config" or "never pushed" miss.
func resolvePGServeVectorState(
	vectorEnabled, genFound bool, wantFP, foundFPs string,
) (bool, string) {
	if !vectorEnabled {
		return false,
			"semantic search: PostgreSQL requires [vector] enabled with a " +
				"matching [vector.embeddings] config and a generation pushed " +
				"by 'agentsview pg push'"
	}
	if genFound {
		return true, ""
	}
	reason := fmt.Sprintf(
		"semantic search: PG has no embedding generation matching fingerprint "+
			"%s (present: %s); run 'agentsview pg push' from a machine with a "+
			"matching [vector.embeddings] config",
		wantFP, foundFPs)
	return false, reason
}

// wirePGVectorSearch attaches PG-backed semantic search to store when the
// local [vector.embeddings] config's fingerprint matches a generation already
// pushed to PostgreSQL. It is the shared startup gate for every PG read
// surface: `pg serve` (which treats the returned error as fatal) and the CLI
// direct-read path via wirePGReadVectorSearch (which warns and degrades).
// label prefixes the log lines with the calling surface ("pg serve",
// "pg read").
//
// A miss is never an error: a missing pgvector table, a fingerprint mismatch,
// or an unbuilt query encoder each leave semantic search unavailable (a
// recorded reason surfaced through db.ErrSemanticUnavailable) rather than
// failing construction. It returns an error only for a genuinely unexpected
// query failure; the caller decides whether that is fatal.
//
// No per-query staleness gate is needed here: a PG generation is keyed by its
// immutable fingerprint, so a startup match cannot go stale while the process
// runs. Changing the local embeddings config changes the fingerprint, which
// requires restarting the serve (or re-running the CLI command), and that
// restart re-runs this gate.
func wirePGVectorSearch(
	ctx context.Context, appCfg config.Config, store *postgres.Store, label string,
) error {
	if !appCfg.Vector.Enabled {
		_, reason := resolvePGServeVectorState(false, false, "", "")
		store.SetSemanticUnavailableReason(reason)
		return nil
	}
	gen := vectorGeneration(appCfg.Vector.Embeddings)
	wantFP := gen.Fingerprint()
	genID, dim, ok, err := postgres.LookupVectorGeneration(ctx, store.DB(), wantFP)
	if err != nil {
		return fmt.Errorf("looking up PG vector generation: %w", err)
	}
	if !ok {
		present, err := postgres.ListVectorGenerationFingerprints(ctx, store.DB())
		if err != nil {
			log.Printf("%s: listing vector generations: %v", label, err)
		}
		_, reason := resolvePGServeVectorState(
			true, false, wantFP, strings.Join(present, ", "))
		store.SetSemanticUnavailableReason(reason)
		log.Printf("%s: %s", label, reason)
		return nil
	}

	// A generation row without its chunk table (a push interrupted between
	// registering the generation and creating the table) must degrade like a
	// fingerprint miss, not fail every query with a missing-relation error.
	tableOK, err := postgres.VectorChunkTableExists(ctx, store.DB(), genID)
	if err != nil {
		return fmt.Errorf("probing PG vector chunk table: %w", err)
	}
	if !tableOK {
		reason := fmt.Sprintf(
			"semantic search: PG generation %d matches fingerprint %s but its "+
				"chunk table is missing (interrupted push?); re-run "+
				"'agentsview pg push' from a machine with a matching "+
				"[vector.embeddings] config", genID, wantFP)
		store.SetSemanticUnavailableReason(reason)
		log.Printf("%s: %s", label, reason)
		return nil
	}

	enc, err := newVectorQueryEncoder(appCfg.Vector.Embeddings, "")
	if err != nil {
		return fmt.Errorf("building query encoder: %w", err)
	}
	encodeQuery := func(ctx context.Context, text string) ([]float32, error) {
		vecs, err := kitvec.EncodeBatched(ctx, enc,
			[]kitvec.Chunk{{Index: 0, Text: text}}, kitvec.BatchOptions{})
		if err != nil {
			return nil, err
		}
		return vecs[0], nil
	}
	store.SetVectorSearcher(postgres.NewVectorSearcher(
		store.DB(), genID, dim, appCfg.Vector.Embeddings.MaxInputChars, encodeQuery))
	log.Printf("%s: semantic search enabled (generation %d, model %s)",
		label, genID, gen.Model)
	return nil
}

// wirePGReadVectorSearchFn is newPGReadService's seam for the vector wiring
// call, overridable in tests that inject fake stores through openPGReadStore.
var wirePGReadVectorSearchFn = wirePGReadVectorSearch

// wirePGReadVectorSearch runs the shared PG vector gate for the CLI
// direct-read path (`session search --pg --semantic|--hybrid`, `mcp --pg`).
// It mirrors installDirectVectorSearcher's error semantics on the SQLite
// direct path: wiring failures never fail service construction — every --pg
// read command shares this constructor, so a vector-side failure must not
// break unrelated reads. A genuine query failure is logged as a warning and
// the command continues with semantic search returning
// db.ErrSemanticUnavailable; a fingerprint miss records its reason inside
// wirePGVectorSearch. Stores that are not *postgres.Store (test fakes
// injected via openPGReadStore) are left untouched.
func wirePGReadVectorSearch(cfg config.Config, store db.Store) {
	pgStore, ok := store.(*postgres.Store)
	if !ok {
		return
	}
	if err := wirePGVectorSearch(
		context.Background(), cfg, pgStore, "pg read",
	); err != nil {
		log.Printf(
			"warning: wiring PG semantic search: %v; "+
				"continuing without semantic search", err,
		)
	}
}
