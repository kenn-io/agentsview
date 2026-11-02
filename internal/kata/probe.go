package kata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"
	"go.kenn.io/agentsview/internal/stringutil"
	kg "go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"
)

// MinAPISchemaVersion is the oldest Kata API schema agentsview uses.
const MinAPISchemaVersion = "0.21.0"

type State string

const (
	StateDisabled        State = "disabled"
	StateNotHub          State = "not_hub"
	StateUnavailable     State = "unavailable"
	StateIncompatible    State = "incompatible"
	StateUnauthenticated State = "unauthenticated"
	StateWrongProject    State = "wrong_project"
	StateReady           State = "ready"
)

type Status struct {
	State            State  `json:"state"`
	Project          string `json:"project"`
	InstanceUID      string `json:"instance_uid,omitempty"`
	APISchemaVersion string `json:"api_schema_version,omitempty"`
	Message          string `json:"message,omitempty"`
}

// Probe detects one Kata instance and resolves the configured project.
// It returns a client only when ready.
func Probe(ctx context.Context, cfg Config) (Status, *Client) {
	st := Status{State: StateDisabled, Project: strings.TrimSpace(cfg.Project)}
	if !cfg.Enabled {
		st.Message = "Kata integration is disabled."
		return st, nil
	}
	if !cfg.Hub {
		st.State = StateNotHub
		st.Message = "this instance pushes its archive to PostgreSQL; only the agentsview hub files to Kata"
		return st, nil
	}
	token := cfg.currentToken()
	if token == "" && (cfg.TokenRequired || cfg.TokenEnv != "") {
		st.State = StateUnauthenticated
		st.Message = "no bearer token: set [kata] token_env to the name of an environment variable that holds the token"
		return st, nil
	}
	endpoint, err := resolveEndpoint(ctx, cfg.Endpoint)
	if err != nil {
		return failed(st, StateUnavailable, err, token), nil
	}
	if token == "" && strings.HasPrefix(endpoint, "https://") {
		st.State = StateUnauthenticated
		st.Message = "no bearer token: set [kata] token_env to the name of an environment variable that holds the token"
		return st, nil
	}
	c, err := newClient(cfg, endpoint)
	if err != nil {
		return failed(st, StateUnavailable, err, token), nil
	}
	schema, err := c.detect(ctx)
	st.APISchemaVersion = schema
	if err != nil {
		return failed(st, stateFor(err), err, token), nil
	}
	st.InstanceUID = c.InstanceUID()
	if err := c.resolveProject(ctx); err != nil {
		return failed(st, stateFor(err), err, token), nil
	}
	st.State = StateReady
	return st, c
}

func (cfg Config) currentToken() string {
	if name := strings.TrimSpace(cfg.TokenEnv); name != "" {
		return os.Getenv(name)
	}
	return cfg.Token
}

func failed(st Status, state State, err error, token string) Status {
	st.State = state
	message := err.Error()
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		message = fmt.Sprintf("kata: HTTP %d", apiErr.Status)
	}
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	st.Message = stringutil.TruncateRunes(message, maxErrorMessageRunes, "")
	return st
}

func stateFor(err error) State {
	switch {
	case errors.Is(err, ErrNotReady):
		return StateUnauthenticated
	case errors.Is(err, ErrIncompatible):
		return StateIncompatible
	case errors.Is(err, ErrWrongProject):
		return StateWrongProject
	}
	if status := StatusOf(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
		return StateUnauthenticated
	}
	return StateUnavailable
}

func compatibleAPISchema(reported string) bool {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return false
	}
	version := "v" + strings.TrimPrefix(reported, "v")
	return semver.IsValid(version) && semver.Compare(version, "v"+MinAPISchemaVersion) >= 0
}

func (c *Client) detect(ctx context.Context) (string, error) {
	var health kg.HealthResponseBody
	if err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, nil, nil, &health); err != nil {
		if StatusOf(err) == http.StatusNotFound {
			return "", fmt.Errorf("%w: /api/v1/health not found", ErrIncompatible)
		}
		return "", err
	}
	schema := ""
	if health.APISchemaVersion != nil {
		schema = strings.TrimSpace(*health.APISchemaVersion)
	}
	if !compatibleAPISchema(schema) {
		return "", fmt.Errorf("%w: api_schema_version is older than %s or missing", ErrIncompatible, MinAPISchemaVersion)
	}
	// Only expose the numeric core. Prerelease and build metadata come from an
	// untrusted server and can carry arbitrarily long private text.
	core := strings.TrimPrefix(schema, "v")
	core, _, _ = strings.Cut(core, "+")
	core, _, _ = strings.Cut(core, "-")
	if len(core) > 32 {
		return "", fmt.Errorf("%w: api_schema_version has an oversized numeric core", ErrIncompatible)
	}
	var instance kg.InstanceResponseBody
	if err := c.do(ctx, http.MethodGet, "/api/v1/instance", nil, nil, nil, &instance); err != nil {
		if StatusOf(err) == http.StatusNotFound {
			return core, fmt.Errorf("%w: /api/v1/instance not found", ErrIncompatible)
		}
		return core, err
	}
	if _, err := ulid.ParseStrict(instance.InstanceUID); err != nil || strings.TrimSpace(instance.Version) == "" {
		return core, fmt.Errorf("%w: instance_uid or version missing", ErrIncompatible)
	}
	c.mu.Lock()
	c.instanceUID = instance.InstanceUID
	c.mu.Unlock()
	return core, nil
}

func (c *Client) resolveProject(ctx context.Context) error {
	var response struct {
		Projects *[]kg.ProjectOut `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, nil, &response); err != nil {
		return err
	}
	if response.Projects == nil {
		return fmt.Errorf("%w: projects list missing", ErrIncompatible)
	}
	for _, project := range *response.Projects {
		if project.Name == c.project && project.Active && project.DeletedAt == nil {
			_, uidErr := ulid.ParseStrict(project.UID)
			if project.ID <= 0 || uidErr != nil {
				return fmt.Errorf("%w: active project has no usable identity", ErrIncompatible)
			}
			c.mu.Lock()
			c.projectID, c.projectUID = project.ID, project.UID
			c.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: no active project named %q; create it in Kata (agentsview never creates projects)", ErrWrongProject, c.project)
}

func (c *Client) InstanceUID() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.instanceUID }

func (c *Client) ProjectUID(ctx context.Context) (string, error) {
	c.mu.RLock()
	uid := c.projectUID
	c.mu.RUnlock()
	if uid != "" {
		return uid, nil
	}
	if err := c.resolveProject(ctx); err != nil {
		return "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.projectUID, nil
}

// withProject retries once when Kata reports a stale numeric project ID.
func (c *Client) withProject(ctx context.Context, fn func(int64) error) error {
	c.mu.RLock()
	projectID := c.projectID
	c.mu.RUnlock()
	if projectID == 0 {
		if err := c.resolveProject(ctx); err != nil {
			return err
		}
		c.mu.RLock()
		projectID = c.projectID
		c.mu.RUnlock()
	}
	err := fn(projectID)
	if !IsCode(err, "project_not_found") {
		return err
	}
	if resolveErr := c.resolveProject(ctx); resolveErr != nil {
		return resolveErr
	}
	c.mu.RLock()
	projectID = c.projectID
	c.mu.RUnlock()
	return fn(projectID)
}
