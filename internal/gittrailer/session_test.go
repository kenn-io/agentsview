package gittrailer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const sessionUUID = "12345678-1234-1234-1234-1234567890ab"

func TestDiscover(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		configured map[string]string
		want       Session
		wantErr    bool
	}{
		{name: "no context"},
		{name: "minimal numeric agent and id regression", env: map[string]string{"MINIMAL": "0"}, configured: map[string]string{"0": "MINIMAL"}, want: Session{"0", "0"}},
		{name: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": sessionUUID}, want: Session{"claude", sessionUUID}},
		{name: "codex thread", env: map[string]string{"CODEX_THREAD_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "codex legacy", env: map[string]string{"CODEX_SESSION_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "aliases agree", env: map[string]string{"CODEX_THREAD_ID": sessionUUID, "CODEX_SESSION_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "agents disagree", env: map[string]string{"CLAUDE_CODE_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": sessionUUID}, wantErr: true},
		{name: "ids disagree", env: map[string]string{"CODEX_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": "abcdefab-1234-1234-1234-1234567890ab"}, wantErr: true},
		{name: "case is exact", env: map[string]string{"CODEX_SESSION_ID": sessionUUID, "CODEX_THREAD_ID": strings.ToUpper(sessionUUID)}, wantErr: true},
		{name: "override wins", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": "run_7.9-A", "CODEX_THREAD_ID": sessionUUID}, want: Session{"pi", "run_7.9-A"}},
		{name: "half pair", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "CODEX_THREAD_ID": sessionUUID}, wantErr: true},
		{name: "invalid pair no fallback", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "codex", "AGENTSVIEW_SESSION_ID": "bad", "CODEX_THREAD_ID": sessionUUID}, wantErr: true},
		{name: "empty pair discovers", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "", "AGENTSVIEW_SESSION_ID": "", "CODEX_THREAD_ID": sessionUUID}, want: Session{"codex", sessionUUID}},
		{name: "configured", env: map[string]string{"PI_SESSION_ID": "run.1"}, configured: map[string]string{"pi": "PI_SESSION_ID"}, want: Session{"pi", "run.1"}},
		{name: "configured built-in agrees", env: map[string]string{"CODEX_THREAD_ID": sessionUUID, "CUSTOM_ID": sessionUUID}, configured: map[string]string{"codex": "CUSTOM_ID"}, want: Session{"codex", sessionUUID}},
		{name: "invalid configured key even with override", env: map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": "run"}, configured: map[string]string{"Bad": "IGNORED"}, wantErr: true},
		{name: "short native id", env: map[string]string{"CODEX_THREAD_ID": "run.1"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Discover(func(k string) string { return tt.env[k] }, tt.configured)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSessionValidationBoundaries(t *testing.T) {
	tests := []struct {
		agent, id string
		valid     bool
	}{
		{"a", "A", true},
		{strings.Repeat("a", 32), strings.Repeat("x", 128), true},
		{strings.Repeat("a", 33), "x", false},
		{"a", strings.Repeat("x", 129), false},
		{"a", "", false},
		{"-a", "x", false},
		{"A", "x", false},
		{"a_b", "x", false},
		{"a", ".x", false},
		{"a", "a/b", false},
		{"a", "a\nb", false},
		{"a", "é", false},
		{"claude", sessionUUID, true},
		{"codex", strings.ToUpper(sessionUUID), true},
		{"claude", "123456781234123412341234567890ab", false},
		{"codex", "g2345678-1234-1234-1234-1234567890ab", false},
	}
	for _, tt := range tests {
		t.Run(tt.agent+"/"+tt.id, func(t *testing.T) {
			env := map[string]string{"AGENTSVIEW_SESSION_AGENT": tt.agent, "AGENTSVIEW_SESSION_ID": tt.id}
			got, err := Discover(func(k string) string { return env[k] }, nil)
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, Session{tt.agent, tt.id}, got)
			} else {
				require.Error(t, err)
				assert.Empty(t, got)
			}
		})
	}
}

func TestDiscoverCustomIDsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		agent := hegel.Draw(ht, hegel.FromRegex(`^[a-z0-9][a-z0-9-]{0,31}$`, true))
		// Native agents have a separate UUID contract tested below.
		ht.Assume(agent != "claude" && agent != "codex")
		id := hegel.Draw(ht, hegel.FromRegex(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, true))
		got, err := Discover(func(k string) string {
			if k == "CURRENT" {
				return id
			}
			return ""
		}, map[string]string{agent: "CURRENT"})
		require.NoError(ht, err)
		assert.Equal(ht, Session{agent, id}, got)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestDiscoverOverrideProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, true))
		inherited := hegel.Draw(ht, hegel.Text())
		env := map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": id, "CLAUDE_CODE_SESSION_ID": inherited, "CODEX_THREAD_ID": inherited, "CUSTOM": inherited}
		got, err := Discover(func(k string) string { return env[k] }, map[string]string{"other": "CUSTOM"})
		require.NoError(ht, err)
		assert.Equal(ht, Session{"pi", id}, got)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestDiscoverNativeAgreementProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.UUIDs()).String()
		other := hegel.Draw(ht, hegel.UUIDs()).String()
		env := map[string]string{"CODEX_THREAD_ID": id, "CODEX_SESSION_ID": other}
		got, err := Discover(func(k string) string { return env[k] }, nil)
		if id == other {
			require.NoError(ht, err)
			assert.Equal(ht, Session{"codex", id}, got)
		} else {
			require.Error(ht, err)
			assert.Empty(ht, got)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestDiscoverRejectsHostileIDsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		// An independent byte-level oracle covers arbitrary Unicode/control input.
		valid := len(id) > 0 && len(id) <= 128
		for i, c := range []byte(id) {
			alphaNum := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
			valid = valid && (alphaNum || i > 0 && (c == '.' || c == '_' || c == '-'))
		}
		got, err := Discover(func(k string) string {
			switch k {
			case "AGENTSVIEW_SESSION_AGENT":
				return "pi"
			case "AGENTSVIEW_SESSION_ID":
				return id
			}
			return ""
		}, nil)
		if valid {
			require.NoError(ht, err)
			assert.Equal(ht, Session{"pi", id}, got)
		} else {
			require.Error(ht, err)
			assert.Empty(ht, got)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestDiscoverNativeSourcesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		agent := hegel.Draw(ht, hegel.SampledFrom([]string{"claude", "codex"}))
		id := hegel.Draw(ht, hegel.UUIDs()).String()
		if hegel.Draw(ht, hegel.Booleans()) {
			id = strings.ToUpper(id)
		}
		env := map[string]string{"NATIVE_CUSTOM": id}
		if agent == "claude" {
			env["CLAUDE_CODE_SESSION_ID"] = id
		} else {
			env["CODEX_THREAD_ID"] = id
			env["CODEX_SESSION_ID"] = id
		}
		got, err := Discover(func(k string) string { return env[k] }, map[string]string{agent: "NATIVE_CUSTOM"})
		require.NoError(ht, err)
		assert.Equal(ht, Session{agent, id}, got)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestDiscoverConfiguredKeyValidationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		key := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.FromRegex(`^[a-z0-9][a-z0-9-]{0,31}$`, true)))
		valid := len(key) > 0 && len(key) <= 32
		for i, c := range []byte(key) {
			valid = valid && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && c == '-')
		}
		env := map[string]string{"AGENTSVIEW_SESSION_AGENT": "pi", "AGENTSVIEW_SESSION_ID": "exact_ID"}
		got, err := Discover(func(k string) string { return env[k] }, map[string]string{key: "EMPTY"})
		if valid {
			require.NoError(ht, err)
			assert.Equal(ht, Session{"pi", "exact_ID"}, got)
		} else {
			require.Error(ht, err)
			assert.Empty(ht, got)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}
