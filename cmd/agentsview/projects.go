package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

func runProjects(jsonOutput bool) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	projects, err := database.GetProjects(ctx, false, false)
	if err != nil {
		fatal("listing projects: %v", err)
	}

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
