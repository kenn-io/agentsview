package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestParityRuntimeUsesPersistedMicrosecondClockAndFinishesFiniteRequest(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	lease := parityRuntimeLease(binding)
	lease.ObservedAt = time.Date(2026, 9, 12, 10, 11, 12, 345678000, time.UTC)
	store := &parityRuntimeStoreFixture{lease: &lease, report: rawderive.ParityReport{BaselineSealed: true}}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild}
	owner := &pgMigrationParityOwner{
		store: store, parser: parser,
		manifests: parityRuntimeManifestFixture{}, materializer: parityRuntimeMaterializerFixture{},
		content: ingest.ContentOptions{}, owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute,
		heartbeatInterval: time.Hour, attemptTimeout: time.Second, snapshotTimeout: time.Second,
		versions: binding.Versions,
		openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
			return nil, binding.BaselineID, binding.BaselineConfig, nil
		},
	}

	require.NoError(t, owner.runOne(t.Context()))
	require.Len(t, store.bound, 1)
	assert.Equal(t, lease.ObservedAt, store.bound[0].ObservedAt)
	assert.Equal(t, 0, store.bound[0].ObservedAt.Nanosecond()%1000)
	assert.Equal(t, []string{"pending"}, store.finished)
	assert.Equal(t, 1, store.validated)
	assert.GreaterOrEqual(t, parser.revalidations, 2)
}

func TestParityRuntimeUnknownProfileFinishesWithoutOpeningCorpusOrParsing(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	lease := parityRuntimeLease(binding)
	store := &parityRuntimeStoreFixture{lease: &lease}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild}
	owner := &pgMigrationParityOwner{
		store: store, parser: parser,
		manifests: parityRuntimeManifestFixture{}, materializer: parityRuntimeMaterializerFixture{},
		owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute, heartbeatInterval: time.Hour,
		attemptTimeout: time.Second, snapshotTimeout: time.Second, versions: binding.Versions,
		openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
			return nil, "", rawderive.ParityDigest{}, parityOwnerFailure{code: "profile_unavailable"}
		},
	}
	require.NoError(t, owner.runOne(t.Context()))
	assert.Equal(t, []string{"profile_unavailable"}, store.finished)
	assert.Empty(t, store.bound)
	assert.Zero(t, store.validated)
}

func TestParityRuntimeExecutableReplacementAfterValidationFinishesNonpassing(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	lease := parityRuntimeLease(binding)
	lease.BatchSize = 0
	store := &parityRuntimeStoreFixture{lease: &lease, report: rawderive.ParityReport{BaselineSealed: true}}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild, failRevalidation: 3}
	owner := &pgMigrationParityOwner{store: store, parser: parser,
		manifests: parityRuntimeManifestFixture{}, materializer: parityRuntimeMaterializerFixture{},
		owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute, heartbeatInterval: time.Hour,
		attemptTimeout: time.Second, snapshotTimeout: time.Second, versions: binding.Versions,
		openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
			return nil, binding.BaselineID, binding.BaselineConfig, nil
		},
	}
	require.NoError(t, owner.runOne(t.Context()))
	assert.Equal(t, 1, store.validated)
	assert.Equal(t, []string{"binding_conflict"}, store.finished)
}

