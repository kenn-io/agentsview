package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frontmatterField extracts a "key: value" field from a YAML frontmatter
// block. It's a minimal stand-in for a full YAML parse, sufficient to prove
// the rendered frontmatter is well-formed and names the skill.
func frontmatterField(t *testing.T, frontmatter, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(frontmatter, "\n") {
		field, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(field) == key {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("frontmatter field %q not found in:\n%s", key, frontmatter)
	return ""
}

func TestAllHarnesses(t *testing.T) {
	assert.Equal(t, []Harness{HarnessClaude, HarnessAgents}, AllHarnesses())
}

func TestRenderProducesParseableFrontmatterAndDelegatePhrase(t *testing.T) {
	tests := []struct {
		harness  Harness
		delegate string
	}{
		{HarnessClaude, "Dispatch a search subagent (e.g. the Task/Agent tool)"},
		{
			HarnessAgents,
			"Delegate to a search subagent if your harness supports one; " +
				"otherwise run the bounded probes yourself in order",
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.harness), func(t *testing.T) {
			rendered, err := Render(tt.harness, "1.2.3")
			require.NoError(t, err)

			assert.Equal(t, "agentsview-finding-history", rendered.Name)

			// Frontmatter parsers require the fence as the first bytes, so
			// the generated-by header must sit on line two, inside the fence.
			lines := strings.SplitN(rendered.Content, "\n", 3)
			require.Len(t, lines, 3, "content must have a fence line, header line, and body")
			require.Equal(t, "---", lines[0], "content must start with the frontmatter fence")
			headerLine := lines[1]

			// The header hash must equal the hash of the content minus the
			// header line (the pure template render).
			match := headerPattern.FindStringSubmatch(headerLine)
			require.NotNil(t, match, "second line must match the generated-by format: %q", headerLine)
			assert.Equal(t, rendered.Hash, match[1])
			sum := sha256.Sum256([]byte("---\n" + lines[2]))
			assert.Equal(t, hex.EncodeToString(sum[:]), rendered.Hash)
			assert.Contains(t, headerLine, "1.2.3")

			// The frontmatter must be well-formed YAML naming the skill; the
			// header is a YAML comment frontmatterField skips over.
			parts := strings.SplitN(rendered.Content, "---", 3)
			require.Len(t, parts, 3, "content must have exactly two frontmatter fences")
			assert.Equal(t, "agentsview-finding-history", frontmatterField(t, parts[1], "name"))
			assert.NotEmpty(t, frontmatterField(t, parts[1], "description"))

			assert.Contains(t, rendered.Content, tt.delegate)
		})
	}
}

func TestRenderUnknownHarness(t *testing.T) {
	_, err := Render(Harness("bogus"), "1.2.3")
	require.Error(t, err)
}

func TestTargetDir(t *testing.T) {
	tests := []struct {
		harness Harness
		base    string
		want    string
	}{
		{HarnessClaude, filepath.Join("home", "user"),
			filepath.Join("home", "user", ".claude", "skills", "agentsview-finding-history")},
		{HarnessAgents, filepath.Join("home", "user"),
			filepath.Join("home", "user", ".agents", "skills", "agentsview-finding-history")},
		{HarnessClaude, "repo",
			filepath.Join("repo", ".claude", "skills", "agentsview-finding-history")},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.harness, tt.base), func(t *testing.T) {
			assert.Equal(t, tt.want, TargetDir(tt.harness, tt.base))
		})
	}
}

func TestClassify(t *testing.T) {
	fresh, err := Render(HarnessClaude, "1.2.3")
	require.NoError(t, err)

	oldBody := "---\nname: agentsview-finding-history\n---\n\nAn earlier revision of the skill body.\n"
	oldHash := bodyHash(oldBody)
	oldContent := "---\n" + fmt.Sprintf(headerFormat, "1.0.0", oldHash) + "\n" +
		strings.TrimPrefix(oldBody, "---\n")

	tamperedContent := fresh.Content + "\nan uninvited edit\n"

	// The pre-release rendered shape put the header above the fence; those
	// files no longer classify as generated.
	header, rest, _ := strings.Cut(strings.TrimPrefix(fresh.Content, "---\n"), "\n")
	headerFirstContent := header + "\n---\n" + rest

	tests := []struct {
		name     string
		existing []byte
		want     InstalledState
	}{
		{"missing file", nil, StateMissing},
		{"current install", []byte(fresh.Content), StateCurrent},
		{"stale but unmodified install", []byte(oldContent), StateStale},
		{"modified install", []byte(tamperedContent), StateModified},
		{"foreign file with no header", []byte("# Just a regular file\n\nsome text\n"), StateForeign},
		{"fenced file without a generated-by header", []byte("---\nname: x\n---\nbody\n"), StateForeign},
		{"header above the fence", []byte(headerFirstContent), StateForeign},
		{"empty file", []byte(""), StateForeign},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.existing, fresh))
		})
	}
}
