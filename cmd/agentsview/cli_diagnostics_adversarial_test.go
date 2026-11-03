package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func TestCLIRecoveryMetadataComposition(t *testing.T) {
	constructors := []struct {
		make func(error, string) error
		code int
		kind string
	}{
		{cliUsageError, 64, "usage"}, {cliValidationError, 65, "validation"}, {cliUnavailableError, 69, "unavailable"},
	}
	hegel.Test(t, func(ht *hegel.T) {
		variant := constructors[hegel.Draw(ht, hegel.Integers(0, len(constructors)-1))]
		rawDepth := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		depth := 1 + min(rawDepth, 32) // Bound materialization, not the drawn domain.
		hint := hegel.Draw(ht, hegel.Text())
		cause := errors.New("failure")
		err := variant.make(fmt.Errorf("operation: %w", cause), "initial advice")
		for range depth {
			err = withCLIHint(err, hint)
		}
		explicit := hegel.Draw(ht, hegel.Integers(0, 255))
		expected := variant.code
		if explicit != 0 {
			err = withExitCode(err, explicit)
			expected = explicit
		}
		err = contextualizeCLIError(&cobra.Command{Use: "fixture"}, err, true)
		assert.Equal(ht, expected, exitCodeFromError(err))
		assert.Equal(ht, "operation: failure", err.Error())
		require.ErrorIs(ht, err, cause)
		var stderr bytes.Buffer
		reportCLIError(err, &stderr)
		var got struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		}
		require.NoError(ht, json.Unmarshal(stderr.Bytes(), &got))
		assert.Equal(ht, variant.kind, got.Error)
		assert.Equal(ht, "operation: failure", got.Message)
		expectedHint := hint
		if expectedHint == "" {
			expectedHint = "run `fixture --help` for flags and examples"
		}
		assert.Equal(ht, expectedHint, got.Hint)
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoverySelectorsFollowFlagGrammar(t *testing.T) {
	trueValues := []string{"1", "t", "T", "TRUE", "true", "True"}
	falseValues := []string{"0", "f", "F", "FALSE", "false", "False"}
	cmd, _, err := newRootCommand().Find([]string{"session", "list"})
	require.NoError(t, err)
	assert.True(t, requestedErrorJSON(cmd, []string{"--json"}))
	hegel.Test(t, func(ht *hegel.T) {
		selected := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		values := falseValues
		if selected {
			values = trueValues
		}
		value := values[hegel.Draw(ht, hegel.Integers(0, len(values)-1))]
		formatJSON := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		format := "human"
		if formatJSON {
			format = "json"
		}
		args := []string{"--bogus", "--json=true", "--json=" + value}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 0 {
			args = append(args, "--format", format)
		} else {
			args = append(args, "--format="+format)
		}
		args = append(args, "--", "--json=true", "--format=json")
		assert.Equal(ht, selected || formatJSON, requestedErrorJSON(cmd, args))
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoverySelectorsIgnoreStringFlagValues(t *testing.T) {
	cmd, _, err := newRootCommand().Find([]string{"session", "list"})
	require.NoError(t, err)
	flags := []string{"--server", "--project", "--cursor"}
	values := []string{"--json", "--format=json"}
	hegel.Test(t, func(ht *hegel.T) {
		flag := flags[hegel.Draw(ht, hegel.Integers(0, len(flags)-1))]
		value := values[hegel.Draw(ht, hegel.Integers(0, len(values)-1))]
		arg := []string{flag, value, "--bogus"}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			arg = []string{flag + "=" + value, "--bogus"}
		}
		assert.False(ht, requestedErrorJSON(cmd, arg))
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoveryNamespacedSyncAdvice(t *testing.T) {
	cmd, _, err := newRootCommand().Find([]string{"session", "sync"})
	require.NoError(t, err)
	assert.NotContains(t, commandRecoveryHint(cmd), "--host")
	hegel.Test(t, func(ht *hegel.T) {
		name := "namespace-" + hex.EncodeToString([]byte(hegel.Draw(ht, hegel.Text())))
		root := &cobra.Command{Use: "agentsview"}
		parent := &cobra.Command{Use: name}
		child := &cobra.Command{Use: "sync"}
		root.AddCommand(parent)
		parent.AddCommand(child)
		assert.NotContains(ht, commandRecoveryHint(child), "--host")
		assert.Contains(ht, commandRecoveryHint(child), child.CommandPath()+" --help")
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoveryRemoteSentinelsHideURLSecrets(t *testing.T) {
	legacyRecoveryEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"unsupported"}`))
	}))
	defer ts.Close()
	hegel.Test(t, func(ht *hegel.T) {
		raw := []byte(hegel.Draw(ht, hegel.Text()))
		raw = raw[:min(len(raw), 256)] // Bound the local HTTP fixture request size.
		secret := hex.EncodeToString(raw)
		target, parseErr := url.Parse(ts.URL)
		require.NoError(ht, parseErr)
		target.User = url.UserPassword("private-user-"+secret, "private-pass-"+secret)
		target.Path = "/private-path-" + secret
		query := url.Values{"token": {"private-token-" + secret}}
		target.RawQuery = query.Encode()
		var stdout, stderr bytes.Buffer
		args := []string{"session", "sync", "codex:fixture", "--server", target.String()}
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			args = append(args, "--json")
		}
		err := executeCLIWithLegacyFlagCompat(args, &stdout, &stderr)
		require.Error(ht, err)
		require.ErrorIs(ht, err, db.ErrReadOnly)
		assert.Equal(ht, 1, reportCLIError(err, &stderr))
		assert.Empty(ht, stdout.String())
		assert.Contains(ht, stderr.String(), "read-only")
		for _, part := range []string{"private-user-", "private-pass-", "private-path-", "private-token-", "/api/v1"} {
			assert.NotContains(ht, stderr.String(), part)
		}
		assert.NotContains(ht, stderr.String(), "--host")
		assert.NotContains(ht, stderr.String(), "daemon restart")
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoveryHiddenLineagesKeepProtocol(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		root := &cobra.Command{Use: "agentsview"}
		current := root
		depth := 1 + min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 8)
		hiddenAt := hegel.Draw(ht, hegel.Integers(0, depth-1))
		for i := range depth {
			child := &cobra.Command{Use: fmt.Sprintf("child-%d", i), Hidden: i == hiddenAt}
			current.AddCommand(child)
			current = child
		}
		err := contextualizeCLIError(current, errors.New(message), true)
		var stderr bytes.Buffer
		assert.Equal(ht, 1, reportCLIError(err, &stderr))
		assert.Equal(ht, "fatal: "+message+"\n", stderr.String())
	}, hegel.WithTestCases(1000))
}
