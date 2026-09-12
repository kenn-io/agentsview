// Package mcpdiscovery publishes the local HTTP MCP listener for CLI clients.
package mcpdiscovery

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

const service = "agentsview-mcp"

// Endpoint describes an existing listener. TokenPath locates a private file;
// status output never includes the bearer token itself.
type Endpoint struct {
	PID        int    `json:"pid"`
	Transport  string `json:"transport"`
	URL        string `json:"url"`
	BackendURL string `json:"backend_url,omitempty"`
	TokenPath  string `json:"token_path,omitempty"`
}

// Publish runs only after bind succeeds. The caller removes discovery state
// after the listener closes, including shutdown caused by a serving error.
// address is the bound listener address; wildcard hosts use loopback in the URL.
func Publish(directory, address, token, backendURL string) (func() error, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse MCP listener address: %w", err)
	}
	if ip := net.ParseIP(host); ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	store := daemon.RuntimeStore{Dir: directory, Prefix: "mcp"}
	rec := daemon.NewRuntimeRecord(service, "", daemon.Endpoint{Network: "tcp", Address: address})
	rec.Metadata = map[string]string{"url": "http://" + net.JoinHostPort(host, port) + "/mcp", "backend_url": backendURL}
	tokenPath := ""
	if token != "" {
		file, err := os.CreateTemp(directory, "mcp-token-*")
		if err != nil {
			return nil, err
		}
		tokenPath = file.Name()
		_, writeErr := file.WriteString(token)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			_ = os.Remove(tokenPath)
			return nil, err
		}
		rec.Metadata["token_path"] = tokenPath
	}
	recordPath, err := store.Write(rec)
	if err != nil {
		if tokenPath != "" {
			_ = os.Remove(tokenPath)
		}
		return nil, err
	}
	return func() error {
		recordErr := os.Remove(recordPath)
		var tokenErr error
		if tokenPath != "" {
			tokenErr = os.Remove(tokenPath)
		}
		return errors.Join(recordErr, tokenErr)
	}, nil
}

// List is observational: it never starts a daemon or prunes another process's
// records. Dead-process records are omitted, including after an unclean exit.
func List(directory string) ([]Endpoint, error) {
	endpoints := []Endpoint{}
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		return endpoints, nil
	} else if err != nil {
		return nil, err
	}
	if err := safefileio.ValidatePrivateDir(directory); err != nil {
		return nil, err
	}
	records, err := (daemon.RuntimeStore{Dir: filepath.Clean(directory), Prefix: "mcp"}).List()
	if err != nil {
		return nil, fmt.Errorf("read MCP listener status: %w", err)
	}
	for _, rec := range records {
		if rec.Service != service || !daemon.ProcessAlive(rec.PID) {
			continue
		}
		endpoints = append(endpoints, Endpoint{PID: rec.PID, Transport: "http", URL: rec.Metadata["url"], BackendURL: rec.Metadata["backend_url"], TokenPath: rec.Metadata["token_path"]})
	}
	return endpoints, nil
}
