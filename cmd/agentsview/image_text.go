package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/imagetext"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/docbank/document/glmocr"
)

func setupImageText(ctx context.Context, cfg config.Config, database *db.DB) (*imagetext.Manager, error) {
	if !cfg.ImageText.Enabled || cfg.ArchiveContent.OmitsToolContent() || database.ArchiveContent().OmitsToolContent() {
		if cfg.ImageText.Enabled {
			log.Print("image_text: disabled because archive_content omits tool results; clearing image text")
		}
		return nil, database.ClearImageText(ctx)
	}
	policy, err := cfg.ImageText.Policy()
	if err != nil {
		return nil, err
	}
	client, err := glmocr.NewClient(policy, glmocr.ClientConfig{})
	if err != nil {
		return nil, err
	}
	processor, err := imagetext.ProcessorKey(policy)
	if err != nil {
		return nil, err
	}
	return &imagetext.Manager{DB: database, Client: client, Processor: processor}, nil
}

func imageTextJob(manager *imagetext.Manager, idle *server.IdleTracker) poller.Job {
	return poller.Job{Name: "image-text", Interval: time.Minute, RunAtStart: true, Run: func(ctx context.Context) error {
		done, ok := idle.BeginWork()
		if !ok {
			return nil
		}
		defer done()
		return manager.Run(ctx)
	}}
}
