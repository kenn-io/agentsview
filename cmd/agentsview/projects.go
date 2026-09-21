package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
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
	api, err := apiclient.NewHTTPClient(tr.URL, authToken, projectsHTTPClient)
	if err != nil {
		return nil, err
	}
	response, err := api.GetAPIV1ProjectsWithResponse(ctx, &apiclient.GetAPIV1ProjectsRequestOptions{Query: &apiclient.GetAPIV1ProjectsQuery{IncludeOneShot: new(!excludeOneShot), IncludeAutomated: new(!excludeAutomated)}})
	if response == nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("projects: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(response.Body)))
	}
	if err != nil {
		return nil, err
	}
	if len(response.Body) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return response.JSON200.Projects, nil
}

func writeProjects(projects []db.ProjectInfo, jsonOutput bool) {
	if jsonOutput {
		if projects == nil {
			projects = []db.ProjectInfo{}
		}
		enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
		if err := json.MarshalEncode(enc, projects); err != nil {
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
