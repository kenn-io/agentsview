package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var formatSourceHeadingRE = regexp.MustCompile(
	"(?m)^## [^\\n]+ \\(`([a-z0-9-]+)`\\)$",
)

var formatSourceEvidenceRE = regexp.MustCompile(
	"(?m)^- \\*\\*Evidence:\\*\\* ((?:`[a-z0-9-]+`, )*`[a-z0-9-]+`)\\.$",
)

var (
	formatSourceEvidenceClassRE = regexp.MustCompile("`([^`]+)`")
	formatSourceCloneRE         = regexp.MustCompile("Clone `https://[^`]+\\.git`")
	formatSourceRevisionRE      = regexp.MustCompile("`([0-9a-f]{40})`")
	formatSourceCheckDateRE     = regexp.MustCompile(
		`\b(?:checked|searched)\s+20\d{2}-\d{2}-\d{2}\b`,
	)
)

func TestSessionFormatSourcesCoverRegistry(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve format inventory test path")

	inventoryPath := filepath.Join(
		filepath.Dir(testFile), "..", "..", "docs", "internal",
		"session-format-sources.md",
	)
	raw, err := os.ReadFile(inventoryPath)
	require.NoError(t, err)
	assert.Empty(t, validateFormatSourceInventory(raw))

	documented := make(map[AgentType]bool)
	for _, match := range formatSourceHeadingRE.FindAllSubmatch(raw, -1) {
		agent := AgentType(match[1])
		assert.Falsef(t, documented[agent],
			"provider %q documented more than once", agent)
		documented[agent] = true
	}

	expected := make(map[AgentType]bool, len(Registry)-1)
	for _, def := range Registry {
		if def.Type == AgentGrok {
			continue
		}
		expected[def.Type] = true
	}

	for agent := range documented {
		assert.Truef(t, expected[agent],
			"inventory documents unknown or excluded provider %q", agent)
	}
	for agent := range expected {
		assert.Truef(t, documented[agent],
			"provider %q missing from format inventory", agent)
	}
}

func TestSessionFormatSourcesRejectIncompleteSection(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** JSONL.
- **Evidence:** ` + "`documentation`" + `.
- **Upstream:** Vendor docs were checked 2026-07-19.
- **Agentsview:** internal/parser/example.go.
`)

	errs := validateFormatSourceInventory(raw)
	assert.Contains(t, errs, `provider "example" missing "Usage and cost" field`)
}

func TestSessionFormatSourcesAcceptGeneratedSchemaEvidence(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** SQLite.
- **Evidence:** ` + "`documentation`, `source`, `generated-schema`" + `.
- **Upstream:** Documentation was checked 2026-07-19. Clone ` + "`https://github.com/example/tool.git`" + `
  at
  ` + "`0123456789abcdef0123456789abcdef01234567`" + `; see the pinned
  [schema](https://github.com/example/tool/blob/0123456789abcdef0123456789abcdef01234567/schema.py).
- **Regeneration:** Run the pinned seeder against a temporary database.
- **Usage and cost:** No usage is persisted.
- **Agentsview:** internal/parser/example.go.
`)

	assert.Empty(t, validateFormatSourceInventory(raw))
}

func TestSessionFormatSourcesRejectInvalidEvidenceClass(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** JSONL.
- **Evidence:** ` + "`inferred`" + `.
- **Upstream:** Vendor material was checked 2026-07-19.
- **Usage and cost:** No usage is persisted.
- **Agentsview:** internal/parser/example.go.
`)

	errs := validateFormatSourceInventory(raw)
	assert.Contains(t, errs, `provider "example" has invalid evidence class "inferred"`)
}

func TestSessionFormatSourcesRejectUnknownCompositeEvidenceClass(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** SQLite.
- **Evidence:** ` + "`documentation`, `source`, `inferred`" + `.
- **Upstream:** Vendor material was checked 2026-07-19.
- **Usage and cost:** No usage is persisted.
- **Agentsview:** internal/parser/example.go.
`)

	errs := validateFormatSourceInventory(raw)
	assert.Contains(t, errs, `provider "example" has invalid evidence class "inferred"`)
}

