package skills

import (
	"bytes"
	"fmt"
)

// MCPGuide renders the shipped history workflow for this server's own tools.
// It reads only the embedded template, without installation or archive access.
func MCPGuide() (string, error) {
	var out bytes.Buffer
	if err := tmpl.ExecuteTemplate(&out, "finding-history.md.tmpl", templateData{DirectMCP: true}); err != nil {
		return "", fmt.Errorf("skills: render MCP guide: %w", err)
	}
	out.WriteString(mcpDifferences)
	return out.String(), nil
}

const mcpDifferences = `
## MCP differences

The server selects the archive at startup. CLI --server, --server-token-file,
--pg, --json, and --format are not MCP tool arguments. Guide calls read bundled
text only; they do not refresh sources, start sync, or execute this workflow.
Existing server startup and authentication requirements still apply.

| CLI command | MCP tool | Difference |
| --- | --- | --- |
| session search --hybrid | search_content | Supply pattern and mode hybrid; retain the inclusion flags and scope all from the workflow. |
| session search --in tool_input,tool_result | search_content | No exact field-selector equivalent. Use pattern and mode substring, inspect returned location values, and disclose that the bounded search covers broader fields. |
| session messages | get_messages | Use session_id, around/before/after or pagination, and the roles array. |

Use the registered tool's leaf name; client-specific prefixes are supplied by
the host. Do not invent a prefix. search_content has no ` + "`in`, `role`, or `roles` selector" + `.
For dialogue reads, get_messages accepts ` + "`roles: [\"user\", \"assistant\"]`" + `,
not the CLI --role flag. CLI --exclude-session maps to current_session_id.
MCP scope supports substring, regex, terms, semantic, and hybrid modes;
preserve ` + "`scope: all`" + ` when falling back to substring.

Hits in ` + "`thinking`, `tool_input`, and `tool_result`" + ` cannot be opened through ` + "`get_messages`" + `.
That tool returns message text and has_tool_use, not separate thinking or tool
payloads. Adjacent dialogue does not verify a matching payload. Report the
location and unread portion as incomplete evidence; corroborate user decisions
in direct conversation. Full-profile get_tool_calls reports masked/truncated
inputs and result sizes, not unrestricted payloads.

Pass a returned transcript_revision as ` + "`expected_revision`" + ` to get_messages.
Consume each returned ` + "`body_cursor`" + ` before advancing ` + "`next_from`" + `.
For a body continuation, retain session_id but omit around, before, after,
from, direction, and roles. Restart the source lookup on ` + "`source_changed`" + `.
If a truncated body has no usable cursor, increase max_chars_per_message
within its limit or disclose the unread remainder. A partial body is not a
complete source read.

Both profiles expose ` + "`agentsview.skills`" + `, ` + "`get_memory_status`" + `,
` + "`search_content`" + `, and ` + "`get_messages`" + `. The full profile also exposes
additional history/report tools, some depending on backend capabilities.
The memory profile does not expose search_docs or get_tool_calls. No tool
loader is needed. Call agentsview.skills with no name to list this fixed
catalog; supply the catalog name to read its authored guide.
`
