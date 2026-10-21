package main

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// The loopback-only fixture models the external HTTP boundary, including held
// requests. Its controls exist only in testfixture, never the product server.
func newDocbankFixtureHandler(key string, revise func(context.Context, bool) error) http.Handler {
	var mu sync.Mutex
	counts := map[string]int{}
	waiting := 0
	heldHash := ""
	var release chan struct{}
	mux := http.NewServeMux()
	//nolint:kennlint // This test-only external protocol double intentionally does not register product API operations.
	mux.HandleFunc("/__test__/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			switch r.URL.Query().Get("mode") {
			case "hold":
				if release != nil {
					close(release)
				}
				heldHash, release = r.URL.Query().Get("sha256"), make(chan struct{})
			case "release":
				if release != nil {
					close(release)
				}
				heldHash, release = "", nil
			case "revise", "reset":
				if revise != nil {
					if err := revise(r.Context(), r.URL.Query().Get("mode") == "reset"); err != nil {
						http.Error(w, "fixture update failed", http.StatusInternalServerError)
						return
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"counts": counts, "waiting": waiting})
	})
	//nolint:kennlint // This test-only external protocol double intentionally does not register product API operations.
	mux.HandleFunc("GET /api/v1/content-references", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		hash := r.URL.Query().Get("sha256")
		mu.Lock()
		counts[hash]++
		wait := release
		if hash != heldHash {
			wait = nil
		}
		if wait != nil {
			waiting++
		}
		mu.Unlock()
		if wait != nil {
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case <-wait:
			case <-r.Context().Done():
			case <-timer.C:
			}
			mu.Lock()
			waiting--
			mu.Unlock()
			if r.Context().Err() != nil {
				return
			}
		}
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset < 0 || r.URL.Query().Get("limit") != "100" {
			http.Error(w, "invalid page", http.StatusBadRequest)
			return
		}
		items := []any{}
		total := 0
		for i := range fileEvidenceNames {
			if hash != fileEvidenceDigest(i) || i == 3 {
				continue
			}
			total = 1
			if i == 1 {
				total = 2
			}
			if offset >= total {
				break
			}
			node := i + 1 + offset*10
			version := fmt.Sprintf("00000000-0000-4000-8000-%012d", node)
			current := version
			if i == 1 {
				current = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			}
			trashed := ""
			if i == 2 {
				trashed = "2026-01-01T00:00:00Z"
			}
			items = append(items, map[string]any{"version": map[string]any{"id": version, "node_id": node, "blob_hash": hash, "size": len(fileEvidenceBytes(i))}, "node": map[string]any{"id": node, "kind": "file", "current_version_id": current, "name": fileEvidenceNames[i], "trashed_at": trashed}, "path": "/examples/" + fileEvidenceNames[i], "is_current": i != 1})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"items": items, "total": total, "limit": 100, "offset": offset})
	})
	return mux
}

func runDocbankFixture(ctx context.Context, configPath, databasePath string) error {
	var listen net.ListenConfig
	listener, err := listen.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	database, err := db.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	database.SetAssetsDir(filepath.Join(filepath.Dir(databasePath), "assets"))
	keyPath := filepath.Join(filepath.Dir(configPath), "docbank-fixture-key")
	key := rand.Text()
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		return err
	}
	defer os.Remove(keyPath)
	address := "http://" + listener.Addr().String()
	session, err := database.GetSession(ctx, fileEvidenceSession)
	if err != nil {
		return err
	}
	if session == nil || session.StartedAt == nil {
		return errors.New("file evidence fixture session is missing")
	}
	start, err := time.Parse(time.RFC3339Nano, *session.StartedAt)
	if err != nil {
		return err
	}
	handler := newDocbankFixtureHandler(key, func(ctx context.Context, reset bool) error {
		if reset {
			return createFileEvidenceFixture(ctx, database, start)
		}
		return database.ReplaceSessionMessages(ctx, fileEvidenceSession, []db.Message{{SessionID: fileEvidenceSession, Ordinal: 0, Role: "user", Content: "Fixture transcript replaced."}})
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	// Publish readiness only once both database and listener are ready.
	config := fmt.Sprintf("[integrations.docbank]\nenabled = true\nurl = %q\nweb_url = %q\napi_key_file = %q\n", address, address, keyPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
