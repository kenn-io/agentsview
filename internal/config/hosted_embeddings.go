package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"
)

type HostedEmbeddingsConfig struct {
	Profiles map[string]HostedEmbeddingProfile `toml:"profiles" json:"profiles"`
}

type HostedEmbeddingProfile struct {
	IncludeAutomated bool                   `toml:"include_automated" json:"include_automated"`
	Embeddings       VectorEmbeddingsConfig `toml:"embeddings" json:"embeddings"`
}

func normalizeHostedEmbeddingProfiles(
	profiles map[string]HostedEmbeddingProfile, meta toml.MetaData,
) (map[string]HostedEmbeddingProfile, error) {
	out := make(map[string]HostedEmbeddingProfile, len(profiles))
	seen := make(map[string]string, len(profiles))
	for rawName, profile := range profiles {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if name == "" {
			return nil, fmt.Errorf("hosted embedding profile names must not be blank")
		}
		if prior, ok := seen[name]; ok {
			return nil, fmt.Errorf("hosted embedding profiles %q and %q normalize to the same name %q", prior, rawName, name)
		}
		seen[name] = rawName
		if profile.Embeddings.MaxInputChars == 0 {
			profile.Embeddings.MaxInputChars = 8192
		}
		profile.Embeddings.Servers = normalizedEmbeddingsServersAtPath(
			profile.Embeddings.Servers, meta,
			"hosted_embeddings", "profiles", rawName, "embeddings", "servers",
		)
		out[name] = profile
	}
	return out, nil
}

// Profile returns and validates one complete hosted embedding profile. Other
// configured profiles and their credential references are deliberately not
// inspected so an unavailable migration profile cannot disable active reads.
func (c HostedEmbeddingsConfig) Profile(name string) (HostedEmbeddingProfile, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	profile, ok := c.Profiles[name]
	if !ok || name == "" {
		return HostedEmbeddingProfile{}, fmt.Errorf("[hosted_embeddings.profiles] no profile named %q", name)
	}
	section := fmt.Sprintf("[hosted_embeddings.profiles.%s.embeddings]", name)
	if profile.Embeddings.Model == "" {
		return HostedEmbeddingProfile{}, fmt.Errorf("%s model is required", section)
	}
	if profile.Embeddings.Dimension <= 0 {
		return HostedEmbeddingProfile{}, fmt.Errorf("%s dimension must be greater than 0", section)
	}
	if profile.Embeddings.MaxInputChars <= 0 {
		return HostedEmbeddingProfile{}, fmt.Errorf("%s max_input_chars must be greater than 0", section)
	}
	if profile.Embeddings.ModelContextTokens < 0 {
		return HostedEmbeddingProfile{}, fmt.Errorf("%s model_context_tokens must not be negative", section)
	}
	if err := validateHostedEmbeddingServers(profile.Embeddings, section); err != nil {
		return HostedEmbeddingProfile{}, err
	}
	for _, serverName := range sortedServerNames(profile.Embeddings.Servers) {
		server := profile.Embeddings.Servers[serverName]
		u, err := url.Parse(server.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return HostedEmbeddingProfile{}, fmt.Errorf("%s.servers.%s endpoint must be an absolute HTTP(S) endpoint", section, serverName)
		}
	}
	return profile, nil
}

func validateHostedEmbeddingServers(c VectorEmbeddingsConfig, section string) error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("%s at least one server is required; define one under %s.servers.<name>", section, section)
	}
	if c.DefaultServer == "" && len(c.Servers) > 1 {
		return fmt.Errorf("%s default_server is required when more than one server is defined (have: %s)", section, strings.Join(sortedServerNames(c.Servers), ", "))
	}
	if c.DefaultServer != "" {
		if _, ok := c.Servers[c.DefaultServer]; !ok {
			return fmt.Errorf("%s default_server %q is not a defined server (have: %s)", section, c.DefaultServer, strings.Join(sortedServerNames(c.Servers), ", "))
		}
	}
	for _, name := range sortedServerNames(c.Servers) {
		if err := c.Servers[name].validateFor(section, name, c.ModelContextTokens); err != nil {
			return err
		}
	}
	return nil
}

func (p PGConfig) ValidateHostedEmbeddings(requireAuth bool) error {
	if !p.HostedEmbeddingsEnabled {
		return nil
	}
	if strings.TrimSpace(p.RawTenant) == "" || p.RawTenant != strings.TrimSpace(p.RawTenant) || len(p.RawTenant) > 128 {
		return fmt.Errorf("hosted embeddings require raw_tenant")
	}
	if !requireAuth {
		return fmt.Errorf("hosted embeddings require authentication")
	}
	if p.Schema == "" {
		return fmt.Errorf("hosted embeddings require a configured schema")
	}
	for _, r := range p.Schema {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("hosted embeddings schema is invalid")
		}
	}
	if p.HostedEmbeddingsPollSeconds < 0 || p.HostedEmbeddingsPollSeconds > 60 ||
		p.HostedEmbeddingsAttemptSeconds < 0 || p.HostedEmbeddingsAttemptSeconds > 300 ||
		p.HostedEmbeddingsMaxAttempts < 0 || p.HostedEmbeddingsMaxAttempts > 10 ||
		p.HostedEmbeddingsConcurrency < 0 || p.HostedEmbeddingsConcurrency > 4 {
		return fmt.Errorf("hosted embedding worker bounds: poll 1-60 seconds, attempt 1-300 seconds, attempts 1-10, concurrency 1-4; zero selects defaults")
	}
	return nil
}

func (p PGConfig) HostedEmbeddingWorkerBounds() (poll, attempt, attempts, concurrency int) {
	poll, attempt = p.HostedEmbeddingsPollSeconds, p.HostedEmbeddingsAttemptSeconds
	attempts, concurrency = p.HostedEmbeddingsMaxAttempts, p.HostedEmbeddingsConcurrency
	if poll == 0 {
		poll = 5
	}
	if attempt == 0 {
		attempt = 120
	}
	if attempts == 0 {
		attempts = 5
	}
	if concurrency == 0 {
		concurrency = 1
	}
	return
}
