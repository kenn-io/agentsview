package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/uptrace/bun"
	corerecall "go.kenn.io/agentsview/internal/recall"
)

// bunRecallCapability contains the SQLite-only Recall paths that depend on
// local FTS/vector state, evidence reconciliation, or archive-only tables.
// Public Store ownership and capability policy remain on BunStore.
type bunRecallCapability interface {
	QueryRecallEntries(context.Context, RecallQuery) (RecallPage, error)
	ImportAcceptedRecallEntriesJSONLWithOptions(
		context.Context, io.Reader, RecallImportOptions,
	) (RecallImportResult, error)
	IngestEvalTrajectory(
		context.Context, EvalTrajectoryIngest,
	) (EvalTrajectoryIngestResult, error)
}

func (b *sqliteBunBackend) QueryRecallEntries(
	ctx context.Context, query RecallQuery,
) (RecallPage, error) {
	return b.store.queryRecallEntries(ctx, query)
}

func (b *sqliteBunBackend) ImportAcceptedRecallEntriesJSONLWithOptions(
	ctx context.Context, reader io.Reader, options RecallImportOptions,
) (RecallImportResult, error) {
	return b.store.importAcceptedRecallEntriesJSONLWithOptions(ctx, reader, options)
}

func (b *sqliteBunBackend) IngestEvalTrajectory(
	ctx context.Context, input EvalTrajectoryIngest,
) (EvalTrajectoryIngestResult, error) {
	return b.store.ingestEvalTrajectory(ctx, input)
}

func (s *BunStore) recallCapability() (bunRecallCapability, error) {
	if !s.backend.Capabilities().Recall {
		return nil, ErrReadOnly
	}
	capability, ok := s.backend.(bunRecallCapability)
	if !ok {
		return nil, fmt.Errorf(
			"backend %s advertises Recall without a Recall capability",
			s.backend.Name(),
		)
	}
	return capability, nil
}

