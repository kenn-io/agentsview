package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"

	native "go.kenn.io/kata"
)

const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"

type ready struct {
	Endpoint  string `json:"endpoint"`
	ProjectID int64  `json:"project_id"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("AGENTSVIEW_TEST_KATA_DSN")
	token := os.Getenv("AGENTSVIEW_TEST_KATA_TOKEN")
	readyPath := os.Getenv("AGENTSVIEW_TEST_KATA_READY")
	if dsn == "" || token == "" || readyPath == "" {
		return errors.New("test service requires DSN, token, and ready file")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	service, err := native.New(ctx, native.Config{DSN: dsn, Auth: native.AuthConfig{Token: token}})
	if err != nil {
		return fmt.Errorf("start Kata service: %w", err)
	}
	defer service.Close()
	project, err := service.EnsureProject(ctx, native.ProjectSpec{UID: projectUID, Name: "agentsview"})
	if err != nil {
		return fmt.Errorf("create Kata test project: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", service.Handler())
	mux.HandleFunc("/__test/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		go stop()
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	data, err := json.Marshal(ready{Endpoint: server.URL, ProjectID: project.Project.ID})
	if err != nil {
		return fmt.Errorf("encode Kata test endpoint: %w", err)
	}
	if err := writeReadyFile(readyPath, data); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func writeReadyFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kata-ready-*")
	if err != nil {
		return fmt.Errorf("create Kata ready file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write Kata ready file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Kata ready file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish Kata ready file: %w", err)
	}
	return nil
}
