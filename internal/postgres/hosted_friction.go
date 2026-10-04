package postgres

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

// HostedStore cannot expose Friction data until its tables have tenant keys
// and row-level policies. Forwarding these operations to the physical store
// would bypass the hosted tenant boundary.
func (h *HostedStore) FrictionSubjectsForDate(
	context.Context, string, *time.Location, bool,
) ([]db.FrictionSubject, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) FrictionFindingsForSubjects(
	context.Context, []string,
) ([]db.FrictionFinding, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) FrictionUsageForSessions(
	context.Context, []string,
) (map[string]friction.SessionUsage, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) FrictionArchiveSpend(
	context.Context, string, string, *time.Location,
) (*friction.ArchiveSpend, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) SaveFrictionDigest(
	context.Context, db.FrictionDigest, []db.FrictionDigestSubject,
	[]db.FrictionPatternUpdate,
) error {
	return db.ErrReadOnly
}

func (h *HostedStore) GetFrictionDigest(
	context.Context, string,
) (*db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) LatestFrictionDigestDate(context.Context) (string, error) {
	return "", db.ErrReadOnly
}

func (h *HostedStore) EarliestSessionDate(
	context.Context, *time.Location,
) (string, error) {
	return "", db.ErrReadOnly
}

func (h *HostedStore) UpdateFrictionDigestRender(
	context.Context, string, []byte, []byte, int,
) error {
	return db.ErrReadOnly
}

func (h *HostedStore) ListFrictionFindings(
	context.Context, db.FrictionFindingFilter,
) ([]db.FrictionFinding, string, error) {
	return nil, "", db.ErrReadOnly
}

func (h *HostedStore) ListFrictionDigests(
	context.Context, string, string,
) ([]db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) ListFrictionPatterns(
	context.Context, db.FrictionPatternFilter,
) ([]db.FrictionPattern, string, error) {
	return nil, "", db.ErrReadOnly
}

func (h *HostedStore) GetFrictionIssueLinks(
	context.Context, []string,
) (map[string]db.FrictionIssueLink, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) UpsertFrictionIssueLink(context.Context, db.FrictionIssueLink) error {
	return db.ErrReadOnly
}

func (h *HostedStore) DeleteFrictionIssueLink(context.Context, string) error {
	return db.ErrReadOnly
}

func (h *HostedStore) DueFrictionFilings(
	context.Context, time.Time, int,
) ([]db.FrictionIssueLink, error) {
	return nil, db.ErrReadOnly
}

func (h *HostedStore) DigestDatesForFingerprints(
	context.Context, []string,
) ([]string, error) {
	return nil, db.ErrReadOnly
}
