package codexcloud

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeAccessToken = "test-access-token-canary-3f1c"

// fakeIDToken builds an unsigned JWT with the auth claim the ID token
// carries. Only its payload is read.
func fakeIDToken(t *testing.T, claim map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"https://api.openai.com/auth": claim})
	require.NoError(t, err)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

type homeSpec struct {
	store     string // empty: no config.toml
	authJSON  bool
	accountID string
	claim     map[string]any
	access    string
}

func writeHome(t *testing.T, spec homeSpec) string {
	t.Helper()
	home := t.TempDir()
	if spec.store != "" {
		cfg := fmt.Sprintf("cli_auth_credentials_store = %q\n", spec.store)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600))
	}
	if spec.authJSON {
		access := spec.access
		if access == "" {
			access = fakeAccessToken
		}
		tokens := map[string]any{
			"access_token":  access,
			"refresh_token": "test-refresh-token",
			"id_token":      fakeIDToken(t, spec.claim),
		}
		if spec.accountID != "" {
			tokens["account_id"] = spec.accountID
		}
		data, err := json.Marshal(map[string]any{"tokens": tokens})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600))
	}
	return home
}

func TestReadLoginModes(t *testing.T) {
	user := map[string]any{"chatgpt_user_id": "user-a"}
	for _, tt := range []struct {
		name string
		spec homeSpec
		want HomeState
	}{
		{name: "default file store", spec: homeSpec{authJSON: true, claim: user}, want: HomeOK},
		{name: "explicit file store", spec: homeSpec{store: "file", authJSON: true, claim: user}, want: HomeOK},
		{name: "auto with token", spec: homeSpec{store: "auto", authJSON: true, claim: user}, want: HomeOK},
		{name: "auto without token", spec: homeSpec{store: "auto"}, want: HomeKeyring},
		{name: "keyring ignores leftover auth.json", spec: homeSpec{store: "keyring", authJSON: true, claim: user}, want: HomeKeyring},
		{name: "ephemeral ignores leftover auth.json", spec: homeSpec{store: "ephemeral", authJSON: true, claim: user}, want: HomeEphemeral},
		{name: "file store without auth.json", spec: homeSpec{store: "file"}, want: HomeSignedOut},
		{name: "empty access token", spec: homeSpec{authJSON: true, claim: user, access: " "}, want: HomeSignedOut},
		{name: "no user in ID token", spec: homeSpec{authJSON: true, claim: map[string]any{}}, want: HomeSignedOut},
	} {
		t.Run(tt.name, func(t *testing.T) {
			login, state, err := ReadLogin(writeHome(t, tt.spec))
			require.NoError(t, err)
			assert.Equal(t, tt.want, state)
			if tt.want == HomeOK {
				assert.Equal(t, fakeAccessToken, login.Token.Reveal())
				wantStore := tt.spec.store
				if wantStore == "" {
					wantStore = "file"
				}
				assert.Equal(t, wantStore, login.Store)
			} else {
				assert.Empty(t, login.Token.Reveal())
			}
		})
	}
}

func TestReadLoginUserIDFallback(t *testing.T) {
	login, state, err := ReadLogin(writeHome(t, homeSpec{
		authJSON: true, accountID: "acct-1", claim: map[string]any{"user_id": "user-legacy"},
	}))
	require.NoError(t, err)
	require.Equal(t, HomeOK, state)
	assert.Equal(t, "user-legacy", login.UserID)
	assert.Equal(t, "acct-1", login.AccountID)
	assert.Equal(t, AccountKeyFor("acct-1", "user-legacy"), login.Key)
}

func TestAccountKeyIgnoresAccountUserClaim(t *testing.T) {
	newer, _, err := ReadLogin(writeHome(t, homeSpec{
		authJSON: true, accountID: "acct-1",
		claim: map[string]any{"chatgpt_user_id": "user-a", "chatgpt_account_user_id": "user-a__acct-1"},
	}))
	require.NoError(t, err)
	older, _, err := ReadLogin(writeHome(t, homeSpec{
		authJSON: true, accountID: "acct-1", claim: map[string]any{"chatgpt_user_id": "user-a"},
	}))
	require.NoError(t, err)
	assert.Equal(t, older.Key, newer.Key)
}

func TestTwoUsersOneWorkspaceSeparate(t *testing.T) {
	assert.NotEqual(t, AccountKeyFor("acct-1", "user-a"), AccountKeyFor("acct-1", "user-b"))
	assert.NotEqual(t, AccountKeyFor("acct-1", "user-a"), AccountKeyFor("acct-2", "user-a"))
	assert.NotEqual(t, AccountKeyFor("ab", "c"), AccountKeyFor("a", "bc"))
	assert.Regexp(t, `^[0-9a-f]{32}$`, string(AccountKeyFor("acct-1", "user-a")))
}

