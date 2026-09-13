package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"hash"
	"os"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/secrets"
)

type parityOwnerStore interface {
	rawderive.ParityQueue
	rawderive.ParityEvidence
	BindParity(context.Context, rawderive.ParityLease, rawderive.ParityBinding) (rawderive.ParityLease, error)
	CaptureParityBaseline(context.Context, rawderive.ParityLease, *sql.DB, rawderive.ParityBinding) error
	ValidateParityEvidence(context.Context, rawderive.ParityLease, *sql.DB, rawderive.ParityBinding, int) error
	ReadParityReport(context.Context, string) (rawderive.ParityReport, error)
}

type parityBoundParser interface {
	rawderive.SourceParser
	BuildIdentity() (rawderive.ParityDigest, error)
	RevalidateExecutable() error
	Close() error
}

type parityOwnerFailure struct{ code string }

func (e parityOwnerFailure) Error() string { return e.code }

type pgMigrationParityOwner struct {
	store        parityOwnerStore
	parser       parityBoundParser
	manifests    rawderive.ExactManifestSource
	materializer rawderive.SourceMaterializer
	content      ingest.ContentOptions
	versions     rawderive.ParityVersions
	owner        string
	tenant       string
	runtimeID    string

	leaseDuration, heartbeatInterval time.Duration
	attemptTimeout, snapshotTimeout  time.Duration
	openBaseline                     func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error)
}

func (o *pgMigrationParityOwner) runOne(ctx context.Context) error {
	if err := o.parser.RevalidateExecutable(); err != nil {
		return parityOwnerFailure{code: "sandbox_unavailable"}
	}
	lease, err := o.store.ClaimParity(ctx, o.owner, o.leaseDuration)
	if err != nil || lease == nil {
		return err
	}
	overall := time.Duration(lease.BatchSize+2)*o.attemptTimeout + 2*o.snapshotTimeout
	workCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	heartbeatErr := make(chan error, 1)
	heartbeatLease := *lease
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Go(func() {
		ticker := time.NewTicker(o.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if heartbeat := o.store.HeartbeatParity(workCtx, heartbeatLease, o.leaseDuration); heartbeat != nil {
					if workCtx.Err() != nil && (errors.Is(heartbeat, context.Canceled) || errors.Is(heartbeat, context.DeadlineExceeded)) {
						return
					}
					select {
					case heartbeatErr <- heartbeat:
					default:
					}
					cancel()
					return
				}
			}
		}
	})
	code, workErr := o.runLease(workCtx, lease)
	cancel()
	heartbeatWG.Wait()
	select {
	case err = <-heartbeatErr:
		return err
	default:
	}
	if workErr != nil {
		var failure parityOwnerFailure
		if errors.As(workErr, &failure) && ctx.Err() == nil {
			return o.store.FinishParityRequest(ctx, *lease, failure.code)
		}
		return workErr
	}
	if err = o.parser.RevalidateExecutable(); err != nil {
		return o.store.FinishParityRequest(ctx, *lease, "binding_conflict")
	}
	return o.store.FinishParityRequest(ctx, *lease, code)
}

func (o *pgMigrationParityOwner) runLease(ctx context.Context, lease *rawderive.ParityLease) (string, error) {
	if o.runtimeID != "" && lease.Request.RuntimeID != o.runtimeID {
		return "", parityOwnerFailure{code: "binding_conflict"}
	}
	baseline, baselineID, baselineConfig, err := o.openBaseline(ctx, lease.Request.BaselineProfile)
	if err != nil {
		return "", err
	}
	if baseline != nil {
		defer baseline.Close()
	}
	build, err := o.parser.BuildIdentity()
	if err != nil || build != o.versions.ParserBuild {
		return "", parityOwnerFailure{code: "binding_conflict"}
	}
	observedAt := lease.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	binding := rawderive.ParityBinding{Request: lease.Request, BaselineID: baselineID, BaselineConfig: baselineConfig,
		Tenant: o.tenant, Versions: o.versions, ObservedAt: observedAt}
	*lease, err = o.store.BindParity(ctx, *lease, binding)
	if err != nil {
		return "", parityOwnerFailure{code: "binding_conflict"}
	}
	report, err := o.store.ReadParityReport(ctx, lease.RunID)
	if err != nil {
		return "", err
	}
	if !report.BaselineSealed {
		snapshotCtx, cancel := context.WithTimeout(ctx, o.snapshotTimeout)
		err = o.store.CaptureParityBaseline(snapshotCtx, *lease, baseline, binding)
		deadline := errors.Is(err, context.DeadlineExceeded)
		cancel()
		if deadline {
			return "", parityOwnerFailure{code: "snapshot_timeout"}
		}
		if err != nil {
			return "", err
		}
	}
	if lease.BatchSize > 0 {
		worker, workerErr := rawderive.NewParityWorker(rawderive.ParityWorkerConfig{Evidence: o.store, Manifests: o.manifests,
			Materializer: o.materializer, Parser: o.parser, Content: o.content, Binding: binding, AttemptTimeout: o.attemptTimeout})
		if workerErr != nil {
			return "", parityOwnerFailure{code: "binding_conflict"}
		}
		if _, err = worker.RunBatch(ctx, *lease); err != nil {
			return "", err
		}
	}
	report, err = o.store.ReadParityReport(ctx, lease.RunID)
	if err != nil {
		return "", err
	}
	if report.PendingSources == 0 {
		snapshotCtx, cancel := context.WithTimeout(ctx, o.snapshotTimeout)
		err = o.store.ValidateParityEvidence(snapshotCtx, *lease, baseline, binding, 128)
		deadline := errors.Is(err, context.DeadlineExceeded)
		cancel()
		if deadline {
			return "", parityOwnerFailure{code: "snapshot_timeout"}
		}
		if err != nil {
			return "", err
		}
	}
	if err = o.parser.RevalidateExecutable(); err != nil {
		return "", parityOwnerFailure{code: "binding_conflict"}
	}
	return "pending", nil
}