func TestSessionFormatSourcesRejectGeneratedSchemaWithoutProvenance(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** SQLite.
- **Evidence:** ` + "`generated-schema`" + `.
- **Upstream:** A database was generated.
- **Usage and cost:** No usage is persisted.
- **Agentsview:** internal/parser/example.go.
`)

	errs := validateFormatSourceInventory(raw)
	assert.Contains(t, errs,
		`provider "example" has invalid evidence class combination "generated-schema"`)
}

func TestSessionFormatSourcesRequireCompositeEvidenceMetadata(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name     string
		upstream string
		want     string
	}{
		{
			name: "source revision",
			upstream: "Documentation was checked 2026-07-19. Clone " +
				"`https://github.com/example/tool.git`.",
			want: `provider "example" source evidence lacks a full revision`,
		},
		{
			name: "source clone URL",
			upstream: "Documentation was checked 2026-07-19. Revision `" +
				revision + "`; see the pinned [schema](https://github.com/" +
				"example/tool/blob/" + revision + "/schema.py).",
			want: `provider "example" source evidence lacks an HTTPS clone URL`,
		},
		{
			name: "source pinned file link",
			upstream: "Documentation was checked 2026-07-19. Clone " +
				"`https://github.com/example/tool.git` at `" + revision + "`.",
			want: `provider "example" source evidence lacks a pinned file link`,
		},
		{
			name: "documentation check date",
			upstream: "Clone `https://github.com/example/tool.git` at `" +
				revision + "`; see the pinned [schema](https://github.com/" +
				"example/tool/blob/" + revision + "/schema.py).",
			want: `provider "example" documentation evidence lacks a check date`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte("## Example (`example`)\n\n" +
				"- **Format:** SQLite.\n" +
				"- **Evidence:** `documentation`, `source`, `generated-schema`.\n" +
				"- **Upstream:** " + tt.upstream + "\n" +
				"- **Regeneration:** Run the pinned seeder.\n" +
				"- **Usage and cost:** No usage is persisted.\n" +
				"- **Agentsview:** internal/parser/example.go.\n")

			errs := validateFormatSourceInventory(raw)
			assert.Contains(t, errs, tt.want)
		})
	}
}

func TestSessionFormatSourcesRejectMalformedEvidenceDeclaration(t *testing.T) {
	raw := []byte(`## Example (` + "`example`" + `)

- **Format:** JSONL.
- **Evidence:** documentation.
- **Upstream:** Vendor material was checked 2026-07-19.
- **Usage and cost:** No usage is persisted.
- **Agentsview:** internal/parser/example.go.
`)

	errs := validateFormatSourceInventory(raw)
	assert.Contains(t, errs, `provider "example" has malformed evidence declaration`)
}

func TestSessionFormatSourcesRequireEvidenceMetadata(t *testing.T) {
	tests := []struct {
		name     string
		evidence string
		upstream string
		want     string
	}{
		{
			name:     "source revision",
			evidence: "source",
			upstream: "Clone `https://github.com/example/tool.git`.",
			want:     `provider "example" source evidence lacks a full revision`,
		},
		{
			name:     "source clone URL",
			evidence: "source",
			upstream: "Revision `0123456789abcdef0123456789abcdef01234567`; " +
				"see https://github.com/example/tool/blob/" +
				"0123456789abcdef0123456789abcdef01234567/session.go.",
			want: `provider "example" source evidence lacks an HTTPS clone URL`,
		},
		{
			name:     "source pinned file link",
			evidence: "source",
			upstream: "Clone `https://github.com/example/tool.git` at " +
				"`0123456789abcdef0123456789abcdef01234567`.",
			want: `provider "example" source evidence lacks a pinned file link`,
		},
		{
			name:     "documentation check date",
			evidence: "documentation",
			upstream: "Vendor documentation was checked recently.",
			want:     `provider "example" documentation evidence lacks a check date`,
		},
		{
			name:     "negative search date",
			evidence: "no-public-source",
			upstream: "Vendor documentation and repositories were searched.",
			want:     `provider "example" no-public-source evidence lacks a check date`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte("## Example (`example`)\n\n" +
				"- **Format:** JSONL.\n" +
				"- **Evidence:** `" + tt.evidence + "`.\n" +
				"- **Upstream:** " + tt.upstream + "\n" +
				"- **Usage and cost:** No usage is persisted.\n" +
				"- **Agentsview:** internal/parser/example.go.\n")

			errs := validateFormatSourceInventory(raw)
			assert.Contains(t, errs, tt.want)
		})
	}
}

func TestSessionFormatSourcesRequireFieldsOnceInOrder(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate field",
			raw: "- **Format:** JSONL.\n" +
				"- **Format:** SQLite.\n" +
				"- **Evidence:** `documentation`.\n" +
				"- **Upstream:** Vendor docs were checked 2026-07-19.\n" +
				"- **Usage and cost:** No usage is persisted.\n" +
				"- **Agentsview:** internal/parser/example.go.\n",
			want: `provider "example" has 2 "Format" fields`,
		},
		{
			name: "misordered field",
			raw: "- **Format:** JSONL.\n" +
				"- **Evidence:** `documentation`.\n" +
				"- **Upstream:** Vendor docs were checked 2026-07-19.\n" +
				"- **Agentsview:** internal/parser/example.go.\n" +
				"- **Usage and cost:** No usage is persisted.\n",
			want: `provider "example" fields are not in required order`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateFormatSourceInventory([]byte(
				"## Example (`example`)\n\n" + tt.raw,
			))
			assert.Contains(t, errs, tt.want)
		})
	}
}