func TestParityRuntimeHeartbeatLossCancelsActiveParserBeforePublication(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	manifest := parityRuntimeManifest(t)
	source := rawderive.ParitySource{ID: rawderive.SourceID(manifest), Identity: manifest.Identity, HeadManifestID: manifest.ManifestID, HeadGeneration: 1, DependencyDigest: rawderive.ParityDigest{1}, Required: true}
	lease := parityRuntimeLease(binding)
	lease.BatchSize = 1
	parserEntered := make(chan struct{})
	store := &parityRuntimeStoreFixture{lease: &lease, report: rawderive.ParityReport{BaselineSealed: true}, sources: []rawderive.ParitySource{source}, history: map[string][]rawderive.ParityHistoryEntry{source.ID: {{ManifestID: manifest.ManifestID, Generation: 1}}}, heartbeatErr: errors.New("lease lost"), heartbeatWaitFor: parserEntered}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild, parse: func(ctx context.Context) (rawderive.ParsedManifest, error) {
		close(parserEntered)
		<-ctx.Done()
		return rawderive.ParsedManifest{}, ctx.Err()
	}}
	owner := &pgMigrationParityOwner{store: store, parser: parser,
		manifests: parityRuntimeManifestFixture{manifest: manifest}, materializer: parityRuntimeMaterializerFixture{},
		owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute, heartbeatInterval: time.Millisecond,
		attemptTimeout: time.Minute, snapshotTimeout: time.Second, versions: binding.Versions,
		openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
			return nil, binding.BaselineID, binding.BaselineConfig, nil
		},
	}
	err := owner.runOne(t.Context())
	require.ErrorContains(t, err, "lease lost")
	assert.Zero(t, store.recorded)
	assert.Empty(t, store.finished)
}

func TestParityRuntimeRestartReusesClockAndRefusesChangedVersion(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	lease := parityRuntimeLease(binding)
	lease.BatchSize = 0
	lease.ObservedAt = time.Date(2026, 9, 12, 10, 11, 12, 345678000, time.UTC)
	store := &parityRuntimeStoreFixture{lease: &lease, report: rawderive.ParityReport{BaselineSealed: true}}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild}
	owner := &pgMigrationParityOwner{store: store, parser: parser, manifests: parityRuntimeManifestFixture{}, materializer: parityRuntimeMaterializerFixture{}, owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute, heartbeatInterval: time.Hour, attemptTimeout: time.Second, snapshotTimeout: time.Second, versions: binding.Versions, openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
		return nil, binding.BaselineID, binding.BaselineConfig, nil
	}}
	require.NoError(t, owner.runOne(t.Context()))
	require.NotNil(t, store.persistedBinding)
	assert.Equal(t, lease.ObservedAt, store.persistedBinding.ObservedAt)

	restart := lease
	restart.Token = "00000000-0000-4000-8000-000000000075"
	store.lease = &restart
	owner.versions.ParserBuild[0]++
	parser.identity = owner.versions.ParserBuild
	require.NoError(t, owner.runOne(t.Context()))
	assert.Equal(t, []string{"pending", "binding_conflict"}, store.finished)
	assert.Equal(t, lease.ObservedAt, store.persistedBinding.ObservedAt)
}

func TestParityRuntimeOwnCancellationDoesNotMasqueradeAsHeartbeatLoss(t *testing.T) {
	binding := parityWorkerRuntimeBinding()
	lease := parityRuntimeLease(binding)
	lease.BatchSize = 0
	started := make(chan struct{})
	store := &parityRuntimeStoreFixture{lease: &lease, report: rawderive.ParityReport{BaselineSealed: true}, heartbeatWait: true, heartbeatStarted: started, validateWait: started}
	parser := &parityRuntimeParserFixture{identity: binding.Versions.ParserBuild}
	owner := &pgMigrationParityOwner{store: store, parser: parser, manifests: parityRuntimeManifestFixture{}, materializer: parityRuntimeMaterializerFixture{}, owner: "parity-owner", tenant: binding.Tenant, leaseDuration: time.Minute, heartbeatInterval: time.Millisecond, attemptTimeout: time.Second, snapshotTimeout: time.Second, versions: binding.Versions, openBaseline: func(context.Context, string) (*sql.DB, string, rawderive.ParityDigest, error) {
		return nil, binding.BaselineID, binding.BaselineConfig, nil
	}}
	require.NoError(t, owner.runOne(t.Context()))
	assert.Equal(t, []string{"pending"}, store.finished)
}

