package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

var projectsHTTPClient = &http.Client{Timeout: 30 * time.Second}

func runProjects(jsonOutput bool) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx := context.Background()
	tr, err := ensureTransport(&appCfg, transportIntentRead, 0)
	if err != nil {
		fatal("resolving transport: %v", err)
	}
	if tr.Mode != transportHTTP {
		fatal("resolving transport: expected daemon transport")
	}
	projects, err := fetchHTTPProjects(
		ctx, tr, appCfg.AuthToken, false, false,
	)
	if err != nil {
		fatal("listing projects: %v", err)
	}

	writeProjects(projects, jsonOutput)
}

func fetchHTTPProjects(
	ctx context.Context,
	tr transport,
	authToken string,
	excludeOneShot bool,
	excludeAutomated bool,
) ([]db.ProjectInfo, error) {
	q := url.Values{}
	q.Set("include_one_shot", strconv.FormatBool(!excludeOneShot))
	q.Set("include_automated", strconv.FormatBool(!excludeAutomated))
	endpoint := strings.TrimSuffix(tr.URL, "/") +
		"/api/v1/projects?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := projectsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"projects: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out struct {
		Projects []db.ProjectInfo `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func writeProjects(projects []db.ProjectInfo, jsonOutput bool) {
	if jsonOutput {
		if projects == nil {
			projects = []db.ProjectInfo{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(projects); err != nil {
			fatal("encoding json: %v", err)
		}
		return
	}

	if len(projects) == 0 {
		fmt.Println("No projects found.")
		return
	}

	fmt.Printf("%-40s %s\n", "PROJECT", "SESSIONS")
	for _, p := range projects {
		name := p.Name
		if name == "" {
			name = "(none)"
		}
		fmt.Printf("%-40s %d\n", name, p.SessionCount)
	}
}