// GetRecallEntry returns one Recall entry and its evidence through the guarded
// Bun read handle.
func (s *BunStore) GetRecallEntry(
	ctx context.Context, id string,
) (*RecallEntry, error) {
	if !s.backend.Capabilities().Recall {
		return nil, ErrReadOnly
	}
	var staged *RecallEntry
	err := s.consistentView(ctx, func(store bun.IDB) error {
		var entry RecallEntry
		err := store.NewRaw(
			"SELECT "+recallBaseCols+" FROM recall_entries WHERE id = ?", id,
		).Scan(ctx, &entry)
		if err == sql.ErrNoRows {
			staged = nil
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting recall entry %s: %w", id, err)
		}
		evidence, err := listRecallEvidenceBun(ctx, store, []string{id})
		if err != nil {
			return err
		}
		entry.Evidence = evidence[id]
		staged = &entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return staged, nil
}

// ListRecallEntries returns canonical Recall rows and evidence through one
// coherent Bun view.
func (s *BunStore) ListRecallEntries(
	ctx context.Context, query RecallQuery,
) ([]RecallEntry, error) {
	if !s.backend.Capabilities().Recall {
		return nil, ErrReadOnly
	}
	if err := ValidateRecallQuery(query); err != nil {
		return nil, err
	}
	query = NormalizeRecallQuery(query)
	where, args := buildRecallEntryWhere(query, false)
	limit := recallLimit(query.Limit)
	if query.ProbeNext {
		limit++
	}
	args = append(args, limit)
	entries := []RecallEntry{}
	err := s.consistentView(ctx, func(store bun.IDB) error {
		if err := store.NewRaw(
			"SELECT "+recallBaseCols+" FROM recall_entries WHERE "+where+
				" ORDER BY updated_at DESC, id ASC LIMIT ?", args...,
		).Scan(ctx, &entries); err != nil {
			return fmt.Errorf("querying entries: %w", err)
		}
		if len(entries) == 0 {
			return nil
		}
		evidence, err := listRecallEvidenceBun(ctx, store, recallIDs(entries))
		if err != nil {
			return err
		}
		for index := range entries {
			entries[index].Evidence = evidence[entries[index].ID]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return entries, nil
}

// QueryRecallEntries dispatches to SQLite's local FTS/vector capability after
// the common Store has enforced Recall availability.
func (s *BunStore) QueryRecallEntries(
	ctx context.Context, query RecallQuery,
) (RecallPage, error) {
	capability, err := s.recallCapability()
	if err != nil {
		return RecallPage{}, err
	}
	return capability.QueryRecallEntries(ctx, query)
}

// InsertRecallEntry writes one entry and its evidence through the guarded Bun
// transaction.
func (s *BunStore) InsertRecallEntry(entry RecallEntry) (string, error) {
	ctx := context.Background()
	var id string
	err := s.update(ctx, WriteRecall, func(store bun.IDB) error {
		if err := normalizeRecallEntryReviewState(&entry); err != nil {
			return err
		}
		if entry.ID == "" {
			return fmt.Errorf("recall entry id is required")
		}
		if entry.Status == "" {
			entry.Status = corerecall.StatusAccepted
		}
		if err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return insertRecallEntryTx(ctx, tx, entry)
		}); err != nil {
			return fmt.Errorf("inserting recall entry: %w", err)
		}
		id = entry.ID
		return nil
	})
	return id, err
}

// RecordRecallQueryEvent inserts a completed query snapshot and exposures
// atomically through Bun.
func (s *BunStore) RecordRecallQueryEvent(
	ctx context.Context, event RecallQueryEvent,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var id string
	err := s.update(ctx, WriteRecall, func(store bun.IDB) error {
		event.QueryID = strings.TrimSpace(event.QueryID)
		event.Surface = strings.TrimSpace(event.Surface)
		event.ScorePolicyVersion = strings.TrimSpace(event.ScorePolicyVersion)
		if event.Surface == "" {
			return fmt.Errorf("recall query surface is required")
		}
		if event.FiltersJSON == "" {
			event.FiltersJSON = "{}"
		}
		if event.ScorePolicyVersion == "" {
			event.ScorePolicyVersion = RecallLexicalScorePolicyVersion
		}
		if event.QueryID == "" {
			generated, err := newUUIDv4()
			if err != nil {
				return fmt.Errorf("generating recall query id: %w", err)
			}
			event.QueryID = generated
		}
		if err := store.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recall_query_events (
					id, query_text, surface, filters_json, trusted_only,
					score_policy_version, result_count, packed_count,
					top_score, miss_reason
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				event.QueryID, event.Query, event.Surface, event.FiltersJSON,
				event.TrustedOnly, event.ScorePolicyVersion, event.ResultCount,
				event.PackedCount, event.TopScore, event.MissReason,
			); err != nil {
				return fmt.Errorf("inserting recall query event: %w", err)
			}
			for start := 0; start < len(event.Exposures); start += recallExposureInsertBatchSize {
				end := min(start+recallExposureInsertBatchSize, len(event.Exposures))
				batch := event.Exposures[start:end]
				if err := insertRecallQueryExposureBatch(
					ctx, tx, event.QueryID, batch,
				); err != nil {
					return fmt.Errorf(
						"inserting recall query exposure ranks %d through %d: %w",
						batch[0].Rank, batch[len(batch)-1].Rank, err,
					)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("recording recall query event: %w", err)
		}
		id = event.QueryID
		return nil
	})
	return id, err
}

// ImportAcceptedRecallEntriesJSONL imports accepted probe results through the
// SQLite Recall capability.
func (s *BunStore) ImportAcceptedRecallEntriesJSONL(
	ctx context.Context, reader io.Reader,
) (RecallImportResult, error) {
	return s.ImportAcceptedRecallEntriesJSONLWithOptions(
		ctx, reader, RecallImportOptions{},
	)
}

func (s *BunStore) ImportAcceptedRecallEntriesJSONLWithOptions(
	ctx context.Context, reader io.Reader, options RecallImportOptions,
) (RecallImportResult, error) {
	if !options.DryRun && !s.backend.Capabilities().AllowsWrite(WriteRecall) {
		return RecallImportResult{}, ErrReadOnly
	}
	capability, err := s.recallCapability()
	if err != nil {
		return RecallImportResult{}, err
	}
	return capability.ImportAcceptedRecallEntriesJSONLWithOptions(
		ctx, reader, options,
	)
}

// IngestEvalTrajectory routes FTS-indexed eval ingestion through the local
// Recall capability after the common write-policy check.
func (s *BunStore) IngestEvalTrajectory(
	ctx context.Context, input EvalTrajectoryIngest,
) (EvalTrajectoryIngestResult, error) {
	if !s.backend.Capabilities().AllowsWrite(WriteRecall) {
		return EvalTrajectoryIngestResult{}, ErrReadOnly
	}
	capability, err := s.recallCapability()
	if err != nil {
		return EvalTrajectoryIngestResult{}, err
	}
	return capability.IngestEvalTrajectory(ctx, input)
}

func listRecallEvidenceBun(
	ctx context.Context, store bun.IDB, ids []string,
) (map[string][]RecallEvidence, error) {
	result := make(map[string][]RecallEvidence, len(ids))
	for start := 0; start < len(ids); start += bunMutationBatchSize {
		end := min(start+bunMutationBatchSize, len(ids))
		var evidence []RecallEvidence
		if err := store.NewRaw(`
			SELECT id, entry_id, session_id, message_start_ordinal,
				message_end_ordinal, message_start_source_uuid,
				message_end_source_uuid, content_digest, tool_use_id, snippet
			FROM recall_evidence
			WHERE entry_id IN (?)
			ORDER BY entry_id ASC, id ASC`, bun.List(ids[start:end])).
			Scan(ctx, &evidence); err != nil {
			return nil, fmt.Errorf("querying recall evidence: %w", err)
		}
		for _, row := range evidence {
			result[row.EntryID] = append(result[row.EntryID], row)
		}
	}
	return result, nil
}
