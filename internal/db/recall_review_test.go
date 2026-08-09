package db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corerecall "go.kenn.io/agentsview/internal/recall"
)

func TestReviewRecallEntryTransitions(t *testing.T) {
	tests := []struct {
		name       string
		action     RecallReviewAction
		provenance bool
		wantStatus string
		wantReview string
		wantErr    error
	}{
		{
			name: "approve", action: RecallReviewApprove, provenance: true,
			wantStatus: corerecall.StatusAccepted,
			wantReview: corerecall.ReviewStateHumanReviewed,
		},
		{
			name: "approve revoked", action: RecallReviewApprove,
			wantStatus: corerecall.StatusAccepted,
			wantReview: corerecall.ReviewStateUnreviewedAuto,
			wantErr:    ErrRecallReviewProvenance,
		},
		{
			name: "archive", action: RecallReviewArchive, provenance: true,
			wantStatus: corerecall.StatusArchived,
			wantReview: corerecall.ReviewStateHumanRejected,
		},
		{
			name: "archive revoked", action: RecallReviewArchive,
			wantStatus: corerecall.StatusArchived,
			wantReview: corerecall.ReviewStateHumanRejected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			insertSession(t, d, "review-session", "agentsview")
			confidence := 0.73
			_, err := d.InsertRecallEntry(RecallEntry{
				ID: "review-entry", Type: "preference", Scope: "project",
				Status:      corerecall.StatusAccepted,
				ReviewState: corerecall.ReviewStateUnreviewedAuto,
				Title:       "Keep commands concise",
				Body:        "The user prefers short operational commands.",
				Trigger:     "when suggesting shell commands",
				Confidence:  &confidence,
				Uncertainty: "May be task specific",
				Project:     "agentsview", CWD: "/work/agentsview",
				GitBranch: "feature", Agent: "codex",
				SourceSessionID: "review-session", SourceEpisodeID: "episode-1",
				SourceRunID: "run-1", ExtractorMethod: "turns-v1", Model: "model-1",
				Transferable: true, ProvenanceOK: tt.provenance,
				Evidence: []RecallEvidence{{
					SessionID: "review-session", MessageStartOrdinal: 3,
					MessageEndOrdinal: 4, Snippet: "Use the shorter command.",
				}},
			})
			require.NoError(t, err)
			_, err = d.getWriter().Exec(`
				UPDATE recall_entries
				SET created_at = '2026-01-02T03:04:05.000Z',
					updated_at = '2026-01-02T03:04:05.000Z'
				WHERE id = 'review-entry'`)
			require.NoError(t, err)

			before, err := d.GetRecallEntry(ctx, "review-entry")
			require.NoError(t, err)
			require.NotNil(t, before)
			got, err := d.ReviewRecallEntry(ctx, "review-entry", tt.action)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				after, getErr := d.GetRecallEntry(ctx, "review-entry")
				require.NoError(t, getErr)
				require.NotNil(t, after)
				assert.Equal(t, tt.wantStatus, after.Status)
				assert.Equal(t, tt.wantReview, after.ReviewState)
				assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Equal(t, tt.wantReview, got.ReviewState)
			assert.Equal(t, before.Title, got.Title)
			assert.Equal(t, before.Body, got.Body)
			assert.Equal(t, before.Trigger, got.Trigger)
			assert.Equal(t, before.Confidence, got.Confidence)
			assert.Equal(t, before.Uncertainty, got.Uncertainty)
			assert.Equal(t, before.Project, got.Project)
			assert.Equal(t, before.CWD, got.CWD)
			assert.Equal(t, before.GitBranch, got.GitBranch)
			assert.Equal(t, before.Agent, got.Agent)
			assert.Equal(t, before.SourceSessionID, got.SourceSessionID)
			assert.Equal(t, before.SourceEpisodeID, got.SourceEpisodeID)
			assert.Equal(t, before.SourceRunID, got.SourceRunID)
			assert.Equal(t, before.ExtractorMethod, got.ExtractorMethod)
			assert.Equal(t, before.Model, got.Model)
			assert.Equal(t, before.Transferable, got.Transferable)
			assert.Equal(t, before.ProvenanceOK, got.ProvenanceOK)
			assert.Equal(t, before.CreatedAt, got.CreatedAt)
			assert.NotEqual(t, before.UpdatedAt, got.UpdatedAt)
			require.Len(t, got.Evidence, 1)
			assert.Equal(t, "Use the shorter command.", got.Evidence[0].Snippet)
		})
	}
}