func TestTokenNeverFormats(t *testing.T) {
	login, state, err := ReadLogin(writeHome(t, homeSpec{
		authJSON: true, accountID: "acct-1", claim: map[string]any{"chatgpt_user_id": "user-a"},
	}))
	require.NoError(t, err)
	require.Equal(t, HomeOK, state)
	texts := []string{fmt.Sprintf("%v %+v %#v %s %v", login.Token, login, login, login.Token, &login)}
	// Verbs that skip String print a struct's fields unless the type
	// formats itself.
	for _, verb := range []string{"%d", "%t", "%x", "%q", "%+d", "%8.3f"} {
		texts = append(texts, fmt.Sprintf(verb, login.Token), fmt.Sprintf(verb, login))
	}
	formatted := strings.Join(texts, " ")
	data, err := json.Marshal(login)
	require.NoError(t, err)
	for _, text := range []string{formatted, string(data)} {
		assert.NotContains(t, text, fakeAccessToken)
		assert.NotContains(t, text, "test-refresh-token")
	}
}

func TestRedactEscapedForms(t *testing.T) {
	tok := NewToken("tok/en+with=chars")
	err := errors.New("dial failed: " + strings.Join([]string{
		"tok/en+with=chars",
		url.QueryEscape("tok/en+with=chars"),
		url.PathEscape("tok/en+with=chars"),
	}, " "))
	redacted := Redact(err, tok)
	assert.NotContains(t, redacted.Error(), "tok/en")
	assert.NotContains(t, redacted.Error(), "tok%2Fen")
	assert.Equal(t, "dial failed: [redacted] [redacted] [redacted]", redacted.Error())
	assert.NoError(t, Redact(nil, tok))
}

func TestDiscoverHomes(t *testing.T) {
	userHome := t.TempDir()
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }
	assert.Equal(t, []string{filepath.Join(userHome, ".codex")}, DiscoverHomes(getenv, userHome, nil))

	env["CODEX_HOME"] = filepath.Join(userHome, "custom")
	configured := []string{"~/work-codex", filepath.Join(userHome, "custom"), filepath.Join(userHome, "work-codex") + "/"}
	assert.Equal(t,
		[]string{filepath.Join(userHome, "custom"), filepath.Join(userHome, "work-codex")},
		DiscoverHomes(getenv, userHome, configured))
}

func TestGroupByKeyKeepsHomeOrder(t *testing.T) {
	a := Login{Home: "/h1", Key: "k1"}
	b := Login{Home: "/h2", Key: "k2"}
	c := Login{Home: "/h3", Key: "k1"}
	groups, order := GroupByKey([]Login{a, b, c})
	assert.Equal(t, []AccountKey{"k1", "k2"}, order)
	assert.Equal(t, []Login{a, c}, groups["k1"])
}

func TestReadLoginErrorsExplainTheProblem(t *testing.T) {
	const secret = "secret-value-canary-91aa"
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, home string)
		want  string
	}{
		{
			name: "config.toml does not parse",
			setup: func(t *testing.T, home string) {
				t.Helper()
				cfg := "api_key = \"" + secret + "\"\nbroken = " + secret + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600))
			},
			want: "could not read this Codex home's config.toml",
		},
		{
			name: "unknown credential store",
			setup: func(t *testing.T, home string) {
				t.Helper()
				cfg := "cli_auth_credentials_store = \"vault\"\n"
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600))
			},
			want: `cli_auth_credentials_store to a value AgentsView does not know. ` +
				`Use "file", "keyring", "auto", or "ephemeral".`,
		},
		{
			name: "auth.json is not JSON",
			setup: func(t *testing.T, home string) {
				t.Helper()
				data := `{"tokens":{"access_token":"` + secret
				require.NoError(t, os.WriteFile(filepath.Join(home, "auth.json"), []byte(data), 0o600))
			},
			want: "auth.json is not valid JSON. Run `codex login`",
		},
		{
			name: "auth.json cannot be read",
			setup: func(t *testing.T, home string) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(home, "auth.json"), 0o700))
			},
			want: "could not read this Codex home's auth.json",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.setup(t, home)
			_, _, err := ReadLogin(home)
			readErr, ok := errors.AsType[*LoginReadError](err)
			require.True(t, ok, "got %v", err)
			assert.Contains(t, readErr.Message, tt.want)
			assert.NotContains(t, readErr.Message, secret)
			assert.NotContains(t, readErr.Message, "vault", "the status does not quote config.toml values")
			assert.NotContains(t, readErr.Message, home, "the row already names the home")
		})
	}
}
