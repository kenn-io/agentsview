package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/agentsview/internal/pathutil"
)

// Container deployment modes selected by AGENTSVIEW_MODE.
const (
	DeploymentModeServe   = "serve"
	DeploymentModePGServe = "pg-serve"
)

// deploymentEmbeddingsServer names the embeddings server built from
// AGENTSVIEW_EMBEDDINGS_ENDPOINT.
const deploymentEmbeddingsServer = "deployment"

// DeploymentMode reports the container deployment mode. set is false when
// AGENTSVIEW_MODE is unset or blank; then no deployment variable applies.
// PG_SERVE (1/true/yes/on, any case) selects pg-serve only when set is true.
func DeploymentMode() (mode string, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv("AGENTSVIEW_MODE"))
	if raw == "" {
		return "", false, nil
	}
	switch mode := strings.ToLower(raw); mode {
	case DeploymentModeServe:
		if pgServeSelected() {
			return DeploymentModePGServe, true, nil
		}
		return mode, true, nil
	case DeploymentModePGServe:
		return mode, true, nil
	default:
		return "", true, fmt.Errorf(
			"AGENTSVIEW_MODE must be %q or %q, got %q",
			DeploymentModeServe, DeploymentModePGServe, raw)
	}
}

// pgServeSelected keeps the image's legacy PG_SERVE selector.
func pgServeSelected() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PG_SERVE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// applyDeploymentEnv runs after the file layer and before explicit flags.
// It is the only reader of the container deployment variables and applies
// nothing unless AGENTSVIEW_MODE is set. It never sets HostExplicit, so a
// persistent non-loopback host from the environment still needs auth.
func (c *Config) applyDeploymentEnv() error {
	if _, set, err := DeploymentMode(); err != nil || !set {
		return err
	}
	if v, ok := os.LookupEnv("AGENTSVIEW_HOST"); ok {
		host := strings.TrimSpace(v)
		if host == "" {
			return errors.New("AGENTSVIEW_HOST must not be blank")
		}
		c.Host = host
	}
	if v, set, err := lookupEnvBool("AGENTSVIEW_REQUIRE_AUTH"); err != nil {
		return err
	} else if set {
		c.RequireAuth = v
	}
	if v, set, err := lookupEnvBool("AGENTSVIEW_NO_BROWSER"); err != nil {
		return err
	} else if set {
		c.NoBrowser = v
	}
	if v, set, err := lookupEnvBool("AGENTSVIEW_PG_ALLOW_INSECURE"); err != nil {
		return err
	} else if set {
		c.pgEnvOverrides.AllowInsecure = &v
	}
	if path := os.Getenv("AGENTSVIEW_AUTH_TOKEN_FILE"); path != "" {
		if os.Getenv("AGENTSVIEW_AUTH_TOKEN") != "" {
			return errors.New(
				"set only one of AGENTSVIEW_AUTH_TOKEN and AGENTSVIEW_AUTH_TOKEN_FILE")
		}
		token, err := readSecretFile("AGENTSVIEW_AUTH_TOKEN_FILE", path)
		if err != nil {
			return err
		}
		c.AuthToken = token
	}
	return c.applyDeploymentEmbeddings()
}

// applyDeploymentEmbeddings builds the embeddings server named by
// AGENTSVIEW_EMBEDDINGS_ENDPOINT. A [vector] section owns the recipe and
// its servers, so the two never combine.
func (c *Config) applyDeploymentEmbeddings() error {
	endpoint, endpointSet := os.LookupEnv("AGENTSVIEW_EMBEDDINGS_ENDPOINT")
	keyPath, keySet := os.LookupEnv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE")
	batchSize, batchSet := os.LookupEnv("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE")
	if !endpointSet && !keySet && !batchSet {
		return nil
	}
	if c.vectorSectionDefined {
		return errors.New(
			"AGENTSVIEW_EMBEDDINGS_* cannot be combined with a [vector] section " +
				"in config.toml; configure the embeddings server in one place")
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return errors.New("AGENTSVIEW_EMBEDDINGS_ENDPOINT is required " +
			"when an AGENTSVIEW_EMBEDDINGS_* variable is set")
	}
	servers := normalizedEmbeddingsServers(map[string]VectorEmbeddingsServerConfig{
		deploymentEmbeddingsServer: {Endpoint: endpoint},
	}, toml.MetaData{})
	server := servers[deploymentEmbeddingsServer]
	if batchSet {
		var err error
		server.BatchSize, err = strconv.Atoi(strings.TrimSpace(batchSize))
		if err != nil {
			return errors.New("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE must be a positive integer")
		}
	}
	if keySet {
		key, err := readSecretFile("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", keyPath)
		if err != nil {
			return err
		}
		server.apiKey = key
	}
	if err := server.validate(deploymentEmbeddingsServer, 0); err != nil {
		if batchSet {
			return fmt.Errorf("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE: %w", err)
		}
		return err
	}
	c.DeploymentEmbeddings = &server
	return nil
}

// readSecretFile reads a secret selected by variable. A missing,
// unreadable or blank file is an error; errors name the variable and path,
// never the value.
func readSecretFile(variable, path string) (string, error) {
	expanded, err := pathutil.ExpandHome(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("%s: expanding path: %w", variable, err)
	}
	if expanded == "" {
		return "", fmt.Errorf("%s must name a file", variable)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("%s: reading secret file: %w", variable, err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("%s: secret file %s is empty", variable, expanded)
	}
	return secret, nil
}

// lookupEnvBool parses a boolean deployment variable; unset leaves the
// lower layer in place.
func lookupEnvBool(name string) (value, set bool, err error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false, nil
	}
	value, err = strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, true, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return value, true, nil
}