type parityRuntimeStoreFixture struct {
	mu               sync.Mutex
	lease            *rawderive.ParityLease
	report           rawderive.ParityReport
	bound            []rawderive.ParityBinding
	finished         []string
	validated        int
	sources          []rawderive.ParitySource
	history          map[string][]rawderive.ParityHistoryEntry
	heartbeatErr     error
	heartbeatWaitFor <-chan struct{}
	heartbeatWait    bool
	heartbeatStarted chan struct{}
	validateWait     <-chan struct{}
	recorded         int
	persistedBinding *rawderive.ParityBinding
}

func (s *parityRuntimeStoreFixture) ClaimParity(context.Context, string, time.Duration) (*rawderive.ParityLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lease
	s.lease = nil
	return l, nil
}

func (s *parityRuntimeStoreFixture) HeartbeatParity(ctx context.Context, _ rawderive.ParityLease, _ time.Duration) error {
	if s.heartbeatWaitFor != nil {
		select {
		case <-s.heartbeatWaitFor:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.heartbeatWait {
		close(s.heartbeatStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	return s.heartbeatErr
}
func (s *parityRuntimeStoreFixture) FinishParityRequest(_ context.Context, _ rawderive.ParityLease, code string) error {
	s.finished = append(s.finished, code)
	return nil
}
func (s *parityRuntimeStoreFixture) BindParity(_ context.Context, lease rawderive.ParityLease, binding rawderive.ParityBinding) (rawderive.ParityLease, error) {
	d, err := rawderive.DigestParityBinding(binding)
	if err != nil {
		return lease, err
	}
	if s.persistedBinding != nil {
		persisted, digestErr := rawderive.DigestParityBinding(*s.persistedBinding)
		if digestErr != nil {
			return lease, digestErr
		}
		if persisted != d {
			return lease, errors.New("binding_conflict")
		}
	} else {
		copy := binding
		s.persistedBinding = &copy
	}
	lease.BindingDigest = d
	lease.ObservedAt = binding.ObservedAt
	s.bound = append(s.bound, binding)
	return lease, nil
}
func (s *parityRuntimeStoreFixture) CaptureParityBaseline(context.Context, rawderive.ParityLease, *sql.DB, rawderive.ParityBinding) error {
	s.report.BaselineSealed = true
	return nil
}
func (s *parityRuntimeStoreFixture) ValidateParityEvidence(context.Context, rawderive.ParityLease, *sql.DB, rawderive.ParityBinding, int) error {
	if s.validateWait != nil {
		<-s.validateWait
	}
	s.validated++
	return nil
}
func (s *parityRuntimeStoreFixture) ReadParityReport(context.Context, string) (rawderive.ParityReport, error) {
	return s.report, nil
}
func (s *parityRuntimeStoreFixture) NextParitySources(context.Context, rawderive.ParityLease, string, int) ([]rawderive.ParitySource, error) {
	return s.sources, nil
}
func (s *parityRuntimeStoreFixture) ReserveParitySource(context.Context, rawderive.ParityLease, rawderive.ParitySource) error {
	return nil
}
func (s *parityRuntimeStoreFixture) NextParityHistory(_ context.Context, _ rawderive.ParityLease, sourceID string, _ int64, _ int) ([]rawderive.ParityHistoryEntry, error) {
	return s.history[sourceID], nil
}
func (s *parityRuntimeStoreFixture) PrepareParityGraph(_ context.Context, _ rawderive.ParityLease, _ rawderive.ParitySource, g rawderive.ParityGraph) (rawderive.ParityGraph, error) {
	return g, nil
}
func (s *parityRuntimeStoreFixture) RecordParitySource(context.Context, rawderive.ParityLease, rawderive.ParitySource, rawderive.ParitySourceResult) error {
	s.recorded++
	return nil
}

type parityRuntimeParserFixture struct {
	identity         rawderive.ParityDigest
	revalidations    int
	failRevalidation int
	parse            func(context.Context) (rawderive.ParsedManifest, error)
	parseTree        func(context.Context, *rawderive.Materialization) (rawderive.ParsedManifest, error)
	close            func() error
}

func (p *parityRuntimeParserFixture) Parse(ctx context.Context, _ rawsync.CanonicalManifest, tree *rawderive.Materialization) (rawderive.ParsedManifest, error) {
	if p.parseTree != nil {
		return p.parseTree(ctx, tree)
	}
	if p.parse != nil {
		return p.parse(ctx)
	}
	return rawderive.ParsedManifest{}, errors.New("unexpected parse")
}
func (p *parityRuntimeParserFixture) BuildIdentity() (rawderive.ParityDigest, error) {
	return p.identity, nil
}
func (p *parityRuntimeParserFixture) RevalidateExecutable() error {
	p.revalidations++
	if p.failRevalidation == p.revalidations {
		return rawderive.ErrSandboxUnavailable
	}
	return nil
}
func (p *parityRuntimeParserFixture) Close() error {
	if p.close != nil {
		return p.close()
	}
	return nil
}

type parityRuntimeManifestFixture struct{ manifest rawsync.CanonicalManifest }

func (f parityRuntimeManifestFixture) LoadManifest(_ context.Context, identity rawsync.AuthIdentity, id string) (rawsync.CanonicalManifest, error) {
	if f.manifest.Identity == identity && f.manifest.ManifestID == id {
		return f.manifest, nil
	}
	return rawsync.CanonicalManifest{}, rawsync.ErrNotFound
}

type parityRuntimeMaterializerFixture struct{}

func (parityRuntimeMaterializerFixture) Materialize(context.Context, rawsync.CanonicalManifest) (*rawderive.Materialization, error) {
	return &rawderive.Materialization{}, nil
}

type parityRuntimeObjectFixture struct{ body []byte }

func (f parityRuntimeObjectFixture) CopyObject(_ context.Context, _ string, ref rawsync.ObjectRef, dst io.Writer) (rawsync.ObjectInfo, error) {
	if int64(len(f.body)) != ref.Length {
		return rawsync.ObjectInfo{}, rawsync.ErrNotFound
	}
	if _, err := dst.Write(f.body); err != nil {
		return rawsync.ObjectInfo{}, err
	}
	return rawsync.ObjectInfo{Ref: ref}, nil
}

func parityRuntimeManifest(t *testing.T) rawsync.CanonicalManifest {
	t.Helper()
	identity, err := rawsync.NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	body := []byte("fixture")
	digest := sha256.Sum256(body)
	ref, err := rawsync.NewObjectRef(fmt.Sprintf("%x", digest), int64(len(body)))
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parityWorkerRuntimeBinding().Request.Cohort.Provider, ConfiguredRootID: "root-a", SourceKey: "source-a", CaptureID: "capture-a", CapturedAt: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{{Path: "source.jsonl", Type: "file", Length: int64(len(body)), Objects: []rawsync.ObjectRef{ref}}}}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	return manifest
}

func parityWorkerRuntimeBinding() rawderive.ParityBinding {
	return rawderive.ParityBinding{
		Request:    rawderive.ParityRequest{RunID: "00000000-0000-4000-8000-000000000071", RuntimeID: "00000000-0000-4000-8000-000000000072", BaselineProfile: "before", Cohort: rawderive.ParityCohort{DeviceID: "device-a", Provider: "claude", RootID: "root-a"}},
		BaselineID: "00000000-0000-4000-8000-000000000073", BaselineConfig: rawderive.ParityDigest{3}, Tenant: "tenant-a",
		Versions:   rawderive.ParityVersions{ParserBuild: rawderive.ParityDigest{4}, Data: 1, Preparation: rawderive.ParityPreparationVersion, Projection: rawderive.ParityProjectionVersion, Comparison: rawderive.ParitySchemaVersion, Quality: 1, SecretRules: "rules", Policy: rawderive.ParityDigest{5}},
		ObservedAt: time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC),
	}
}
func parityRuntimeLease(binding rawderive.ParityBinding) rawderive.ParityLease {
	return rawderive.ParityLease{RunID: binding.Request.RunID, Owner: "parity-owner", Token: "00000000-0000-4000-8000-000000000074", Request: binding.Request, RequestGeneration: 1, InitEpoch: 1, BatchSize: 32}
}
