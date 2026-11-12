package shellquote

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Parse the generated command without invoking it or interpreting its argument.
func TestPowerShellLiteralParser(t *testing.T) {
	const parser = `
[Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$text = [Console]::In.ReadToEnd()
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$tokens, [ref]$errors)
if ($errors.Count -ne 0 -or $ast.EndBlock.Statements.Count -ne 1) { exit 1 }
$pipeline = $ast.EndBlock.Statements[0]
if ($pipeline -isnot [System.Management.Automation.Language.PipelineAst] -or $pipeline.PipelineElements.Count -ne 1) { exit 2 }
$command = $pipeline.PipelineElements[0]
if ($command -isnot [System.Management.Automation.Language.CommandAst] -or $command.CommandElements.Count -ne 2) { exit 3 }
if ($command.CommandElements[0].Value -cne 'pi' -or $command.CommandElements[1] -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) { exit 4 }
ConvertTo-Json -Compress -InputObject ($command.CommandElements[1].Value)
`
	for _, executable := range []string{"pwsh", "powershell.exe"} {
		t.Run(executable, func(t *testing.T) {
			binary, err := exec.LookPath(executable)
			if err != nil {
				t.Skipf("%s is unavailable: %v", executable, err)
			}
			for _, value := range []string{
				`C:\sessions\plain.jsonl`,
				"C:\\sessions\\' ; Write-Output injected #.jsonl",
				"C:\\sessions\\'‘’‚‛''; $(Write-Output injected)\n$env:HOME & |.jsonl",
			} {
				t.Run(value, func(t *testing.T) {
					cmd := exec.CommandContext(t.Context(), binary, "-NoProfile", "-NonInteractive", "-Command", parser)
					cmd.Stdin = strings.NewReader("pi " + PowerShell(value))
					output, err := cmd.Output()
					var exit *exec.ExitError
					if errors.As(err, &exit) {
						require.NoError(t, err, "%s", exit.Stderr)
					}
					require.NoError(t, err)
					var parsed string
					require.NoError(t, json.Unmarshal(output, &parsed), "%s", output)
					assert.Equal(t, value, parsed)
				})
			}
		})
	}
}
