package filing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
)

func TestImmediateStoredFindingWithoutDigest(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "claude:today", "example", func(s *db.Session) {
		s.StartedAt = new("2026-10-04T00:30:00Z")
		s.EndedAt = nil
	})
	sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:today", Text: "No, edit the configuration first.", Ordinal: new(2)}
	require.NoError(t, d.ReplaceSessionFriction(t.Context(), sig.SubjectID, []db.FrictionFinding{{
		SessionID: sig.SubjectID, Kind: "correction", Text: sig.Text, MessageOrdinal: sig.Ordinal, Fingerprint: sig.Fingerprint(),
	}}, nil, friction.RulesVersion, "synthetic"))
	f := &filing.Filer{Store: d, Archive: d, Location: time.FixedZone("example", -7*60*60), PublicURL: "https://archive.example.test"}
	got, run, err := f.SignalForFingerprint(t.Context(), sig.Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, sig.SubjectID, got.SubjectID)
	assert.Equal(t, sig.Fingerprint(), got.Fingerprint())
	assert.Equal(t, "2026-10-03", run.Date)
	assert.Empty(t, run.DigestURL, "immediate filing must not promise a digest that does not exist")
	_, _, err = f.SignalForFingerprint(t.Context(), "fl1:missing")
	assert.ErrorIs(t, err, filing.ErrSignalNotFound)
	require.NoError(t, d.SoftDeleteSession(t.Context(), sig.SubjectID))
	_, _, err = f.SignalForFingerprint(t.Context(), sig.Fingerprint())
	assert.ErrorIs(t, err, filing.ErrSignalNotFound)
}

func TestPlanIncludesBoundedRedactedTranscript(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "claude:context", "example")
	dbtest.SeedSession(t, d, "claude:other", "example")
	dbtest.SeedMessages(t, d,
		db.Message{SessionID: "claude:context", Ordinal: 0, Role: "system", Content: "hidden instruction", IsSystem: true},
		db.Message{SessionID: "claude:context", Ordinal: 1, Role: "assistant", Content: "I changed the wrong configuration."},
		db.Message{SessionID: "claude:context", Ordinal: 2, Role: "user", Content: "No, edit the configuration first."},
		db.Message{SessionID: "claude:context", Ordinal: 3, Role: "assistant", Content: "AWS_KEY=" + "AKIA" + "IOSFODNN7EXAMPLE " + strings.Repeat("雪", 1000)},
		db.Message{SessionID: "claude:context", Ordinal: 4, Role: "assistant", Content: "I will correct the configuration."},
		db.Message{SessionID: "claude:context", Ordinal: 5, Role: "assistant", Content: "outside window"},
		db.Message{SessionID: "claude:other", Ordinal: 2, Role: "user", Content: "unrelated transcript"},
	)
	f := &filing.Filer{Archive: d}
	p, err := f.PlanWithContext(t.Context(), friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:context", Text: "No, edit the configuration first.", Ordinal: new(2)}, filing.RunContext{Date: "2026-10-03"})
	require.NoError(t, err)
	assert.Contains(t, p.Body, "## Transcript context")
	assert.Contains(t, p.Body, "Message 1 (assistant)")
	assert.Contains(t, p.Body, "I changed the wrong configuration.")
	assert.Contains(t, p.Body, "Message 2 (user)")
	assert.NotContains(t, p.Body, "hidden instruction")
	assert.NotContains(t, p.Body, "unrelated transcript")
	assert.NotContains(t, p.Body, "outside window")
	assert.NotContains(t, p.Body, "AKIA"+"IOSFODNN7EXAMPLE")
	assert.Contains(t, p.Body, "[truncated]")
	require.Contains(t, p.Body, "Message 3 (assistant):\n")
	longExcerpt := strings.Split(strings.Split(p.Body, "Message 3 (assistant):\n")[1], "Message 4 (assistant):")[0]
	longExcerpt = strings.TrimSpace(strings.TrimPrefix(longExcerpt, "> "))
	assert.LessOrEqual(t, len(longExcerpt), 2000)
	assert.Less(t, len(p.Body), 11000)
}

type unavailableEvidence struct{ filing.ArchiveStore }

func (unavailableEvidence) GetSession(context.Context, string) (*db.Session, error) {
	return &db.Session{}, nil
}

func (unavailableEvidence) GetMessagesWindow(context.Context, string, db.MessageWindow) ([]db.Message, error) {
	return nil, errors.New("archive unavailable")
}

func TestPlanDoesNotSilentlyDropUnavailableEvidence(t *testing.T) {
	f := &filing.Filer{Archive: unavailableEvidence{}}
	_, err := f.PlanWithContext(t.Context(), friction.Signal{SubjectKind: friction.SubjectSession, SubjectID: "claude:context", Ordinal: new(1)}, filing.RunContext{})
	require.ErrorContains(t, err, "archive unavailable")
}

func TestPlanRejectsTrashedTranscriptFromFrozenSignal(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "claude:trashed", "example")
	dbtest.SeedMessages(t, d, db.Message{SessionID: "claude:trashed", Ordinal: 1, Role: "user", Content: "private archived context"})
	sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:trashed", Text: "No, this is wrong.", Ordinal: new(1)}
	require.NoError(t, d.SoftDeleteSession(t.Context(), sig.SubjectID))
	f := &filing.Filer{Archive: d}
	plan, err := f.PlanWithContext(t.Context(), sig, filing.RunContext{Date: "2026-10-03"})
	assert.ErrorIs(t, err, filing.ErrSignalNotFound)
	assert.Empty(t, plan.Body)
}
