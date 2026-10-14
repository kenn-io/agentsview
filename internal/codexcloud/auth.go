// Package codexcloud imports threads from OpenAI's hosted Codex app-server
// into per-thread mirror files that the codex-cloud provider parses.
package codexcloud

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// AccountKey identifies one ChatGPT login: a workspace and a user. It
// names the per-login state and mirror directory and never contains token
// material.
type AccountKey string

// AccountKeyFor derives the key from the workspace account ID (which may
// be empty) and the user ID.
func AccountKeyFor(accountID, userID string) AccountKey {
	sum := sha256.Sum256([]byte(accountID + "\x00" + userID))
	return AccountKey(hex.EncodeToString(sum[:16]))
}

// HomeState is what a Codex home offers AgentsView.
type HomeState int

const (
	HomeOK HomeState = iota
	HomeSignedOut
	HomeKeyring
	HomeEphemeral
)

// Login is one Codex home's ChatGPT login, read at the start of a fetch or
// a live-capture connection and dropped when that ends.
type Login struct {
	Home      string
	Token     Token
	AccountID string
	UserID    string
	Key       AccountKey
	// Store is the home's cli_auth_credentials_store mode.
	Store string
	// AuthModTime is auth.json's modification time, used to end
	// signed-out and refresh-requested waits when the login changes.
	AuthModTime time.Time
}

// DiscoverHomes lists the Codex homes to sync: the daemon's CODEX_HOME,
// else ~/.codex, then each configured [agents.codex] home, without
// repeats.
func DiscoverHomes(getenv func(string) string, userHome string, configured []string) []string {
	first := getenv("CODEX_HOME")
	if strings.TrimSpace(first) == "" {
		first = filepath.Join(userHome, ".codex")
	}
	var homes []string
	seen := map[string]bool{}
	for _, home := range append([]string{first}, configured...) {
		home = strings.TrimSpace(home)
		if home == "~" {
			home = userHome
		} else if rest, ok := strings.CutPrefix(home, "~/"); ok {
			home = filepath.Join(userHome, rest)
		}
		if home == "" {
			continue
		}
		home = filepath.Clean(home)
		if seen[home] {
			continue
		}
		seen[home] = true
		homes = append(homes, home)
	}
	return homes
}

// LoginReadError is a Codex home whose login could not be read at all.
// Message is the status text; it never quotes file content, which can
// hold tokens or other secrets.
type LoginReadError struct {
	Message string
	err     error
}

func (e *LoginReadError) Error() string { return e.err.Error() }
func (e *LoginReadError) Unwrap() error { return e.err }

// credentialStores are the cli_auth_credentials_store values Codex
// accepts.
const credentialStores = `"file", "keyring", "auto", or "ephemeral"`

// ReadLogin reads a home's ChatGPT login. It reads auth.json only in the
// file and auto credential modes, because Codex ignores that file in the
// others and a leftover copy is stale. The ID token is decoded in memory
// only. An error means the home could not be read at all.
func ReadLogin(home string) (Login, HomeState, error) {
	store, err := credentialStore(home)
	if err != nil {
		return Login{}, HomeSignedOut, err
	}
	login := Login{Home: home, Store: store}
	switch store {
	case "keyring":
		return login, HomeKeyring, nil
	case "ephemeral":
		return login, HomeEphemeral, nil
	case "file", "auto":
	default:
		return login, HomeSignedOut, &LoginReadError{
			// The status names only the setting; the log has its value.
			Message: fmt.Sprintf("This Codex home's config.toml sets cli_auth_credentials_store to a value "+
				"AgentsView does not know. Use %s.", credentialStores),
			err: fmt.Errorf("codex home %s: unknown cli_auth_credentials_store %q; use %s",
				home, store, credentialStores),
		}
	}

	path := filepath.Join(home, "auth.json")
	info, statErr := os.Stat(path)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if store == "auto" {
			return login, HomeKeyring, nil
		}
		return login, HomeSignedOut, nil
	}
	if err != nil {
		return login, HomeSignedOut, &LoginReadError{
			Message: "AgentsView could not read this Codex home's auth.json. Check that you can read the file, " +
				"or run `codex login` to write it again. " + logName + " has the details.",
			err: fmt.Errorf("codex home %s: read auth.json: %w", home, err),
		}
	}
	if statErr == nil {
		login.AuthModTime = info.ModTime()
	}
	var auth struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			IDToken     string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		// The decoder's message can quote file content, which holds tokens.
		return login, HomeSignedOut, &LoginReadError{
			Message: "This Codex home's auth.json is not valid JSON. Run `codex login` to write it again.",
			err:     fmt.Errorf("codex home %s: auth.json is not valid JSON", home),
		}
	}
	if auth.Tokens == nil || strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		if store == "auto" {
			return login, HomeKeyring, nil
		}
		return login, HomeSignedOut, nil
	}
	userID := idTokenUserID(auth.Tokens.IDToken)
	if userID == "" {
		return login, HomeSignedOut, nil
	}
	login.Token = NewToken(auth.Tokens.AccessToken)
	login.AccountID = auth.Tokens.AccountID
	login.UserID = userID
	login.Key = AccountKeyFor(login.AccountID, userID)
	return login, HomeOK, nil
}

func credentialStore(home string) (string, error) {
	var cfg struct {
		Store string `toml:"cli_auth_credentials_store"`
	}
	path := filepath.Join(home, "config.toml")
	if _, err := toml.DecodeFile(path, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The decoder's message can quote the file, which may hold
		// secrets, so only the log gets it.
		return "", &LoginReadError{
			Message: "AgentsView could not read this Codex home's config.toml, so it cannot tell where Codex " +
				"keeps the login. The file may not be valid TOML. " + logName + " has the error and where it is.",
			err: fmt.Errorf("codex home %s: read config.toml: %w", home, err),
		}
	}
	if cfg.Store == "" {
		return "file", nil
	}
	return cfg.Store, nil
}

// idTokenUserID returns chatgpt_user_id, else user_id, from the ID token's
// auth claim. The signature is not checked: the value only keys local
// state for a token AgentsView already holds.
func idTokenUserID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]jsontext.Value
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	var auth struct {
		ChatGPTUserID string `json:"chatgpt_user_id"`
		UserID        string `json:"user_id"`
	}
	if err := json.Unmarshal(claims["https://api.openai.com/auth"], &auth); err != nil {
		return ""
	}
	if auth.ChatGPTUserID != "" {
		return auth.ChatGPTUserID
	}
	return auth.UserID
}

// GroupByKey groups logins by account key, keeping home order within a
// key and returning keys in order of first appearance.
func GroupByKey(logins []Login) (map[AccountKey][]Login, []AccountKey) {
	groups := map[AccountKey][]Login{}
	var order []AccountKey
	for _, login := range logins {
		if _, ok := groups[login.Key]; !ok {
			order = append(order, login.Key)
		}
		groups[login.Key] = append(groups[login.Key], login)
	}
	return groups, order
}
