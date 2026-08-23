package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

const (
	pgRawSyncDataDirectory = "raw-sync"
	pgRawSyncTokenTTL      = 15 * time.Minute
)

func preparePGRawSyncServicesIfWritable(
	ctx context.Context,
	dataDir string,
	database *sql.DB,
	schemaWritable bool,
) (server.Option, func() error, error) {
	if !schemaWritable {
		return nil, nil, nil
	}
	return preparePGRawSyncServices(ctx, dataDir, database)
}

func preparePGRawSyncServices(
	_ context.Context,
	dataDir string,
	database *sql.DB,
) (server.Option, func() error, error) {
	metadata, err := postgres.NewRawIngestStore(database)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing raw sync metadata store: %w", err)
	}
	authStore, err := postgres.NewRawDeviceAuthStore(database)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing raw sync device auth store: %w", err)
	}
	auth, err := rawsync.NewDeviceAuthService(authStore, pgRawSyncTokenTTL)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing raw sync device auth service: %w", err)
	}
	custody := &pgRawSyncCustody{
		dataDir:  dataDir,
		metadata: metadata,
		limits:   rawsync.DefaultManifestLimits(),
		version:  fmt.Sprintf("parser-data-%d", db.CurrentDataVersion()),
	}
	uploadOption, closeUploads, err := preparePGRawSyncUploads(
		dataDir, database, custody,
	)
	if err != nil {
		return nil, nil, errors.Join(err, custody.Close())
	}
	return combinePGRawSyncOptions(
			server.WithRawSyncServices(auth, custody), uploadOption,
		), func() error {
			return errors.Join(closeUploads(), custody.Close())
		}, nil
}

// pgRawSyncCustody keeps ordinary pg serve startup independent from the local
// raw-custody vault. The exclusive Docbank repository is opened only when a
// raw-sync custody route is actually used.
type pgRawSyncCustody struct {
	mu sync.Mutex

	dataDir  string
	metadata rawsync.MetadataStore
	limits   rawsync.ManifestLimits
	version  string

	repository *artifact.Repository
	service    *rawsync.Service
	closed     bool
}

func (c *pgRawSyncCustody) MissingObjects(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	provider parser.AgentType,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	service, err := c.openService(ctx)
	if err != nil {
		return nil, err
	}
	return service.MissingObjects(ctx, identity, provider, objects)
}

func (c *pgRawSyncCustody) FinalizeObject(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	provider parser.AgentType,
	object rawsync.ObjectRef,
	body io.Reader,
) (rawsync.PutResult, error) {
	service, err := c.openService(ctx)
	if err != nil {
		return rawsync.PutResult{}, err
	}
	return service.FinalizeObject(ctx, identity, provider, object, body)
}

func (c *pgRawSyncCustody) CommitManifest(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	manifest rawsync.Manifest,
) (rawsync.CommitResult, error) {
	service, err := c.openService(ctx)
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	return service.CommitManifest(ctx, identity, manifest)
}

func (c *pgRawSyncCustody) openService(ctx context.Context) (*rawsync.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, errors.New("raw sync custody is closed")
	}
	if c.service != nil {
		return c.service, nil
	}
	repository, err := artifact.OpenRepository(
		ctx, filepath.Join(c.dataDir, pgRawSyncDataDirectory),
	)
	if err != nil {
		return nil, fmt.Errorf("opening raw sync object repository: %w", err)
	}
	fail := func(err error) (*rawsync.Service, error) {
		return nil, errors.Join(err, repository.Close())
	}
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	if err != nil {
		return fail(fmt.Errorf("preparing raw sync object store: %w", err))
	}
	service, err := rawsync.NewService(objects, c.metadata, c.limits, c.version)
	if err != nil {
		return fail(fmt.Errorf("preparing raw sync custody service: %w", err))
	}
	c.repository = repository
	c.service = service
	return service, nil
}

func (c *pgRawSyncCustody) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.repository == nil {
		return nil
	}
	return c.repository.Close()
}

func preparePGRawSyncUploads(
	dataDir string,
	database *sql.DB,
	custody rawsync.UploadCustody,
) (server.Option, func() error, error) {
	uploadStore, err := postgres.NewRawUploadStore(database, dataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing raw upload session store: %w", err)
	}
	uploads, err := rawsync.NewUploadService(
		uploadStore, custody, rawsync.DefaultUploadSessionTTL,
	)
	if err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("preparing raw upload service: %w", err),
			uploadStore.Close(),
		)
	}
	return server.WithRawSyncUploads(uploads), uploadStore.Close, nil
}

func combinePGRawSyncOptions(options ...server.Option) server.Option {
	return func(target *server.Server) {
		for _, option := range options {
			option(target)
		}
	}
}