func newPGMigrationParityOwner(ctx context.Context, app config.Config, pg config.PGConfig, database *sql.DB, custody *pgRawSyncCustody) (*pgMigrationParityOwner, error) {
	if err := pg.ValidateParity(app.RequireAuth); err != nil {
		return nil, err
	}
	poll, attempt, snapshot := pg.ParityWorkerBounds()
	_ = poll
	parser, err := rawderive.NewBoundSubprocessParser(time.Duration(attempt) * time.Second)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pgMigrationParityOwner, error) { _ = parser.Close(); return nil, err }
	if err = parser.Preflight(ctx); err != nil {
		return fail(err)
	}
	build, err := parser.BuildIdentity()
	if err != nil {
		return fail(err)
	}
	store, err := postgres.NewMigrationParityStore(ctx, database, postgres.MigrationParityOptions{Schema: pg.Schema, Tenant: pg.RawTenant})
	if err != nil {
		return fail(err)
	}
	var runtimeID string
	if err = database.QueryRowContext(ctx, `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&runtimeID); err != nil {
		return fail(err)
	}
	content := hostedRawProjectionOptions(app, pg.RawTenant, rawderive.RetryPolicy{}).Content
	versions := rawderive.ParityVersions{ParserBuild: build, Data: db.CurrentDataVersion(), Preparation: rawderive.ParityPreparationVersion,
		Projection: rawderive.ParityProjectionVersion, Comparison: rawderive.ParitySchemaVersion,
		Quality: db.CurrentQualitySignalVersion, SecretRules: secrets.DefiniteRulesVersion(), Policy: parityPolicyDigest(content)}
	owner := &pgMigrationParityOwner{store: store, parser: parser,
		manifests:    rawderive.ManifestLoader{Store: custody, Limits: custody.limits},
		materializer: rawderive.Materializer{Store: custody, BaseDir: os.TempDir(), MaxTotalBytes: 512 << 20},
		content:      content, versions: versions, owner: "parity-" + runtimeID, tenant: pg.RawTenant, runtimeID: runtimeID,
		leaseDuration: time.Minute, heartbeatInterval: 10 * time.Second,
		attemptTimeout: time.Duration(attempt) * time.Second, snapshotTimeout: time.Duration(snapshot) * time.Second,
	}
	owner.openBaseline = func(openCtx context.Context, profileName string) (*sql.DB, string, rawderive.ParityDigest, error) {
		resolved, profile, resolveErr := app.ResolveParityBaseline(pg, profileName)
		if resolveErr != nil {
			return nil, "", rawderive.ParityDigest{}, parityOwnerFailure{code: "profile_unavailable"}
		}
		baseline, openErr := postgres.OpenHostedContext(openCtx, resolved.Config.URL, resolved.Config.Schema, resolved.Config.RawTenant, resolved.Config.AllowInsecure)
		if openErr != nil {
			return nil, "", rawderive.ParityDigest{}, parityOwnerFailure{code: "baseline_unprovisioned"}
		}
		var identity string
		if queryErr := baseline.QueryRowContext(openCtx, `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity); queryErr != nil || identity != profile.Identity {
			baseline.Close()
			return nil, "", rawderive.ParityDigest{}, parityOwnerFailure{code: "baseline_unprovisioned"}
		}
		digest, digestErr := postgres.ParityTargetConfigDigest(resolved.Config.URL, resolved.Config.Schema, resolved.Config.RawTenant, resolved.Name, identity, resolved.Config.AllowInsecure)
		if digestErr != nil {
			baseline.Close()
			return nil, "", rawderive.ParityDigest{}, parityOwnerFailure{code: "baseline_unprovisioned"}
		}
		return baseline, identity, digest, nil
	}
	return owner, nil
}

func parityPolicyDigest(content ingest.ContentOptions) rawderive.ParityDigest {
	h := sha256.New()
	parityPolicyField(h, "agentsview-parity-policy-v1")
	parityPolicyField(h, string(content.ArchiveContent))
	parityPolicyField(h, string(content.ToolResultImages))
	categories := make([]string, 0, len(content.BlockedResultCategories))
	for category, blocked := range content.BlockedResultCategories {
		if blocked {
			categories = append(categories, category)
		}
	}
	sort.Strings(categories)
	for _, category := range categories {
		parityPolicyField(h, category)
	}
	parityPolicyField(h, db.ClassifierHash())
	var digest rawderive.ParityDigest
	copy(digest[:], h.Sum(nil))
	return digest
}
func parityPolicyField(h hash.Hash, value string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(value)))
	h.Write(n[:])
	h.Write([]byte(value))
}
