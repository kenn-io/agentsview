package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type diagnosticFixtureError struct{ message string }

func (e *diagnosticFixtureError) Error() string { return e.message }

// Diagnostic JSON must preserve arbitrary error text and hints, including
// control characters and quotes, without changing the wrapped cause.
func TestCLIRecoveryJSONPreservesDiagnosticText(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		hint := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Integers(0, 1)) == 1 {
			rawMessage := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 255)))
			rawMessage = rawMessage[:min(len(rawMessage), 4096)]
			messageBytes := make([]byte, len(rawMessage))
			for i, n := range rawMessage {
				messageBytes[i] = byte(n)
			}
			message = string(messageBytes)

			rawHint := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 255)))
			rawHint = rawHint[:min(len(rawHint), 4096)]
			hintBytes := make([]byte, len(rawHint))
			for i, n := range rawHint {
				hintBytes[i] = byte(n)
			}
			hint = string(hintBytes)
		}
		cause := &diagnosticFixtureError{message: message}
		err := contextualizeCLIError(&cobra.Command{Use: "fixture"}, withCLIHint(fmt.Errorf("operation: %w", cause), hint), true)
		var out bytes.Buffer
		reportCLIError(err, &out)
		var got struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		}
		require.NoError(ht, json.Unmarshal(out.Bytes(), &got))
		assert.Equal(ht, "error", got.Error)
		assert.Equal(ht, "operation: "+strings.ToValidUTF8(message, "\uFFFD"), got.Message)
		expectedHint := strings.ToValidUTF8(hint, "\uFFFD")
		if expectedHint == "" {
			expectedHint = "run `fixture --help` for flags and examples"
		}
		assert.Equal(ht, expectedHint, got.Hint)
		assert.Equal(ht, 1, strings.Count(out.String(), "\n"))
		assert.Equal(ht, "operation: "+message, err.Error())
		require.ErrorIs(ht, err, cause)
		var typedCause *diagnosticFixtureError
		require.ErrorAs(ht, err, &typedCause)
		assert.Same(ht, cause, typedCause)
	}, hegel.WithTestCases(1000))
}

func TestCLIRecoveryExplicitExitCodesSurvive(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.Integers(1, 255))
		err := contextualizeCLIError(&cobra.Command{Use: "fixture"}, withSilentExitCode(errors.New("result already written"), code), true)
		var out bytes.Buffer
		assert.Equal(ht, code, reportCLIError(err, &out))
		assert.Empty(ht, out.String())
	}, hegel.WithTestCases(1000))
}