func TestReviewRecallEntryRejectsInvalidTransitions(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		action RecallReviewAction
		status string
		review string
		want   error
	}{
		{
			name: "missing", id: "missing", action: RecallReviewApprove,
			status: corerecall.StatusAccepted,
			review: corerecall.ReviewStateUnreviewedAuto,
			want:   ErrRecallEntryNotFound,
		},
		{
			name: "unknown action", id: "entry", action: "publish",
			status: corerecall.StatusAccepted,
			review: corerecall.ReviewStateUnreviewedAuto,
			want:   ErrInvalidRecallReviewAction,
		},
		{
			name: "already approved", id: "entry", action: RecallReviewArchive,
			status: corerecall.StatusAccepted,
			review: corerecall.ReviewStateHumanReviewed,
			want:   ErrRecallReviewConflict,
		},
		{
			name: "already rejected", id: "entry", action: RecallReviewApprove,
			status: corerecall.StatusArchived,
			review: corerecall.ReviewStateHumanRejected,
			want:   ErrRecallReviewConflict,
		},
		{
			name: "archived automatic", id: "entry", action: RecallReviewArchive,
			status: corerecall.StatusArchived,
			review: corerecall.ReviewStateUnreviewedAuto,
			want:   ErrRecallReviewConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "review-session", "agentsview")
			if tt.id != "missing" {
				_, err := d.InsertRecallEntry(RecallEntry{
					ID: tt.id, Type: "fact", Scope: "project",
					Status: tt.status, ReviewState: tt.review,
					Title: "Entry", Body: "Body", ProvenanceOK: true,
					SourceSessionID: "review-session",
				})
				require.NoError(t, err)
			}
			_, err := d.ReviewRecallEntry(context.Background(), tt.id, tt.action)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestReviewRecallEntryAdvancesRelevantRevisions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "review-session", "agentsview")
	insert := func(id string) {
		t.Helper()
		_, err := d.InsertRecallEntry(RecallEntry{
			ID: id, Type: "fact", Scope: "project",
			Status:      corerecall.StatusAccepted,
			ReviewState: corerecall.ReviewStateUnreviewedAuto,
			Title:       "Entry", Body: "Body", ProvenanceOK: true,
			SourceSessionID: "review-session",
		})
		require.NoError(t, err)
	}

	insert("approve-entry")
	queryBefore, err := d.RecallQueryRevision(ctx)
	require.NoError(t, err)
	corpusBefore, err := d.RecallCorpusRevision(ctx)
	require.NoError(t, err)
	_, err = d.ReviewRecallEntry(ctx, "approve-entry", RecallReviewApprove)
	require.NoError(t, err)
	queryAfter, err := d.RecallQueryRevision(ctx)
	require.NoError(t, err)
	corpusAfter, err := d.RecallCorpusRevision(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, queryBefore, queryAfter)
	assert.Equal(t, corpusBefore, corpusAfter)

	insert("archive-entry")
	queryBefore, err = d.RecallQueryRevision(ctx)
	require.NoError(t, err)
	corpusBefore, err = d.RecallCorpusRevision(ctx)
	require.NoError(t, err)
	_, err = d.ReviewRecallEntry(ctx, "archive-entry", RecallReviewArchive)
	require.NoError(t, err)
	queryAfter, err = d.RecallQueryRevision(ctx)
	require.NoError(t, err)
	corpusAfter, err = d.RecallCorpusRevision(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, queryBefore, queryAfter)
	assert.NotEqual(t, corpusBefore, corpusAfter)
}

func TestRecallReviewActionValidate(t *testing.T) {
	assert.NoError(t, RecallReviewApprove.Validate())
	assert.NoError(t, RecallReviewArchive.Validate())
	assert.ErrorIs(t, RecallReviewAction("publish").Validate(),
		ErrInvalidRecallReviewAction)
	assert.False(t, errors.Is(ErrRecallReviewConflict, ErrRecallEntryNotFound))
}
