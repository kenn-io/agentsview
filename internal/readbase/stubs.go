package readbase

import (
	"context"
	"io"

	"go.kenn.io/agentsview/internal/db"
)

// ReadOnlyStubs supplies unsupported archive writes for remote stores.
type ReadOnlyStubs struct{}

func (ReadOnlyStubs) ListRecallEntries(
	_ context.Context, _ db.RecallQuery,
) ([]db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (ReadOnlyStubs) GetRecallEntry(
	_ context.Context, _ string,
) (*db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (ReadOnlyStubs) ReviewRecallEntry(
	_ context.Context, _ string, _ db.RecallReviewAction,
) (db.RecallEntry, error) {
	return db.RecallEntry{}, db.ErrReadOnly
}

func (ReadOnlyStubs) QueryRecallEntries(
	_ context.Context, _ db.RecallQuery,
) (db.RecallPage, error) {
	return db.RecallPage{}, db.ErrReadOnly
}

func (ReadOnlyStubs) RecordRecallQueryEvent(
	_ context.Context, _ db.RecallQueryEvent,
) (string, error) {
	return "", db.ErrReadOnly
}

func (ReadOnlyStubs) InsertRecallEntry(ctx context.Context, _ db.RecallEntry) (string, error) {
	return "", db.ErrReadOnly
}

func (ReadOnlyStubs) ImportAcceptedRecallEntriesJSONL(
	_ context.Context, _ io.Reader,
) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (ReadOnlyStubs) ImportAcceptedRecallEntriesJSONLWithOptions(
	_ context.Context, _ io.Reader, _ db.RecallImportOptions,
) (db.RecallImportResult, error) {
	return db.RecallImportResult{}, db.ErrReadOnly
}

func (ReadOnlyStubs) IngestEvalTrajectory(
	_ context.Context, _ db.EvalTrajectoryIngest,
) (db.EvalTrajectoryIngestResult, error) {
	return db.EvalTrajectoryIngestResult{}, db.ErrReadOnly
}

func (ReadOnlyStubs) UpsertSession(ctx context.Context, _ db.Session) error { return db.ErrReadOnly }

func (ReadOnlyStubs) ReplaceSessionMessages(ctx context.Context, _ string, _ []db.Message) error {
	return db.ErrReadOnly
}

func (ReadOnlyStubs) WriteSessionBatchAtomic(ctx context.Context,
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}

// MirrorStubs supplies unsupported curation and insight operations.
type MirrorStubs struct{ ReadOnlyStubs }

func (MirrorStubs) InsertInsight(ctx context.Context, _ db.Insight) (int64, error) {
	return 0, db.ErrReadOnly
}

func (MirrorStubs) DeleteInsight(ctx context.Context, _ int64) error { return db.ErrReadOnly }

func (MirrorStubs) ListInsights(_ context.Context, _ db.InsightFilter) ([]db.Insight, error) {
	return []db.Insight{}, nil
}

func (MirrorStubs) GetInsight(_ context.Context, _ int64) (*db.Insight, error) { return nil, nil }

func (MirrorStubs) GetCachedInsight(_ context.Context, _ string) (*db.Insight, error) {
	return nil, nil
}

func (MirrorStubs) RenameSession(ctx context.Context, _ string, _ *string) error {
	return db.ErrReadOnly
}

func (MirrorStubs) SoftDeleteSession(ctx context.Context, _ string) error { return db.ErrReadOnly }

func (MirrorStubs) SoftDeleteSessions(ctx context.Context, _ []string) (int, error) {
	return 0, db.ErrReadOnly
}

func (MirrorStubs) RestoreSession(ctx context.Context, _ string) (int64, error) {
	return 0, db.ErrReadOnly
}

func (MirrorStubs) DeleteSessionIfTrashed(ctx context.Context, _ string) (int64, error) {
	return 0, db.ErrReadOnly
}

func (MirrorStubs) EmptyTrash(ctx context.Context) (int, error) { return 0, db.ErrReadOnly }

func (MirrorStubs) StarSession(ctx context.Context, sessionID string) (bool, error) {
	return false, db.ErrReadOnly
}

func (MirrorStubs) UnstarSession(ctx context.Context, sessionID string) error {
	return db.ErrReadOnly
}

func (MirrorStubs) BulkStarSessions(ctx context.Context, sessionIDs []string) error {
	return db.ErrReadOnly
}

func (MirrorStubs) PinMessage(ctx context.Context, sessionID string, messageID int64, note *string) (int64, error) {
	return 0, db.ErrReadOnly
}

func (MirrorStubs) UnpinMessage(ctx context.Context, sessionID string, messageID int64) error {
	return db.ErrReadOnly
}