func validateFormatSourceInventory(raw []byte) []string {
	matches := formatSourceHeadingRE.FindAllSubmatchIndex(raw, -1)
	var errs []string
	for i, match := range matches {
		sectionEnd := len(raw)
		if i+1 < len(matches) {
			sectionEnd = matches[i+1][0]
		}
		agent := string(raw[match[2]:match[3]])
		section := string(raw[match[1]:sectionEnd])
		fields := []string{
			"Format", "Evidence", "Upstream", "Usage and cost", "Agentsview",
		}
		lastFieldIndex := -1
		fieldsInOrder := true
		for _, field := range fields {
			marker := "- **" + field + ":**"
			fieldCount := strings.Count(section, marker)
			if fieldCount == 0 {
				errs = append(errs, fmt.Sprintf(
					"provider %q missing %q field", agent, field,
				))
				continue
			}
			if fieldCount > 1 {
				errs = append(errs, fmt.Sprintf(
					"provider %q has %d %q fields", agent, fieldCount, field,
				))
			}
			fieldIndex := strings.Index(section, marker)
			if fieldIndex <= lastFieldIndex {
				fieldsInOrder = false
			}
			lastFieldIndex = fieldIndex
		}
		if !fieldsInOrder {
			errs = append(errs, fmt.Sprintf(
				"provider %q fields are not in required order", agent,
			))
		}

		evidenceMatch := formatSourceEvidenceRE.FindStringSubmatch(section)
		if evidenceMatch == nil {
			errs = append(errs, fmt.Sprintf(
				"provider %q has malformed evidence declaration", agent,
			))
			continue
		}
		evidenceMatches := formatSourceEvidenceClassRE.FindAllStringSubmatch(
			evidenceMatch[1], -1,
		)
		evidenceClasses := make([]string, 0, len(evidenceMatches))
		hasInvalidClass := false
		for _, evidenceMatch := range evidenceMatches {
			evidence := evidenceMatch[1]
			evidenceClasses = append(evidenceClasses, evidence)
			if evidence != "source" && evidence != "documentation" &&
				evidence != "no-public-source" &&
				evidence != "generated-schema" {
				errs = append(errs, fmt.Sprintf(
					"provider %q has invalid evidence class %q", agent, evidence,
				))
				hasInvalidClass = true
			}
		}
		if hasInvalidClass {
			continue
		}
		composite := false
		if len(evidenceClasses) > 1 {
			if len(evidenceClasses) == 3 &&
				evidenceClasses[0] == "documentation" &&
				evidenceClasses[1] == "source" &&
				evidenceClasses[2] == "generated-schema" {
				composite = true
			} else {
				errs = append(errs, fmt.Sprintf(
					"provider %q has invalid evidence class combination %q",
					agent, strings.Join(evidenceClasses, ", "),
				))
				continue
			}
		}

		evidence := evidenceClasses[0]
		if evidence == "generated-schema" {
			errs = append(errs, fmt.Sprintf(
				"provider %q has invalid evidence class combination %q",
				agent, evidence,
			))
			continue
		}
		hasSource := evidence == "source" || composite
		hasDocumentation := evidence == "documentation" || composite
		if hasSource {
			revisionMatch := formatSourceRevisionRE.FindStringSubmatch(section)
			if revisionMatch == nil {
				errs = append(errs, fmt.Sprintf(
					"provider %q source evidence lacks a full revision", agent,
				))
			} else {
				if !formatSourceCloneRE.MatchString(section) {
					errs = append(errs, fmt.Sprintf(
						"provider %q source evidence lacks an HTTPS clone URL", agent,
					))
				}
				if !strings.Contains(section, "/blob/"+revisionMatch[1]+"/") {
					errs = append(errs, fmt.Sprintf(
						"provider %q source evidence lacks a pinned file link", agent,
					))
				}
			}
		}
		if hasDocumentation {
			if !formatSourceCheckDateRE.MatchString(section) {
				errs = append(errs, fmt.Sprintf(
					"provider %q documentation evidence lacks a check date", agent,
				))
			}
		} else if evidence == "no-public-source" &&
			!formatSourceCheckDateRE.MatchString(section) {
			errs = append(errs, fmt.Sprintf(
				"provider %q no-public-source evidence lacks a check date",
				agent,
			))
		}
	}
	return errs
}
